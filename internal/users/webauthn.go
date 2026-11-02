package users

import (
	"context"
	"database/sql"
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
