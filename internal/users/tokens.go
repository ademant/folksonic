package users

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	apiKeyPrefix   = "tsk_"
	sessionPrefix  = "tss_"
	pairingPrefix  = "tsp_"
	PairingTTL     = 5 * time.Minute
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
	// Scope is "" for a full key, or ScopeWorker for a key that may only
	// call the analysis worker endpoints.
	Scope string
}

// ScopeWorker marks the key of an analysis worker (see internal/worker). It
// is accepted only in the X-Api-Key header and only on /api/worker/.
const ScopeWorker = "worker"

// CreateAPIKey issues a new full key for userID and returns its plaintext. A
// zero ttl means the key does not expire.
func CreateAPIKey(ctx context.Context, q querier, userID int64, label string, ttl time.Duration) (string, error) {
	return CreateScopedAPIKey(ctx, q, userID, label, "", ttl)
}

// CreateScopedAPIKey issues a key limited to scope ("" for a full key).
func CreateScopedAPIKey(ctx context.Context, q querier, userID int64, label, scope string, ttl time.Duration) (string, error) {
	plain, err := randomToken(apiKeyPrefix)
	if err != nil {
		return "", err
	}
	var expires any
	if ttl > 0 {
		expires = ts(time.Now().Add(ttl))
	}
	if label == "" {
		label = "api key"
	}
	_, err = q.Exec(
		`INSERT INTO api_keys (user_id, label, prefix, key_hash, created_at, expires_at, scope)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, label, plain[:apiKeyDisplayN], hashToken(plain), Now(), expires, scope)
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
		SELECT u.id, u.username, u.password_hash, u.role, u.status, u.created_at, u.webauthn_handle, k.id, k.scope
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ? AND k.revoked_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > ?)
		  AND u.status = ?`,
		hashToken(plain), Now(), StatusActive,
	)
	var u User
	var hash sql.NullString
	err := row.Scan(&u.ID, &u.Username, &hash, &u.Role, &u.Status, &u.CreatedAt, &u.WebAuthnHandle, &info.ID, &info.Scope)
	u.PasswordHash = hash.String
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, KeyInfo{}, ErrNotFound
	}
	if err != nil {
		return User{}, KeyInfo{}, err
	}
	// Best effort: a failed timestamp write must not reject a valid key.
	_, _ = db.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE key_hash = ?", Now(), hashToken(plain))
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
	return CreateScopedPairingToken(ctx, db, userID, "")
}

// CreateScopedPairingToken issues a pairing token whose key has scope.
func CreateScopedPairingToken(ctx context.Context, db *sql.DB, userID int64, scope string) (string, error) {
	plain, err := randomToken(pairingPrefix)
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO pairing_tokens (token_hash, user_id, expires_at, scope) VALUES (?, ?, ?, ?)",
		hashToken(plain), userID, ts(time.Now().Add(PairingTTL)), scope)
	if err != nil {
		return "", err
	}
	return plain, nil
}

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
	if used.Valid || expires <= Now() {
		return "", User{}, ErrNotFound
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
	label := "app: " + deviceLabel
	if scope == ScopeWorker {
		label = "worker: " + deviceLabel
	}
	key, err := CreateScopedAPIKey(ctx, tx, userID, label, scope, 0)
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
	var revoked, expires sql.NullString
	var status string
	err := db.QueryRowContext(ctx, `
		SELECT k.id, k.label, k.revoked_at, k.expires_at, u.status
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ?`, hashToken(plain),
	).Scan(&keyID, &label, &revoked, &expires, &status)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return "unknown-key", 0, ""
	}
	switch {
	case revoked.Valid:
		return "revoked-key", keyID, label
	case expires.Valid && expires.String <= Now():
		return "expired-key", keyID, label
	case status != StatusActive:
		return "disabled-user", keyID, label
	}
	return "invalid-key", keyID, label
}

// SessionProblem says why a presented session token is not valid, for the log:
// an unknown token, an expired one, or one whose account is disabled.
func SessionProblem(ctx context.Context, db *sql.DB, plain string) string {
	var userID int64
	var expires string
	err := db.QueryRowContext(ctx,
		"SELECT user_id, expires_at FROM sessions WHERE token_hash = ?", hashToken(plain),
	).Scan(&userID, &expires)
	if err != nil {
		return "unknown-session"
	}
	if expires <= Now() {
		return "expired-session"
	}
	var status string
	if err := db.QueryRowContext(ctx, "SELECT status FROM users WHERE id = ?", userID).Scan(&status); err != nil || status != StatusActive {
		return "disabled-user"
	}
	return "invalid-session"
}
