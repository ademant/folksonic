package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created before accounts existed has starred(id PRIMARY KEY) and
// playlists without an owner. Init must upgrade it in place and keep the rows.
func TestMigratesLegacyStarredAndPlaylists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE starred (id TEXT PRIMARY KEY, starred_at TEXT NOT NULL)`,
		`INSERT INTO starred (id, starred_at) VALUES ('al-old', '2025-01-01T00:00:00Z')`,
		`CREATE TABLE playlists (id TEXT PRIMARY KEY, name TEXT NOT NULL, comment TEXT,
			public INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO playlists (id, name, created_at, updated_at) VALUES ('pl-old', 'Old', 't', 't')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	pool, err := Init(path)
	if err != nil {
		t.Fatalf("Init on legacy database: %v", err)
	}
	defer pool.Close()

	var stars int
	if err := pool.QueryRow("SELECT COUNT(*) FROM starred WHERE id = 'al-old' AND user_id IS NULL").Scan(&stars); err != nil {
		t.Fatal(err)
	}
	if stars != 1 {
		t.Fatalf("legacy star not kept with NULL owner: %d", stars)
	}
	var pls int
	if err := pool.QueryRow("SELECT COUNT(*) FROM playlists WHERE id = 'pl-old' AND owner_id IS NULL").Scan(&pls); err != nil {
		t.Fatal(err)
	}
	if pls != 1 {
		t.Fatalf("legacy playlist not kept with NULL owner: %d", pls)
	}

	// Running the migration again must be harmless.
	if err := migrate(pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
