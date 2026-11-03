package users

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tsirysndr/tinysonic/internal/db"
)

// Legacy stars and playlists (NULL owner) go to the first admin.
func TestAdoptLegacyDataAssignsFirstAdmin(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Init(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	admin, err := Create(ctx, pool, "admin", "pw", RoleLibrarian, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO starred (user_id, id, starred_at) VALUES (NULL, 'al-old', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO playlists (id, name, created_at, updated_at) VALUES ('pl-old', 'Old', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	if err := AdoptLegacyData(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var owner int64
	if err := pool.QueryRow("SELECT user_id FROM starred WHERE id = 'al-old'").Scan(&owner); err != nil || owner != admin.ID {
		t.Fatalf("star owner = %d (%v), want %d", owner, err, admin.ID)
	}
	if err := pool.QueryRow("SELECT owner_id FROM playlists WHERE id = 'pl-old'").Scan(&owner); err != nil || owner != admin.ID {
		t.Fatalf("playlist owner = %d (%v), want %d", owner, err, admin.ID)
	}
}
