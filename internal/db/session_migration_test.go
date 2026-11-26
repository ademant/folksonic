package db

import (
	"path/filepath"
	"testing"
)

// #189: an existing session gets created_at and last_used_at, so the upgrade
// signs no one out, and the step can run again.
func TestMigrateSessionActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	d, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, username, status, created_at) VALUES (1, 'a', 'active', '2026-01-01T00:00:00Z')`,
		`ALTER TABLE sessions DROP COLUMN created_at`,
		`ALTER TABLE sessions DROP COLUMN last_used_at`,
		`ALTER TABLE sessions DROP COLUMN label`,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ('h', 1, '2099-01-01T00:00:00Z')`,
		`PRAGMA user_version = 12`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()
	for range 2 {
		d, err = Init(path)
		if err != nil {
			t.Fatal(err)
		}
		var created, used, label string
		if err := d.QueryRow(`SELECT created_at, last_used_at, label FROM sessions`).Scan(&created, &used, &label); err != nil {
			t.Fatal(err)
		}
		if created == "" || used == "" || label != "" {
			t.Fatalf("created %q used %q label %q", created, used, label)
		}
		if _, err := d.Exec(`PRAGMA user_version = 12`); err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
}
