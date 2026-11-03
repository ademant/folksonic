package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database from before passwordless accounts: password NOT NULL, no handle
// column, no invite username. Child rows must survive the table rebuild.
func TestMigrationMakesPasswordNullable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []string{
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			password TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'disabled')),
			approved_by INTEGER,
			created_at TEXT NOT NULL)`,
		`CREATE TABLE invites (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			code_hash TEXT NOT NULL UNIQUE,
			role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			used_at TEXT,
			used_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
			revoked_at TEXT)`,
		`CREATE TABLE webauthn_credentials (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			credential TEXT NOT NULL,
			created_at TEXT NOT NULL)`,
		`INSERT INTO users (username, password, role, status, created_at) VALUES ('admin', 'pw', 'admin', 'active', '2026-01-01T00:00:00Z')`,
		`INSERT INTO webauthn_credentials (user_id, credential, created_at) VALUES (1, '{}', '2026-01-01T00:00:00Z')`,
		`INSERT INTO invites (code_hash, role, created_at, expires_at) VALUES ('old', 'admin', '2026-01-01T00:00:00Z', '2026-01-08T00:00:00Z')`,
	}
	for _, q := range legacy {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q[:30], err)
		}
	}
	raw.Close()

	d, err := Init(path)
	if err != nil {
		t.Fatalf("migrating legacy database: %v", err)
	}
	defer d.Close()

	var role string
	if err := d.QueryRow("SELECT role FROM users WHERE username = 'admin'").Scan(&role); err != nil || role != "librarian" {
		t.Fatalf("admin role not renamed: %v %q", err, role)
	}
	if err := d.QueryRow("SELECT role FROM invites WHERE code_hash = 'old'").Scan(&role); err != nil || role != "librarian" {
		t.Fatalf("invite role not renamed: %v %q", err, role)
	}
	if _, err := d.Exec("INSERT INTO users (username, password, role, status, created_at) VALUES ('eve', 'x', 'admin', 'active', '2026-01-02T00:00:00Z')"); err == nil {
		t.Fatal("old role name still accepted after migration")
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM users WHERE username = 'admin' AND password = 'pw'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing user not preserved: %v %d", err, n)
	}
	if err := d.QueryRow("SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("child rows deleted by the rebuild: %v %d", err, n)
	}
	if _, err := d.Exec("INSERT INTO users (username, password, role, status, created_at) VALUES ('alice', NULL, 'user', 'active', '2026-01-02T00:00:00Z')"); err != nil {
		t.Fatalf("passwordless insert after migration: %v", err)
	}
	if _, err := d.Exec("INSERT INTO invites (code_hash, created_at, expires_at, username) VALUES ('h', '2026-01-02T00:00:00Z', '2026-01-09T00:00:00Z', 'bob')"); err != nil {
		t.Fatalf("invite username column missing: %v", err)
	}
	d.Close()

	// Running the migrations again must be a no-op.
	d2, err := Init(path)
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	d2.Close()
}
