package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

func init() {
	sqlite3.AutoExtension(fts5.Register)
}

func Init(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating db parent dir %s: %w", dir, err)
		}
	}
	dsn := fmt.Sprintf(
		"file:%s"+
			"?_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)"+
			"&_pragma=foreign_keys(on)"+
			"&_pragma=busy_timeout(5000)"+
			"&_pragma=cache_size(-20000)"+
			"&_pragma=temp_store(memory)"+
			"&_pragma=mmap_size(67108864)",
		path,
	)
	d, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite at %s: %w", path, err)
	}
	d.SetMaxOpenConns(8)
	if err := d.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(d); err != nil {
		return nil, err
	}
	return d, nil
}

func migrate(d *sql.DB) error {
	if _, err := d.Exec(schema); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return backfillFTS(d)
}

const schema = `
CREATE TABLE IF NOT EXISTS artists (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    name_lower   TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS albums (
    id           TEXT PRIMARY KEY,
    title        TEXT NOT NULL,
    artist       TEXT NOT NULL,
    artist_id    TEXT NOT NULL,
    year         INTEGER NOT NULL DEFAULT 0,
    cover_art    TEXT,
    FOREIGN KEY (artist_id) REFERENCES artists(id)
);

CREATE INDEX IF NOT EXISTS idx_albums_artist_id ON albums(artist_id);
CREATE INDEX IF NOT EXISTS idx_albums_title ON albums(title);

CREATE TABLE IF NOT EXISTS songs (
    id            TEXT PRIMARY KEY,
    path          TEXT NOT NULL UNIQUE,
    title         TEXT NOT NULL,
    artist        TEXT NOT NULL,
    artist_id     TEXT NOT NULL,
    album         TEXT NOT NULL,
    album_id      TEXT NOT NULL,
    genre         TEXT,
    track_number  INTEGER,
    disc_number   INTEGER,
    year          INTEGER,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    bitrate       INTEGER NOT NULL DEFAULT 0,
    filesize      INTEGER NOT NULL DEFAULT 0,
    suffix        TEXT NOT NULL,
    content_type  TEXT NOT NULL,
    cover_art     TEXT,
    mtime         INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (album_id) REFERENCES albums(id),
    FOREIGN KEY (artist_id) REFERENCES artists(id)
);

CREATE INDEX IF NOT EXISTS idx_songs_album_id ON songs(album_id);
CREATE INDEX IF NOT EXISTS idx_songs_artist_id ON songs(artist_id);
CREATE INDEX IF NOT EXISTS idx_songs_title ON songs(title);
CREATE INDEX IF NOT EXISTS idx_songs_genre ON songs(genre);

CREATE TABLE IF NOT EXISTS starred (
    id          TEXT PRIMARY KEY,
    starred_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS playlists (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    comment     TEXT,
    public      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS playlist_songs (
    playlist_id TEXT NOT NULL,
    position    INTEGER NOT NULL,
    song_id     TEXT NOT NULL,
    PRIMARY KEY (playlist_id, position),
    FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_playlist_songs_pl ON playlist_songs(playlist_id);

CREATE VIRTUAL TABLE IF NOT EXISTS songs_fts USING fts5(
    id UNINDEXED, title, artist, album, genre,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE VIRTUAL TABLE IF NOT EXISTS albums_fts USING fts5(
    id UNINDEXED, title, artist,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE VIRTUAL TABLE IF NOT EXISTS artists_fts USING fts5(
    id UNINDEXED, name,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS songs_ai AFTER INSERT ON songs BEGIN
    INSERT INTO songs_fts (id, title, artist, album, genre)
    VALUES (NEW.id, NEW.title, NEW.artist, NEW.album, COALESCE(NEW.genre, ''));
END;

CREATE TRIGGER IF NOT EXISTS songs_ad AFTER DELETE ON songs BEGIN
    DELETE FROM songs_fts WHERE id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS songs_au AFTER UPDATE ON songs BEGIN
    DELETE FROM songs_fts WHERE id = OLD.id;
    INSERT INTO songs_fts (id, title, artist, album, genre)
    VALUES (NEW.id, NEW.title, NEW.artist, NEW.album, COALESCE(NEW.genre, ''));
END;

CREATE TRIGGER IF NOT EXISTS albums_ai AFTER INSERT ON albums BEGIN
    INSERT INTO albums_fts (id, title, artist)
    VALUES (NEW.id, NEW.title, NEW.artist);
END;

CREATE TRIGGER IF NOT EXISTS albums_ad AFTER DELETE ON albums BEGIN
    DELETE FROM albums_fts WHERE id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS albums_au AFTER UPDATE ON albums BEGIN
    DELETE FROM albums_fts WHERE id = OLD.id;
    INSERT INTO albums_fts (id, title, artist)
    VALUES (NEW.id, NEW.title, NEW.artist);
END;

CREATE TRIGGER IF NOT EXISTS artists_ai AFTER INSERT ON artists BEGIN
    INSERT INTO artists_fts (id, name) VALUES (NEW.id, NEW.name);
END;

CREATE TRIGGER IF NOT EXISTS artists_ad AFTER DELETE ON artists BEGIN
    DELETE FROM artists_fts WHERE id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS artists_au AFTER UPDATE ON artists BEGIN
    DELETE FROM artists_fts WHERE id = OLD.id;
    INSERT INTO artists_fts (id, name) VALUES (NEW.id, NEW.name);
END;
CREATE TABLE IF NOT EXISTS users (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    username    TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password    TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'disabled')),
    approved_by INTEGER,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label        TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id);

CREATE TABLE IF NOT EXISTS pairing_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    used_at    TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential TEXT NOT NULL,
    created_at TEXT NOT NULL
);

`

func backfillFTS(d *sql.DB) error {
	pairs := []struct {
		fts, fill string
	}{
		{
			"SELECT COUNT(*) FROM songs_fts",
			"INSERT INTO songs_fts (id, title, artist, album, genre) " +
				"SELECT id, title, artist, album, COALESCE(genre, '') FROM songs",
		},
		{
			"SELECT COUNT(*) FROM albums_fts",
			"INSERT INTO albums_fts (id, title, artist) SELECT id, title, artist FROM albums",
		},
		{
			"SELECT COUNT(*) FROM artists_fts",
			"INSERT INTO artists_fts (id, name) SELECT id, name FROM artists",
		},
	}
	for _, p := range pairs {
		var n int64
		if err := d.QueryRow(p.fts).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := d.Exec(p.fill); err != nil {
				return err
			}
		}
	}
	return nil
}
