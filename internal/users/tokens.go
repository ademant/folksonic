package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	apiKeyPrefix   = "tsk_"
	sessionPrefix  = "tss_"
	pairingPrefix  = "tsp_"
	PairingTTL     = 15 * time.Minute
	sessionTTL     = 30 * 24 * time.Hour
	apiKeyDisplayN = 12
)

// APIKey is a stored key. The plaintext is only returned once, at creation.
type APIKey struct {
	ID         int64
	Label      string
	Prefix     string
	CreatedAt  string
	ExpiresAt  sql.NullString
	LastUsedAt sql.NullString
	RevokedAt  sql.NullString
	// Scope is "" for a full key. A key with a scope (the analysis worker
	// keys removed in #105) is refused everywhere.
	Scope string
}

// KeyTTL is the lifetime of a new key when the caller gives none, and KeyIdle
// is how long a key may go unused before it is revoked. The server sets both
// from its config at start; every key expires.
var (
	KeyTTL  = 365 * 24 * time.Hour
	KeyIdle = 180 * 24 * time.Hour
)

// keyRenewWindow: a key with less than this left is extended to a full KeyTTL
// (or to its own lifetime, when it was given one) the next time it is used.
const keyRenewWindow = 30 * 24 * time.Hour

// CreateAPIKey issues a new full key for userID and returns its plaintext. A
// zero ttl means KeyTTL.
func CreateAPIKey(ctx context.Context, q querier, userID int64, label string, ttl time.Duration) (string, error) {
	return CreateScopedAPIKey(ctx, q, userID, label, "", ttl)
}

// CreateScopedAPIKey issues a key limited to scope ("" for a full key). A zero
// ttl means KeyTTL: every key expires. The lifetime is kept with the key, so
// renewal extends it by the same amount.
func CreateScopedAPIKey(ctx context.Context, q querier, userID int64, label, scope string, ttl time.Duration) (string, error) {
	plain, err := randomToken(apiKeyPrefix)
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = KeyTTL
	}
	expires := ts(time.Now().Add(ttl))
	if label == "" {
		label = "api key"
	}
	_, err = q.Exec(
		`INSERT INTO api_keys (user_id, label, prefix, key_hash, created_at, expires_at, scope, ttl_seconds)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, label, plain[:apiKeyDisplayN], hashToken(plain), Now(), expires, scope, int64(ttl.Seconds()))
	if err != nil {
		return "", err
	}
	return plain, nil
}

// KeyInfo describes the key that authenticated a request.
type KeyInfo struct {
	ID    int64
	Scope string
}

// LookupAPIKey returns the active user behind a presented key. Revoked,
// expired and disabled-user keys are rejected with ErrNotFound. The scope is
// not checked here; see LookupAPIKeyInfo.
func LookupAPIKey(ctx context.Context, db *sql.DB, plain string) (User, error) {
	u, _, err := LookupAPIKeyInfo(ctx, db, plain)
	return u, err
}

// LookupAPIKeyInfo is LookupAPIKey that also returns the key's id and scope.
func LookupAPIKeyInfo(ctx context.Context, db *sql.DB, plain string) (User, KeyInfo, error) {
	var info KeyInfo
	// scanUser handles passwordless accounts, whose password_hash is NULL.
	row := db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.password_hash, u.role, u.status, u.created_at, u.webauthn_handle, k.id, k.scope,
		       k.ttl_seconds, k.expires_at
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ? AND k.revoked_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > ?)
		  AND u.status = ?`,
		hashToken(plain), Now(), StatusActive,
	)
	var u User
	var hash sql.NullString
	var ttl sql.NullInt64
	var expires sql.NullString
	err := row.Scan(&u.ID, &u.Username, &hash, &u.Role, &u.Status, &u.CreatedAt, &u.WebAuthnHandle, &info.ID, &info.Scope, &ttl, &expires)
	u.PasswordHash = hash.String
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, KeyInfo{}, ErrNotFound
	}
	if err != nil {
		return User{}, KeyInfo{}, err
	}
	// Best effort: a failed write must not reject a valid key.
	now := time.Now()
	_, _ = db.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE key_hash = ?", ts(now), hashToken(plain))
	// Renewal: a key in use that has less than keyRenewWindow left gets its
	// full lifetime again. Keys that are not near the end are not written.
	if ttl.Valid && expires.Valid && ttl.Int64 > 0 {
		_, _ = db.ExecContext(ctx,
			"UPDATE api_keys SET expires_at = ? WHERE key_hash = ? AND expires_at < ?",
			ts(now.Add(time.Duration(ttl.Int64)*time.Second)), hashToken(plain), ts(now.Add(keyRenewWindow)))
	}
	return u, info, nil
}

func ListAPIKeys(ctx context.Context, db *sql.DB, userID int64) ([]APIKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, label, prefix, created_at, expires_at, last_used_at, revoked_at, scope
		FROM api_keys WHERE user_id = ? AND revoked_at IS NULL ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Label, &k.Prefix, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt, &k.Scope); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey revokes one of userID's keys. Keys belonging to other users
