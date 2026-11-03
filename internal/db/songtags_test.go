package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSyncSongTagsForcesRescanOnlyOnChange(t *testing.T) {
	d, err := Init(filepath.Join(t.TempDir(), "tags.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.Exec("INSERT INTO artists (id, name, name_lower) VALUES ('ar-1', 'A', 'a')"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO albums (id, title, artist, artist_id) VALUES ('al-1', 'B', 'A', 'ar-1')"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO songs (id, path, title, artist, artist_id, album, album_id, suffix, content_type, mtime) VALUES ('so-1', '/m/x.flac', 'x', 'A', 'ar-1', 'B', 'al-1', 'flac', 'audio/flac', 99)"); err != nil {
		t.Fatal(err)
	}

	// An empty allowlist against a never-configured library changes nothing.
	if changed, err := SyncSongTags(ctx, d, nil); err != nil || changed {
		t.Fatalf("empty allowlist on a fresh library: changed=%v err=%v", changed, err)
	}
	if changed, err := SyncSongTags(ctx, d, []string{"DANCE", "USE"}); err != nil || !changed {
		t.Fatalf("new allowlist not detected: changed=%v err=%v", changed, err)
	}
	var mtime int64
	if err := d.QueryRow("SELECT mtime FROM songs WHERE id = 'so-1'").Scan(&mtime); err != nil || mtime != 0 {
		t.Fatalf("mtime not reset: %v %d", err, mtime)
	}
	if changed, err := SyncSongTags(ctx, d, []string{"DANCE", "USE"}); err != nil || changed {
		t.Fatalf("unchanged allowlist reported as changed: %v %v", changed, err)
	}
}
