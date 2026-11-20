package users

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The device authorization grant (RFC 8628) and the short-lived device tokens
// it issues to our own apps (#93). A device token is an access token bound to
// the DPoP key that asked for it: a request is admitted only with a token and a
// proof signed by that key. Refresh tokens rotate on every use, and a refresh
// token used a second time revokes its whole grant.
const (
	// DeviceCodeTTL is how long a user code can be approved.
	DeviceCodeTTL = 10 * time.Minute
	// DeviceInterval is the poll interval the app is asked to keep.
	DeviceInterval = 5 * time.Second
	// AccessTokenTTL is the life of an access token.
	AccessTokenTTL = 15 * time.Minute
	// RefreshIdleTTL is how long a refresh token may go unused.
	RefreshIdleTTL = 30 * 24 * time.Hour

	accessPrefix  = "tsa_"
	refreshPrefix = "tsr_"
	devicePrefix  = "tsd_"
	// userCodeAlphabet leaves out characters that are easy to misread.
	userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ23456789"
	userCodeLength   = 8
)

// Token errors. The names are the RFC 8628 and RFC 6749 error codes, so the
// token endpoint can return them as they are.
var (
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
	ErrDeviceCodeExpired    = errors.New("expired_token")
	ErrInvalidGrant         = errors.New("invalid_grant")
	// ErrRefreshReused is ErrInvalidGrant for a refresh token that was already
	// rotated: the grant is revoked as well.
	ErrRefreshReused = errors.New("refresh token reused")
	// ErrTokenExpired and ErrTokenInvalid are answers to an access token.
	ErrTokenExpired = errors.New("access token expired")
	ErrTokenInvalid = errors.New("access token invalid")
)

// DeviceAuth is what the app receives when it starts the grant.
type DeviceAuth struct {
	DeviceCode string
	UserCode   string // as shown to the user, "XXXX-XXXX"
	ExpiresIn  int
	Interval   int
}

// Tokens is what the token endpoint returns.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

// maxLabel bounds the label a device gives itself.
const maxLabel = 64

// CreateDeviceAuth starts a grant for a device called label. The device code is
// kept only as a hash; the user code is short enough to type.
func CreateDeviceAuth(ctx context.Context, db *sql.DB, label string, now time.Time) (DeviceAuth, error) {
	label = truncateRunes(strings.TrimSpace(label), maxLabel)
	deviceCode, err := randomToken(devicePrefix)
	if err != nil {
		return DeviceAuth{}, err
	}
	// Expired grants and tokens go now, so the tables stay small.
	for _, q := range []string{
		"DELETE FROM oauth_device_codes WHERE expires_ms <= ?",
		"DELETE FROM oauth_access_tokens WHERE expires_ms <= ?",
	} {
		if _, err := db.ExecContext(ctx, q, now.UnixMilli()); err != nil {
			return DeviceAuth{}, err
		}
	}
	// A collision of user codes is rare; a few tries make it impossible in practice.
	for range 5 {
		userCode, err := randomUserCode()
		if err != nil {
			return DeviceAuth{}, err
		}
		_, err = db.ExecContext(ctx,
			"INSERT INTO oauth_device_codes (device_hash, user_code, label, expires_ms, interval_ms, created_ms) "+
				"VALUES (?, ?, ?, ?, ?, ?)",
			hashToken(deviceCode), userCode, label,
			now.Add(DeviceCodeTTL).UnixMilli(), DeviceInterval.Milliseconds(), now.UnixMilli())
		if err == nil {
			return DeviceAuth{
				DeviceCode: deviceCode,
				UserCode:   userCode[:4] + "-" + userCode[4:],
				ExpiresIn:  int(DeviceCodeTTL.Seconds()),
				Interval:   int(DeviceInterval.Seconds()),
			}, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			return DeviceAuth{}, err
		}
	}
	return DeviceAuth{}, errors.New("could not find a free user code")
}

// PendingDeviceCodes counts the grants waiting for approval, so the server can
// refuse new ones when there are too many.
func PendingDeviceCodes(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM oauth_device_codes WHERE user_id IS NULL AND expires_ms > ?",
		now.UnixMilli()).Scan(&n)
	return n, err
}

// ApproveUserCode gives a pending grant to userID. The user code is matched
// without its dash and without case. ErrNotFound means no pending grant has it.
func ApproveUserCode(ctx context.Context, db *sql.DB, userID int64, userCode string, now time.Time) error {
	res, err := db.ExecContext(ctx,
		"UPDATE oauth_device_codes SET user_id = ? "+
			"WHERE user_code = ? AND user_id IS NULL AND expires_ms > ?",
		userID, normalizeUserCode(userCode), now.UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeviceCodeKnown reports whether a device code is one the server issued and
// has not used up or expired, without touching it. The server checks this
// before it records a proof, so unknown codes cannot fill the replay store.
func DeviceCodeKnown(ctx context.Context, db *sql.DB, deviceCode string, now time.Time) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM oauth_device_codes WHERE device_hash = ? AND expires_ms > ?",
		hashToken(deviceCode), now.UnixMilli()).Scan(&n)
	return n > 0, err
}