// report ErrNotFound, so callers cannot probe for other accounts' key IDs.
func RevokeAPIKey(ctx context.Context, db *sql.DB, userID, keyID int64) error {
	res, err := db.ExecContext(ctx,
		"UPDATE api_keys SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL",
		Now(), keyID, userID)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// CreatePairingToken issues a single-use token a device can redeem for an
// API key. It expires after PairingTTL.
func CreatePairingToken(ctx context.Context, db *sql.DB, userID int64) (string, error) {
	plain, err := randomToken(pairingPrefix)
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO pairing_tokens (token_hash, user_id, expires_at, scope) VALUES (?, ?, ?, ?)",
		hashToken(plain), userID, ts(time.Now().Add(PairingTTL)), "")
	if err != nil {
		return "", err
	}
	return plain, nil
}

// ErrPairingExpired is returned for a pairing token past its time limit. It
// matches ErrNotFound, so callers that do not care about the reason need not
// change.
var ErrPairingExpired = fmt.Errorf("%w: pairing token expired", ErrNotFound)

// RedeemPairingToken consumes a pairing token and issues an API key for the
// device. The token is marked used in the same transaction, so a concurrent
// second redemption fails.
func RedeemPairingToken(ctx context.Context, db *sql.DB, plain, deviceLabel string) (string, User, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", User{}, err
	}
	defer tx.Rollback()

	var userID int64
	var expires, scope string
	var used sql.NullString
	err = tx.QueryRowContext(ctx,
		"SELECT user_id, expires_at, used_at, scope FROM pairing_tokens WHERE token_hash = ?",
		hashToken(plain)).Scan(&userID, &expires, &used, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return "", User{}, ErrNotFound
	}
	if err != nil {
		return "", User{}, err
	}
	// A token with a scope was for an analysis worker (removed in #105).
	if used.Valid || scope != "" {
		return "", User{}, ErrNotFound
	}
	if expires <= Now() {
		return "", User{}, ErrPairingExpired
	}
	// A QR code a device has already scanned is for that verified pairing
	// (#103): it must not also hand out an API key that needs no confirmation.
	var devicePairings int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM device_pairings WHERE token_hash = ?", hashToken(plain)).Scan(&devicePairings); err != nil {
		return "", User{}, err
	}
	if devicePairings > 0 {
		return "", User{}, ErrPairingTokenUsed
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE pairing_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL",
		Now(), hashToken(plain))
	if err != nil {
		return "", User{}, err
	}
	if err := requireOne(res); err != nil {
		return "", User{}, ErrNotFound
	}
	u, err := ByID(ctx, db, userID)
	if err != nil {
		return "", User{}, err
	}
	if u.Status != StatusActive {
		return "", User{}, ErrNotFound
	}
	key, err := CreateAPIKey(ctx, tx, userID, "app: "+deviceLabel, 0)
	if err != nil {
		return "", User{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", User{}, err
	}
	return key, u, nil
}

// CreateSession issues a login token for the web UI or API clients that
// sign in with a passkey.
func CreateSession(ctx context.Context, db *sql.DB, userID int64) (string, error) {
	plain, err := randomToken(sessionPrefix)
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)",
		hashToken(plain), userID, ts(time.Now().Add(sessionTTL)))
	if err != nil {
		return "", err
	}
	return plain, nil
}

func LookupSession(ctx context.Context, db *sql.DB, plain string) (User, error) {
	var userID int64
	var expires string
	err := db.QueryRowContext(ctx,
		"SELECT user_id, expires_at FROM sessions WHERE token_hash = ?", hashToken(plain),
	).Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires <= Now()) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u, err := ByID(ctx, db, userID)
	if err != nil {
		return User{}, err
	}
	if u.Status != StatusActive {
		return User{}, ErrNotFound
	}
	return u, nil
}

func DeleteSession(ctx context.Context, db *sql.DB, plain string) error {
	_, err := db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", hashToken(plain))
	return err
}

// APIKeyProblem says why a presented API key is not valid, for the log: an
// unknown key, or a revoked, expired or disabled one. It is called only after
// LookupAPIKeyInfo has refused the key. For a key that exists, it also returns
// the key's id and label, so an administrator can find it; the key itself is
// never returned.
func APIKeyProblem(ctx context.Context, db *sql.DB, plain string) (reason string, keyID int64, label string) {
	var revoked, expires, why sql.NullString
	var status string
	err := db.QueryRowContext(ctx, `
		SELECT k.id, k.label, k.revoked_at, k.revoked_reason, k.expires_at, u.status
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ?`, hashToken(plain),
	).Scan(&keyID, &label, &revoked, &why, &expires, &status)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return CodeKeyUnknown, 0, ""
	}
	switch {
	case revoked.Valid && why.String == RevokedIdle:
		return CodeKeyIdle, keyID, label
	case revoked.Valid:
		return CodeKeyRevoked, keyID, label
	case expires.Valid && expires.String <= Now():
		return CodeKeyExpired, keyID, label
	case status != StatusActive:
		return CodeUserDisabled, keyID, label
	}
	return CodeKeyUnknown, keyID, label
}

