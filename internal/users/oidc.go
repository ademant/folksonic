package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// OIDCIdentity is one external sign-in identity linked to an account. The pair
// (Issuer, Subject) is what the identity provider guarantees to be stable.
type OIDCIdentity struct {
	ID        int64
	Issuer    string
	Subject   string
	CreatedAt string
}

// UserByOIDC returns the account linked to an identity. A missing link is
// ErrNotFound: OIDC never creates accounts on its own.
func UserByOIDC(ctx context.Context, db *sql.DB, issuer, subject string) (User, error) {
	var userID int64
	err := db.QueryRowContext(ctx,
		"SELECT user_id FROM oidc_identities WHERE issuer = ? AND subject = ?", issuer, subject).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return ByID(ctx, db, userID)
}

// LinkOIDC attaches an identity to userID. Linking an identity the user already
// has is a no-op; one held by another account is ErrExists.
func LinkOIDC(ctx context.Context, db *sql.DB, userID int64, issuer, subject string) error {
	res, err := db.ExecContext(ctx,
		`INSERT INTO oidc_identities (issuer, subject, user_id, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (issuer, subject) DO NOTHING`,
		issuer, subject, userID, Now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	owner, err := UserByOIDC(ctx, db, issuer, subject)
	if err != nil {
		return err
	}
	if owner.ID != userID {
		return ErrExists
	}
	return nil
}

// ListOIDCIdentities returns the identities linked to userID.
func ListOIDCIdentities(ctx context.Context, db *sql.DB, userID int64) ([]OIDCIdentity, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, issuer, subject, created_at FROM oidc_identities WHERE user_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OIDCIdentity
	for rows.Next() {
		var i OIDCIdentity
		if err := rows.Scan(&i.ID, &i.Issuer, &i.Subject, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RemoveOIDCIdentity unlinks one identity owned by userID. Anything else is
// ErrNotFound, so one user cannot unlink another's identity.
func RemoveOIDCIdentity(ctx context.Context, db *sql.DB, userID, id int64) error {
	res, err := db.ExecContext(ctx, "DELETE FROM oidc_identities WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// RegisterOIDC creates an account that signs in only through the identity, and
// consumes the invite in the same transaction. The invite must be bound to
// username or unbound, as for passkey sign-up.
func RegisterOIDC(ctx context.Context, db *sql.DB, code, username, issuer, subject string) (User, error) {
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
	var linked int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM oidc_identities WHERE issuer = ? AND subject = ?", issuer, subject).Scan(&linked); err != nil {
		return User{}, err
	}
	if linked > 0 {
		return User{}, ErrExists
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password, role, status, created_at) VALUES (?, NULL, ?, ?, ?)`,
		username, role, StatusActive, now)
	if err != nil {
		return User{}, err
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO oidc_identities (issuer, subject, user_id, created_at) VALUES (?, ?, ?, ?)",
		issuer, subject, userID, now); err != nil {
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

// SignInMethods counts the ways userID can sign in: a password, each passkey
// and each linked identity. Removing a method is refused when it would leave
// this count at zero.
func SignInMethods(ctx context.Context, db *sql.DB, userID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?)
		     + (SELECT COUNT(*) FROM oidc_identities WHERE user_id = ?)
		     + (SELECT CASE WHEN password IS NULL OR password = '' THEN 0 ELSE 1 END FROM users WHERE id = ?)`,
		userID, userID, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting sign-in methods: %w", err)
	}
	return n, nil
}