// ExchangeDeviceCode is the token endpoint for the device grant. It returns
// ErrAuthorizationPending or ErrSlowDown until the user approves, then issues
// tokens bound to jkt and consumes the device code.
func ExchangeDeviceCode(ctx context.Context, db *sql.DB, deviceCode, jkt string, now time.Time) (Tokens, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var userID sql.NullInt64
	var label string
	var expires, interval, lastPoll int64
	err = tx.QueryRowContext(ctx,
		"SELECT user_id, label, expires_ms, interval_ms, last_poll_ms FROM oauth_device_codes WHERE device_hash = ?",
		hashToken(deviceCode)).Scan(&userID, &label, &expires, &interval, &lastPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, ErrInvalidGrant
	}
	if err != nil {
		return Tokens{}, err
	}
	nowMs := now.UnixMilli()
	if nowMs >= expires {
		if _, err := tx.ExecContext(ctx, "DELETE FROM oauth_device_codes WHERE device_hash = ?", hashToken(deviceCode)); err != nil {
			return Tokens{}, err
		}
		return Tokens{}, commitThen(tx, ErrDeviceCodeExpired)
	}
	if lastPoll > 0 && nowMs-lastPoll < interval {
		// Polling faster than asked: back off, as RFC 8628 section 3.5 says.
		if _, err := tx.ExecContext(ctx,
			"UPDATE oauth_device_codes SET last_poll_ms = ?, interval_ms = interval_ms + ? WHERE device_hash = ?",
			nowMs, DeviceInterval.Milliseconds(), hashToken(deviceCode)); err != nil {
			return Tokens{}, err
		}
		return Tokens{}, commitThen(tx, ErrSlowDown)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE oauth_device_codes SET last_poll_ms = ? WHERE device_hash = ?", nowMs, hashToken(deviceCode)); err != nil {
		return Tokens{}, err
	}
	if !userID.Valid {
		return Tokens{}, commitThen(tx, ErrAuthorizationPending)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM oauth_device_codes WHERE device_hash = ?", hashToken(deviceCode)); err != nil {
		return Tokens{}, err
	}
	if err := requireActiveUser(ctx, tx, userID.Int64); err != nil {
		return Tokens{}, commitThen(tx, ErrInvalidGrant)
	}
	toks, err := mintTokens(ctx, tx, userID.Int64, label, jkt, now)
	if err != nil {
		return Tokens{}, err
	}
	return toks, tx.Commit()
}

// RefreshTokens rotates a refresh token: the presented one stops working and a
// new refresh token and access token are issued. A refresh token that was
// rotated before revokes its grant; the answer is ErrRefreshReused.
func RefreshTokens(ctx context.Context, db *sql.DB, refresh, jkt string, now time.Time) (Tokens, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, err
	}
	defer func() { _ = tx.Rollback() }()

	h := hashToken(refresh)
	var id, userID, lastUsed int64
	var grantJKT string
	var revoked sql.NullInt64
	err = tx.QueryRowContext(ctx,
		"SELECT id, user_id, jkt, revoked_ms, last_used_ms FROM oauth_grants WHERE refresh_hash = ?", h).
		Scan(&id, &userID, &grantJKT, &revoked, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, reuseOrInvalid(ctx, tx, h, now)
	}
	if err != nil {
		return Tokens{}, err
	}
	nowMs := now.UnixMilli()
	switch {
	case revoked.Valid || grantJKT != jkt:
		return Tokens{}, commitThen(tx, ErrInvalidGrant)
	case nowMs-lastUsed > RefreshIdleTTL.Milliseconds():
		if err := revokeGrant(ctx, tx, id, nowMs); err != nil {
			return Tokens{}, err
		}
		return Tokens{}, commitThen(tx, ErrInvalidGrant)
	}
	if err := requireActiveUser(ctx, tx, userID); err != nil {
		return Tokens{}, commitThen(tx, ErrInvalidGrant)
	}

	next, err := randomToken(refreshPrefix)
	if err != nil {
		return Tokens{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO oauth_refresh_used (refresh_hash, grant_id, used_ms) VALUES (?, ?, ?)", h, id, nowMs); err != nil {
		return Tokens{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE oauth_grants SET refresh_hash = ?, last_used_ms = ? WHERE id = ?", hashToken(next), nowMs, id); err != nil {
		return Tokens{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM oauth_refresh_used WHERE used_ms < ?", nowMs-RefreshIdleTTL.Milliseconds()); err != nil {
		return Tokens{}, err
	}
	access, err := newAccessToken(ctx, tx, id, now)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, RefreshToken: next, ExpiresIn: int(AccessTokenTTL.Seconds())}, nil
}

// LookupAccessToken resolves an access token presented with a proof from jkt.
// It is ErrTokenExpired when the token is past its life, and ErrTokenInvalid
// for an unknown token, a token of another key, a revoked grant or a disabled
// account.
func LookupAccessToken(ctx context.Context, db *sql.DB, access, jkt string, now time.Time) (User, error) {
	var expires, userID int64
	var grantJKT string
	var revoked sql.NullInt64
	err := db.QueryRowContext(ctx,
		"SELECT t.expires_ms, g.user_id, g.jkt, g.revoked_ms FROM oauth_access_tokens t "+
			"JOIN oauth_grants g ON g.id = t.grant_id WHERE t.token_hash = ?", hashToken(access)).
		Scan(&expires, &userID, &grantJKT, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrTokenInvalid
	}
	if err != nil {
		return User{}, err
	}
	if revoked.Valid || grantJKT != jkt {
		return User{}, ErrTokenInvalid
	}
	if now.UnixMilli() >= expires {
		return User{}, ErrTokenExpired
	}
	u, err := ByID(ctx, db, userID)
	if err != nil {
		return User{}, ErrTokenInvalid
	}
	if u.Status != StatusActive {
		return User{}, ErrTokenInvalid
	}
	return u, nil
}

// GrantKnown reports whether a refresh token belongs to a grant, now or
// rotated away. Like DeviceCodeKnown, it guards the replay store.
func GrantKnown(ctx context.Context, db *sql.DB, refresh string) (bool, error) {
	h := hashToken(refresh)
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT (SELECT COUNT(*) FROM oauth_grants WHERE refresh_hash = ?) + "+
			"(SELECT COUNT(*) FROM oauth_refresh_used WHERE refresh_hash = ?)", h, h).Scan(&n)
	return n > 0, err
}

