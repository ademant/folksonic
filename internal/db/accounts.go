package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// migrateAccounts brings databases created by earlier builds up to the current
// users and invites layout. It is idempotent:
//   - users.webauthn_handle is added, for passkey-only accounts;
//   - invites.username is added, so an invite can name the account it is for;
//   - users.password loses its NOT NULL constraint;
//   - the role "admin" becomes "librarian", which changes the CHECK constraint
//     on users and invites and rewrites the existing rows.
//
// SQLite cannot change a CHECK or NOT NULL constraint in place, so the affected
// tables are rebuilt. The rebuild DDL below must match the schema in db.go.
func migrateAccounts(d *sql.DB) error {
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
	usersOld, err := tableMentions(d, "users", "'admin'")
	if err != nil {
		return err
	}
	invitesOld, err := tableMentions(d, "invites", "'admin'")
	if err != nil {
		return err
	}
	if !notNull && !usersOld && !invitesOld {
		return nil
	}
	return rebuildAccounts(d, notNull || usersOld, invitesOld)
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

// tableMentions reports whether the stored CREATE statement of table contains
// text. It is used to detect the old CHECK constraint on roles.
func tableMentions(d *sql.DB, table, text string) (bool, error) {
	var n int
	err := d.QueryRow("SELECT instr(sql, ?) FROM sqlite_master WHERE type = 'table' AND name = ?", text, table).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return n > 0, err
}

// rebuildAccounts recreates users and/or invites in the current layout, mapping
// the role "admin" to "librarian". Dropping users with foreign keys on would
// cascade-delete every row that refers to it, so the rebuild runs on one
// connection with foreign keys off. Integrity is checked before commit.
func rebuildAccounts(d *sql.DB, users, invites bool) (err error) {
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

	var stmts []string
	if users {
		stmts = append(stmts,
			`CREATE TABLE users_new (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				username    TEXT NOT NULL UNIQUE COLLATE NOCASE,
				password    TEXT,
				role        TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('librarian', 'user')),
				status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'disabled')),
				approved_by INTEGER,
				created_at  TEXT NOT NULL,
				webauthn_handle BLOB
			)`,
			`INSERT INTO users_new (id, username, password, role, status, approved_by, created_at, webauthn_handle)
			 SELECT id, username, password, CASE role WHEN 'admin' THEN 'librarian' ELSE role END,
			        status, approved_by, created_at, webauthn_handle FROM users`,
			`DROP TABLE users`,
			`ALTER TABLE users_new RENAME TO users`)
	}
	if invites {
		stmts = append(stmts,
			`CREATE TABLE invites_new (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				code_hash  TEXT NOT NULL UNIQUE,
				role       TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('librarian', 'user')),
				note       TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				expires_at TEXT NOT NULL,
				used_at    TEXT,
				used_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
				revoked_at TEXT,
				username   TEXT
			)`,
			`INSERT INTO invites_new (id, code_hash, role, note, created_at, expires_at, used_at, used_by, revoked_at, username)
			 SELECT id, code_hash, CASE role WHEN 'admin' THEN 'librarian' ELSE role END,
			        note, created_at, expires_at, used_at, used_by, revoked_at, username FROM invites`,
			`DROP TABLE invites`,
			`ALTER TABLE invites_new RENAME TO invites`)
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
		return errors.New("foreign key violations after rebuilding accounts; migration aborted")
	}
	return tx.Commit()
}