// Error codes for a refused credential. They are stable: clients branch on
// them, and the text in the response only explains them to people.
const (
	CodeKeyUnknown          = "key_unknown"
	CodeKeyRevoked          = "key_revoked"
	CodeKeyIdle             = "key_idle"
	CodeKeyExpired          = "key_expired"
	CodeSessionUnknown      = "session_unknown"
	CodeSessionExpired      = "session_expired"
	CodeUserDisabled        = "user_disabled"
	CodeCredentialsRequired = "credentials_required"
	CodeScopeForbidden      = "scope_forbidden"
	CodeSessionRequired     = "session_required"
	CodePairingExpired      = "pairing_expired"
	CodePairingInvalid      = "pairing_invalid"
	CodeInviteExpired       = "invite_expired"
	CodeInviteInvalid       = "invite_invalid"
	CodeStreamURLExpired    = "stream_url_expired"
	CodeStreamURLInvalid    = "stream_url_invalid"
	CodeProofInvalid        = "proof_invalid"
	CodeProofReplayed       = "proof_replayed"
	CodePairingPending      = "pairing_pending"
	CodeDeviceUnknown       = "device_unknown"
	CodePairingRejected     = "pairing_rejected"
	CodePairingTokenUsed    = "pairing_token_used"
	CodePairingBusy         = "pairing_busy"
	// RevokedIdle is the revoked_reason of a key the idle sweep revoked.
	RevokedIdle = "idle"
)

// SessionProblem says why a presented session token is not valid, for the log:
// an unknown token, an expired one, or one whose account is disabled.
func SessionProblem(ctx context.Context, db *sql.DB, plain string) string {
	var userID int64
	var expires string
	err := db.QueryRowContext(ctx,
		"SELECT user_id, expires_at FROM sessions WHERE token_hash = ?", hashToken(plain),
	).Scan(&userID, &expires)
	if err != nil {
		return CodeSessionUnknown
	}
	if expires <= Now() {
		return CodeSessionExpired
	}
	var status string
	if err := db.QueryRowContext(ctx, "SELECT status FROM users WHERE id = ?", userID).Scan(&status); err != nil || status != StatusActive {
		return CodeUserDisabled
	}
	return CodeSessionUnknown
}

// IdleKey is a key the idle sweep revoked.
type IdleKey struct {
	ID    int64
	Label string
}

// RevokeIdleKeys revokes every active key that has not been used for idle, and
// returns them, so the caller can log each one. A key that was never used is
// measured from its creation.
func RevokeIdleKeys(ctx context.Context, db *sql.DB, idle time.Duration) ([]IdleKey, error) {
	now := time.Now()
	cutoff := ts(now.Add(-idle))
	rows, err := db.QueryContext(ctx, `
		SELECT id, label FROM api_keys
		WHERE revoked_at IS NULL AND COALESCE(last_used_at, created_at) < ?
		ORDER BY id`, cutoff)
	if err != nil {
		return nil, err
	}
	var out []IdleKey
	for rows.Next() {
		var k IdleKey
		if err := rows.Scan(&k.ID, &k.Label); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range out {
		if _, err := db.ExecContext(ctx,
			"UPDATE api_keys SET revoked_at = ?, revoked_reason = ? WHERE id = ? AND revoked_at IS NULL",
			ts(now), RevokedIdle, k.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// BackfillKeyLifetimes gives keys made before lifetimes existed one: each
// active key with no expiry expires its default lifetime after its last use, or
// after its creation if it was never used. It only touches keys that lack a
// lifetime, so running it again changes nothing.
func BackfillKeyLifetimes(ctx context.Context, db *sql.DB, ttl time.Duration) error {
	if _, err := db.ExecContext(ctx,
		"UPDATE api_keys SET ttl_seconds = ? WHERE ttl_seconds IS NULL", int64(ttl.Seconds())); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(last_used_at, created_at) FROM api_keys
		WHERE expires_at IS NULL AND revoked_at IS NULL`)
	if err != nil {
		return err
	}
	type pending struct {
		id   int64
		base string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.base); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range todo {
		base, err := time.Parse("2006-01-02T15:04:05Z", p.base)
		if err != nil {
			return fmt.Errorf("api key %d: bad timestamp %q: %w", p.id, p.base, err)
		}
		if _, err := db.ExecContext(ctx,
			"UPDATE api_keys SET expires_at = ? WHERE id = ?", ts(base.Add(ttl)), p.id); err != nil {
			return err
		}
	}
	return nil
}
