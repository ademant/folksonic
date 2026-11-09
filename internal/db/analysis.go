package db

import (
	"database/sql"
	"fmt"
)

// analysisSchema holds per-track analysis. analysis records which tool last
// produced each feature of a song (or why it failed); song_values holds the
// values themselves as JSON, whether read from a tag or computed. Both are
// keyed by song id and go with the song.
const analysisSchema = `
CREATE TABLE IF NOT EXISTS analysis (
    song_id      TEXT NOT NULL REFERENCES songs(id) ON DELETE CASCADE ON UPDATE CASCADE,
    feature      TEXT NOT NULL,
    tool         TEXT NOT NULL,              -- 'tag', 'builtin', 'ffmpeg', 'folksentia', …
    tool_version TEXT,
    analysed_ms  INTEGER NOT NULL,
    error        TEXT,                       -- set when the run failed; the value is then left as it was
    PRIMARY KEY (song_id, feature)
);

-- analysis_attempts counts runs of a tool on a track that started but did not
-- store a result (the process was killed or the server stopped). A row is
-- written before each run and removed when the run's result is stored; a
-- track that reaches the limit is recorded as failed instead of retried.
CREATE TABLE IF NOT EXISTS analysis_attempts (
    song_id   TEXT NOT NULL REFERENCES songs(id) ON DELETE CASCADE ON UPDATE CASCADE,
    tool      TEXT NOT NULL,
    attempts  INTEGER NOT NULL,
    started_ms INTEGER NOT NULL,
    PRIMARY KEY (song_id, tool)
);

CREATE TABLE IF NOT EXISTS song_values (
    song_id    TEXT NOT NULL REFERENCES songs(id) ON DELETE CASCADE ON UPDATE CASCADE,
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,                -- JSON
    changed_ms INTEGER NOT NULL,             -- when value last changed
    PRIMARY KEY (song_id, name)
);
`

// fileColumns record the size and modification time (ms) of the file the
// analysis belongs to. They are separate from mtime, which migrations reset
// to force a tag re-read without the audio having changed.
var fileColumns = []struct{ name, typ string }{
	{"file_size", "INTEGER"}, {"file_mtime_ms", "INTEGER"},
}

// migrateAnalysis creates the analysis tables and adds the file columns to
// songs. Adding nullable columns is instant even on a large library; NULL
// file columns count as "unknown", so the next re-read of a file records
// them.
func migrateAnalysis(d *sql.DB) error {
	if _, err := d.Exec(analysisSchema); err != nil {
		return err
	}
	for _, c := range fileColumns {
		found, _, err := tableColumn(d, "songs", c.name)
		if err != nil {
			return err
		}
		if !found {
			if _, err := d.Exec(fmt.Sprintf("ALTER TABLE songs ADD COLUMN %s %s", c.name, c.typ)); err != nil {
				return err
			}
		}
	}
	return nil
}
