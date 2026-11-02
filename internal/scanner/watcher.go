package scanner

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchDebounce is how long we wait after the last Create/Write event for a
// given file before reading its metadata. Without this we'd race against
// in-progress copies and read truncated tag data.
const watchDebounce = 750 * time.Millisecond

// Watch monitors every root recursively and keeps the library in sync with the
// filesystem: new audio files are scanned and inserted, removed files are
// purged from the database. It blocks until ctx is cancelled.
func Watch(ctx context.Context, pool *sql.DB, roots []string, coversDir string, progress *Progress) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	for _, root := range roots {
		progress.watchFailures.Add(int64(addRecursive(w, root)))
	}
	log.Printf("watch: monitoring %d music director(ies) for changes (%d directories not watched)", len(roots), progress.WatchFailures())

	d := newDebouncer(watchDebounce)
	defer d.stopAll()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log.Printf("watch error: %v", err)
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			handleEvent(w, pool, coversDir, ev, d, progress)
		}
	}
}

func handleEvent(w *fsnotify.Watcher, pool *sql.DB, coversDir string, ev fsnotify.Event, d *debouncer, progress *Progress) {
	switch {
	case ev.Op&fsnotify.Create != 0:
		info, err := os.Stat(ev.Name)
		if err != nil {
			return
		}
		if info.IsDir() {
			progress.watchFailures.Add(int64(addRecursive(w, ev.Name)))
			walkAudio(ev.Name, make(map[string]bool), func(p string) {
				d.schedule(p, func() { runProcess(pool, coversDir, p) })
			})
			return
		}
		if hasAudioExt(ev.Name) {
			d.schedule(ev.Name, func() { runProcess(pool, coversDir, ev.Name) })
		}

	case ev.Op&fsnotify.Write != 0:
		if hasAudioExt(ev.Name) {
			d.schedule(ev.Name, func() { runProcess(pool, coversDir, ev.Name) })
		}

	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		d.cancel(ev.Name)
		// We don't know whether the path was a file or a directory anymore.
		// Try the single-song path first, then a prefix sweep in case a whole
		// directory disappeared without per-file events.
		if err := DeletePath(pool, ev.Name); err != nil {
			log.Printf("watch delete %s: %v", ev.Name, err)
		}
		// A file cannot contain songs, so the directory sweep is only needed
		// for paths that are not audio files. This avoids a full-table LIKE
		// scan on every removed audio file.
		if !hasAudioExt(ev.Name) {
			if err := deletePrefix(pool, ev.Name); err != nil {
				log.Printf("watch delete prefix %s: %v", ev.Name, err)
			}
		}
	}
}

func runProcess(pool *sql.DB, coversDir, path string) {
	// File may have vanished between event and debounce fire.
	if _, err := os.Stat(path); err != nil {
		return
	}
	res, err := processFile(pool, path, coversDir)
	if err != nil {
		log.Printf("watch process %s: %v", path, err)
		return
	}
	switch res {
	case resultInserted:
		log.Printf("watch: added %s", path)
	case resultUpdated:
		log.Printf("watch: updated %s", path)
	}
}

// addRecursive watches root and every directory below it. It returns how many
// directories could not be watched, for example at the inotify watch limit.
// Those directories stay out of sync until the next restart, so the count is
// surfaced in getScanStatus rather than only written to the log.
func addRecursive(w *fsnotify.Watcher, root string) int {
	failures := 0
	_ = filepath.WalkDir(root, func(path string, dirEntry os.DirEntry, err error) error {
		if err != nil {
			// Don't abort the whole walk on a single unreadable dir.
			log.Printf("watch walk %s: %v", path, err)
			failures++
			return nil
		}
		if !dirEntry.IsDir() {
			return nil
		}
		if err := w.Add(path); err != nil {
			log.Printf("watch add %s: %v", path, err)
			failures++
		}
		return nil
	})
	return failures
}

// DeletePath removes the song at path from the library and prunes its album
// and artist if they no longer have any songs. It is a no-op if no song with
// that path exists.
func DeletePath(pool *sql.DB, path string) error {
	tx, err := pool.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var songID, albumID, artistID string
	err = tx.QueryRow(
		"SELECT id, album_id, artist_id FROM songs WHERE path = ?", path,
	).Scan(&songID, &albumID, &artistID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM songs WHERE id = ?", songID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM playlist_songs WHERE song_id = ?", songID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM starred WHERE id = ?", songID); err != nil {
		return err
	}

	if err := pruneAlbum(tx, albumID); err != nil {
		return err
	}
	if err := pruneArtist(tx, artistID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("watch: removed %s from library", path)
	return nil
}

func pruneAlbum(tx *sql.Tx, albumID string) error {
	var n int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM songs WHERE album_id = ?", albumID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := tx.Exec("DELETE FROM albums WHERE id = ?", albumID); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM starred WHERE id = ?", albumID)
	return err
}

func pruneArtist(tx *sql.Tx, artistID string) error {
	var songs, albums int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM songs WHERE artist_id = ?", artistID).Scan(&songs); err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT COUNT(*) FROM albums WHERE artist_id = ?", artistID).Scan(&albums); err != nil {
		return err
	}
	if songs > 0 || albums > 0 {
		return nil
	}
	if _, err := tx.Exec("DELETE FROM artists WHERE id = ?", artistID); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM starred WHERE id = ?", artistID)
	return err
}

// deletePrefix removes every song whose path lives under prefix. Used when a
// directory is removed and we don't get per-file events for its contents.
func deletePrefix(pool *sql.DB, prefix string) error {
	paths, err := songPathsUnder(pool, prefix)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := DeletePath(pool, p); err != nil {
			log.Printf("watch delete %s: %v", p, err)
		}
	}
	return nil
}

type debouncer struct {
	mu      sync.Mutex
	pending map[string]*time.Timer
	delay   time.Duration
}

func newDebouncer(delay time.Duration) *debouncer {
	return &debouncer{pending: map[string]*time.Timer{}, delay: delay}
}

func (d *debouncer) schedule(key string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.pending[key]; ok {
		t.Stop()
	}
	d.pending[key] = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		delete(d.pending, key)
		d.mu.Unlock()
		fn()
	})
}

func (d *debouncer) cancel(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.pending[key]; ok {
		t.Stop()
		delete(d.pending, key)
	}
}

func (d *debouncer) stopAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.pending {
		t.Stop()
		delete(d.pending, k)
	}
}

// songPathsUnder returns the stored path of every song below root.
func songPathsUnder(pool *sql.DB, root string) ([]string, error) {
	like := strings.ReplaceAll(root, "\\", "\\\\")
	like = strings.ReplaceAll(like, "%", "\\%")
	like = strings.ReplaceAll(like, "_", "\\_")
	like += string(filepath.Separator) + "%"

	rows, err := pool.Query(`SELECT path FROM songs WHERE path LIKE ? ESCAPE '\'`, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}
