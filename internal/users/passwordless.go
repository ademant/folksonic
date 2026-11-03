package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// InviteRoleFor checks that code can create username, and returns the role it
// grants. It does not consume the code: RegisterPasskey does that once the
// passkey has been verified. The error is ErrInviteInvalid for a bad code or a
// username the code is not bound to, and ErrExists for a taken username.
func InviteRoleFor(ctx context.Context, db *sql.DB, code, username string) (string, error) {
	if err := ValidUsername(username); err != nil {
		return "", err
	}
	body, ok := normaliseInvite(code)
	if !ok {
		return "", ErrInviteInvalid
	}
	var role string
	var bound sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT role, username FROM invites
		 WHERE code_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		hashToken(body), Now()).Scan(&role, &bound)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInviteInvalid
	}
	if err != nil {
		return "", err
	}
	if bound.Valid && !strings.EqualFold(bound.String, username) {
		return "", ErrInviteInvalid
	}
	var taken int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE username = ?", username).Scan(&taken); err != nil {
		return "", err
	}
	if taken > 0 {
		return "", ErrExists
	}
	return role, nil
}

// RegisterPasskey creates a passkey-only account and consumes the invite in one
// transaction. credentialJSON is the passkey the browser has just verified and
// handle its WebAuthn user handle. Both are required, so no account can exist
// without a sign-in method.
func RegisterPasskey(ctx context.Context, db *sql.DB, code, username string, handle []byte, credentialJSON string) (User, error) {
	if len(handle) == 0 || credentialJSON == "" {
		return User{}, fmt.Errorf("%w: a passkey is required", ErrInvalid)
	}
	if err := ValidUsername(username); err != nil {
		return User{}, err
	}
	body, ok := normaliseInvite(code)
	if !ok {
		return User{}, ErrInviteInvalid
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	now := Now()
	var inviteID int64
	var role string
	err = tx.QueryRowContext(ctx,
		`UPDATE invites SET used_at = ?
		 WHERE code_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?
		   AND (username IS NULL OR username = ? COLLATE NOCASE)
		 RETURNING id, role`,
		now, hashToken(body), now, username).Scan(&inviteID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrInviteInvalid
	}
	if err != nil {
		return User{}, err
	}

	var taken int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE username = ?", username).Scan(&taken); err != nil {
		return User{}, err
	}
	if taken > 0 {
		return User{}, ErrExists
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password, role, status, created_at, webauthn_handle) VALUES (?, NULL, ?, ?, ?, ?)`,
		username, role, StatusActive, now, handle)
	if err != nil {
		return User{}, err
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO webauthn_credentials (user_id, credential, created_at) VALUES (?, ?, ?)",
		userID, credentialJSON, now); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE invites SET used_by = ? WHERE id = ?", userID, inviteID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return ByID(ctx, db, userID)
}