// RevokeUserGrants ends every device grant of userID. Its access tokens stop at
// once, and its refresh tokens can no longer be used.
func RevokeUserGrants(ctx context.Context, db *sql.DB, userID int64, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx,
		"UPDATE oauth_grants SET revoked_ms = ? WHERE user_id = ? AND revoked_ms IS NULL",
		now.UnixMilli(), userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// reuseOrInvalid answers an unknown refresh token. One that was rotated away
// revokes its grant, because the same token presented twice means it leaked.
func reuseOrInvalid(ctx context.Context, tx *sql.Tx, h string, now time.Time) error {
	var grantID int64
	err := tx.QueryRowContext(ctx, "SELECT grant_id FROM oauth_refresh_used WHERE refresh_hash = ?", h).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return commitThen(tx, ErrInvalidGrant)
	}
	if err != nil {
		return err
	}
	if err := revokeGrant(ctx, tx, grantID, now.UnixMilli()); err != nil {
		return err
	}
	return commitThen(tx, ErrRefreshReused)
}

func revokeGrant(ctx context.Context, tx *sql.Tx, id, nowMs int64) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE oauth_grants SET revoked_ms = ? WHERE id = ? AND revoked_ms IS NULL", nowMs, id)
	return err
}

// mintTokens creates a grant with its first refresh token and an access token.
func mintTokens(ctx context.Context, tx *sql.Tx, userID int64, label, jkt string, now time.Time) (Tokens, error) {
	refresh, err := randomToken(refreshPrefix)
	if err != nil {
		return Tokens{}, err
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO oauth_grants (user_id, label, jkt, refresh_hash, created_ms, last_used_ms) VALUES (?, ?, ?, ?, ?, ?)",
		userID, label, jkt, hashToken(refresh), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Tokens{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Tokens{}, err
	}
	access, err := newAccessToken(ctx, tx, id, now)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, RefreshToken: refresh, ExpiresIn: int(AccessTokenTTL.Seconds())}, nil
}

func newAccessToken(ctx context.Context, tx *sql.Tx, grantID int64, now time.Time) (string, error) {
	access, err := randomToken(accessPrefix)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO oauth_access_tokens (token_hash, grant_id, expires_ms) VALUES (?, ?, ?)",
		hashToken(access), grantID, now.Add(AccessTokenTTL).UnixMilli()); err != nil {
		return "", err
	}
	return access, nil
}

func requireActiveUser(ctx context.Context, tx *sql.Tx, userID int64) error {
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM users WHERE id = ?", userID).Scan(&status); err != nil {
		return err
	}
	if status != StatusActive {
		return ErrNotFound
	}
	return nil
}

// commitThen commits what was done so far (a revocation, a poll) and returns err.
func commitThen(tx *sql.Tx, err error) error {
	if cerr := tx.Commit(); cerr != nil {
		return cerr
	}
	return err
}

// randomBytes returns n bytes from the system's random source.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// randomUserCode draws userCodeLength characters from userCodeAlphabet.
func randomUserCode() (string, error) {
	var b strings.Builder
	buf, err := randomBytes(userCodeLength)
	if err != nil {
		return "", err
	}
	for _, c := range buf {
		b.WriteByte(userCodeAlphabet[int(c)%len(userCodeAlphabet)])
	}
	return b.String(), nil
}

// normalizeUserCode drops the dash and spaces a person may type and folds case.
func normalizeUserCode(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
