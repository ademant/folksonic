package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// migratePasswordless brings databases created before passwordless accounts up
// to the current users and invites layout. It is idempotent:
//   - users.webauthn_handle is added, for passkey-only accounts;
//   - invites.username is added, so an invite can name the account it is for;
//   - users.password loses its NOT NULL constraint. SQLite cannot drop that in
//     place, so users is rebuilt.
func migratePasswordless(d *sql.DB) error {
	found, _, err := tableColumn(d, "users", "webauthn_handle")
	if err != nil {
		return err
	}
	if !found {
		if _, err := d.Exec("ALTER TABLE users ADD COLUMN webauthn_handle BLOB"); err != nil {
			return err
		}
	}
	found, _, err = tableColumn(d, "invites", "username")
	if err != nil {
		return err
	}
	if !found {
		if _, err := d.Exec("ALTER TABLE invites ADD COLUMN username TEXT"); err != nil {
			return err
		}
	}
	_, notNull, err := tableColumn(d, "users", "password")
	if err != nil {
		return err
	}
	if notNull {
		return rebuildUsersNullablePassword(d)
	}
	return nil
}

// tableColumn reports whether table has column col, and whether it is NOT NULL.
// The table and column names are fixed in this package, never user input.
func tableColumn(d *sql.DB, table, col string) (found, notNull bool, err error) {
	var nn int
	err = d.QueryRow(fmt.Sprintf(`SELECT "notnull" FROM pragma_table_info('%s') WHERE name = '%s'`, table, col)).Scan(&nn)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, nn == 1, nil
}

// rebuildUsersNullablePassword recreates users with a nullable password.
// Dropping users with foreign keys on would cascade-delete every row that
// refers to it, so the rebuild runs on one connection with foreign keys off,
// checks integrity before committing, then turns them back on.
func rebuildUsersNullablePassword(d *sql.DB) (err error) {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer func() {
		if _, rerr := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); rerr != nil && err == nil {
			err = rerr
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE users_new (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			username    TEXT NOT NULL UNIQUE COLLATE NOCASE,
			password    TEXT,
			role        TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
			status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'disabled')),
			approved_by INTEGER,
			created_at  TEXT NOT NULL,
			webauthn_handle BLOB
		)`,
		`INSERT INTO users_new (id, username, password, role, status, approved_by, created_at, webauthn_handle)
		 SELECT id, username, password, role, status, approved_by, created_at, webauthn_handle FROM users`,
		`DROP TABLE users`,
		`ALTER TABLE users_new RENAME TO users`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}

	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	violations := rows.Next()
	rows.Close()
	if violations {
		return errors.New("foreign key violations after rebuilding users; migration aborted")
	}
	return tx.Commit()
}
