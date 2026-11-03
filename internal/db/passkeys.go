package db

import "database/sql"

// migratePasskeyLabel adds webauthn_credentials.label, the name a user gives a
// passkey on the account page. It is idempotent.
func migratePasskeyLabel(d *sql.DB) error {
	found, _, err := tableColumn(d, "webauthn_credentials", "label")
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = d.Exec("ALTER TABLE webauthn_credentials ADD COLUMN label TEXT NOT NULL DEFAULT ''")
	return err
}
