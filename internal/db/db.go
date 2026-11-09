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
	if err := migrateUserScope(d); err != nil {
		return fmt.Errorf("user-scoped tables: %w", err)
	}
	if err := migrateAccounts(d); err != nil {
		return fmt.Errorf("accounts: %w", err)
	}
	if err := migrateMBID(d); err != nil {
		return fmt.Errorf("musicbrainz ids: %w", err)
	}
	if err := migrateSongTagsColumn(d); err != nil {
		return fmt.Errorf("song tags: %w", err)
	}
	if err := migrateIDScheme(d); err != nil {
		return fmt.Errorf("library ids: %w", err)
	}
	if err := migratePasswordHash(d); err != nil {
		return fmt.Errorf("password hashes: %w", err)
	}
	if err := migratePasskeyLabel(d); err != nil {
		return fmt.Errorf("passkey labels: %w", err)
	}
	if err := migrateSongCreated(d); err != nil {
		return fmt.Errorf("song created_at: %w", err)
	}
	if err := migrateAudioColumns(d); err != nil {
		return fmt.Errorf("loudness, meter and bpm columns: %w", err)
	}
	if err := migrateChangeFeed(d); err != nil {
		return fmt.Errorf("change feed: %w", err)
	}
	if err := migrateAnalysis(d); err != nil {
		return fmt.Errorf("analysis: %w", err)
	}
	if err := migrateKeyScope(d); err != nil {
		return fmt.Errorf("key scope: %w", err)
	}
	return backfillFTS(d)
}

// migrateUserScope brings starred and playlists to the per-user layout. It is
// idempotent. Rows created before accounts existed have a NULL owner; the
// users package assigns them to the first librarian once the bootstrap has run.
func migrateUserScope(d *sql.DB) error {
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM pragma_table_info('starred') WHERE name = 'user_id'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		// Legacy starred table: primary key was the item id alone.
		stmts := []string{
			"ALTER TABLE starred RENAME TO starred_legacy",
			`CREATE TABLE starred (
				user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE,
				id          TEXT NOT NULL,
				starred_at  TEXT NOT NULL
			)`,
			"INSERT INTO starred (user_id, id, starred_at) SELECT NULL, id, starred_at FROM starred_legacy",
			"DROP TABLE starred_legacy",
		}
		for _, q := range stmts {
			if _, err := d.Exec(q); err != nil {
				return err
			}
		}
	}
	if _, err := d.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_starred_user_item ON starred(user_id, id)"); err != nil {
		return err
	}

	if err := d.QueryRow("SELECT COUNT(*) FROM pragma_table_info('playlists') WHERE name = 'owner_id'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := d.Exec("ALTER TABLE playlists ADD COLUMN owner_id INTEGER REFERENCES users(id) ON DELETE CASCADE"); err != nil {
			return err
		}
	}
	_, err := d.Exec("CREATE INDEX IF NOT EXISTS idx_playlists_owner ON playlists(owner_id)")
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS artists (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    name_lower   TEXT NOT NULL UNIQUE,
    mbid         TEXT
);

CREATE TABLE IF NOT EXISTS albums (
    id           TEXT PRIMARY KEY,
    title        TEXT NOT NULL,
    artist       TEXT NOT NULL,
    artist_id    TEXT NOT NULL,
    year         INTEGER NOT NULL DEFAULT 0,
    cover_art    TEXT,
    mbid         TEXT,
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
    mbid          TEXT,
    tags          TEXT,
    created_at    TEXT,                       -- when the file was first added; kept by rescans
    rg_track_gain REAL,                       -- ReplayGain in dB (-18 LUFS reference)
    rg_album_gain REAL,
    rg_track_peak REAL,
    rg_album_peak REAL,
    rg_base_gain  REAL,                       -- Opus header output gain in dB
    meter         TEXT,                       -- METER / TIME_SIGNATURE tag text
    bpm           REAL,
    bpm_source    TEXT,                       -- 'tag', 'calc' (built-in) or an analysis tool name
    bpm_confidence REAL,                      -- 0..1, calculated values only
    bpm_at        TEXT,                       -- when bpm was set
    FOREIGN KEY (album_id) REFERENCES albums(id),
    FOREIGN KEY (artist_id) REFERENCES artists(id)
);

CREATE INDEX IF NOT EXISTS idx_songs_album_id ON songs(album_id);
CREATE INDEX IF NOT EXISTS idx_songs_artist_id ON songs(artist_id);
CREATE INDEX IF NOT EXISTS idx_songs_title ON songs(title);
CREATE INDEX IF NOT EXISTS idx_songs_genre ON songs(genre);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS starred (
    user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE,
    id          TEXT NOT NULL,
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

CREATE TRIGGER IF NOT EXISTS songs_au AFTER UPDATE OF ` + songsFTSColumns + ` ON songs BEGIN
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
    password    TEXT,                        -- legacy plaintext; always NULL after migratePasswordHash
    password_hash TEXT,                      -- argon2id encoding; NULL for passkey-only accounts
    role        TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('librarian', 'user')),
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'disabled')),
    approved_by INTEGER,
    created_at  TEXT NOT NULL,
    webauthn_handle BLOB                     -- random passkey user handle; NULL uses the id
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
    revoked_at   TEXT,
    scope        TEXT NOT NULL DEFAULT ''   -- '' for a full key, 'worker' for an analysis worker
);

CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id);

CREATE TABLE IF NOT EXISTS pairing_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    scope      TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invites (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code_hash  TEXT NOT NULL UNIQUE,
    role       TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('librarian', 'user')),
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    used_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    revoked_at TEXT,
    username   TEXT                          -- when set, only this username may redeem the code
);

CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS oidc_identities (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    UNIQUE (issuer, subject)
);
CREATE INDEX IF NOT EXISTS idx_oidc_identities_user ON oidc_identities(user_id);

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
