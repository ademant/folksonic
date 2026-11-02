package users

import (
	"context"
	"database/sql"
	"errors"
)

// PasskeyRecord is one stored WebAuthn credential. JSON is the serialized
// webauthn.Credential; ID identifies the row for updates.
type PasskeyRecord struct {
	ID   int64
	JSON string
}

func AddPasskey(ctx context.Context, db *sql.DB, userID int64, credentialJSON string) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO webauthn_credentials (user_id, credential, created_at) VALUES (?, ?, ?)",
		userID, credentialJSON, Now())
	return err
}

func ListPasskeys(ctx context.Context, db *sql.DB, userID int64) ([]PasskeyRecord, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, credential FROM webauthn_credentials WHERE user_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PasskeyRecord
	for rows.Next() {
		var r PasskeyRecord
		if err := rows.Scan(&r.ID, &r.JSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdatePasskey stores a credential after a successful login, so the
// signature counter stays current and clone detection keeps working.
func UpdatePasskey(ctx context.Context, db *sql.DB, id int64, credentialJSON string) error {
	res, err := db.ExecContext(ctx, "UPDATE webauthn_credentials SET credential = ? WHERE id = ?", credentialJSON, id)
	if err != nil {
		return err
	}
	return requireOne(res)
}

// AdoptLegacyData assigns starred songs, albums, artists and playlists that
// predate accounts to the first admin. Rows keep a NULL owner until an admin
// exists, so this runs after the bootstrap and is safe to call repeatedly.
func AdoptLegacyData(ctx context.Context, db *sql.DB) error {
	var owner sql.NullInt64
	err := db.QueryRowContext(ctx,
		"SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1").Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE OR IGNORE starred SET user_id = ? WHERE user_id IS NULL", owner.Int64); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"UPDATE playlists SET owner_id = ? WHERE owner_id IS NULL", owner.Int64)
	return err
}
