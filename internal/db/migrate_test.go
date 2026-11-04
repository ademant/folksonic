package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created before accounts existed has starred(id PRIMARY KEY) and
// playlists without an owner. Init must upgrade it in place: playlists keep
// their rows, and stars are cleared because their IDs use the old scheme.
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

	// The star's ID is in the old MD5 scheme. The ID-scheme reset removes it,
	// since the library is rebuilt under the new IDs (see migrateIDScheme).
	var stars int
	if err := pool.QueryRow("SELECT COUNT(*) FROM starred WHERE id = 'al-old'").Scan(&stars); err != nil {
		t.Fatal(err)
	}
	if stars != 0 {
		t.Fatalf("star with an old-scheme ID kept after the reset: %d", stars)
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
