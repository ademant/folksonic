package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ValidRole reports whether role is one of the account roles.
func ValidRole(role string) bool {
	return role == RoleUser || role == RoleLibrarian
}

// SetRole changes an account's role. Roles are read on every request, so the
// change applies to existing sessions and keys immediately.
func SetRole(ctx context.Context, db *sql.DB, id int64, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("%w: role must be %q or %q", ErrInvalid, RoleUser, RoleLibrarian)
	}
	res, err := db.ExecContext(ctx, "UPDATE users SET role = ? WHERE id = ?", role, id)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// Rename changes an account's username. Everything else references the
// numeric id, so playlists, stars, keys, sessions and passkeys stay attached.
// A change of case only ("alice" → "Alice") is allowed.
func Rename(ctx context.Context, db *sql.DB, id int64, newName string) error {
	if err := ValidUsername(newName); err != nil {
		return err
	}
	var other int64
	err := db.QueryRowContext(ctx,
		"SELECT id FROM users WHERE username = ? COLLATE NOCASE AND id <> ?", newName, id).Scan(&other)
	if err == nil {
		return ErrExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	res, err := db.ExecContext(ctx, "UPDATE users SET username = ? WHERE id = ?", newName, id)
	if err != nil {
		// A concurrent rename or registration can still hit the UNIQUE index.
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrExists
		}
		return err
	}
	return requireOne(res)
}

// ListAllAPIKeys returns all of userID's keys, revoked ones included, for
// administration. ListAPIKeys returns only the usable ones.
func ListAllAPIKeys(ctx context.Context, db *sql.DB, userID int64) ([]APIKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, label, prefix, created_at, expires_at, last_used_at, revoked_at, scope
		FROM api_keys WHERE user_id = ? ORDER BY id`, userID)
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

// State describes a key as "active", "expired" or "revoked".
func (k APIKey) State() string {
	switch {
	case k.RevokedAt.Valid:
		return "revoked"
	case k.ExpiresAt.Valid && k.ExpiresAt.String <= Now():
		return "expired"
	}
	return "active"
}

// Summary is an account with the ways it can sign in, for `user list`.
type Summary struct {
	User
	HasPassword bool
	Passkeys    int
	OIDC        int
	ActiveKeys  int // API keys that are neither revoked nor expired
}

// ListSummaries returns every account with its sign-in methods, ordered like
// List.
func ListSummaries(ctx context.Context, db *sql.DB) ([]Summary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT u.id, u.username, u.password_hash, u.role, u.status, u.created_at, u.webauthn_handle,
		       (SELECT COUNT(*) FROM webauthn_credentials w WHERE w.user_id = u.id),
		       (SELECT COUNT(*) FROM oidc_identities o WHERE o.user_id = u.id),
		       (SELECT COUNT(*) FROM api_keys k WHERE k.user_id = u.id AND k.revoked_at IS NULL
		            AND (k.expires_at IS NULL OR k.expires_at > ?))
		FROM users u ORDER BY u.username COLLATE NOCASE`, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var s Summary
		var pw sql.NullString
		if err := rows.Scan(&s.ID, &s.Username, &pw, &s.Role, &s.Status, &s.CreatedAt, &s.WebAuthnHandle,
			&s.Passkeys, &s.OIDC, &s.ActiveKeys); err != nil {
			return nil, err
		}
		s.PasswordHash = pw.String
		s.HasPassword = pw.String != ""
		out = append(out, s)
	}
	return out, rows.Err()
}
