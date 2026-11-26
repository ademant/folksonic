package scanner

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/dhowden/tag"

	"github.com/tsirysndr/tinysonic/internal/logging"
)

// A file that could not be read as audio is recorded in scan_failures with
// its size and modification time (#219). The scan and the watcher skip it
// until the file changes, so a file that makes a parser panic costs one
// attempt and not one per scan or one crash per restart.

// ErrNotAudio is a file named like audio whose content is no audio format
// the scanner knows.
var ErrNotAudio = errors.New("not an audio file")

// errParserPanic is a parser that panicked on the file.
var errParserPanic = errors.New("parser failed")

// maxTagBytes bounds how much the tag reader may read from one file. The
// largest legitimate part is an embedded cover (maxCoverBytes); past this,
// the tags are ignored and the file is indexed with names from its path. A
// variable only so tests can lower it.
var maxTagBytes int64 = 2*maxCoverBytes + 4<<20

// readTags is the tag reader, a variable so tests can make it panic.
var readTags = tag.ReadFrom

// safeExtract runs extractMetadata and turns a panic in any parser it calls
// into an error. The stack goes to the debug log.
func safeExtract(roots []string, path string, filesize int64, coversDir string, songTags []string, suffix string) (ex extracted, err error) {
	defer func() {
		if v := recover(); v != nil {
			logging.Debugf("scan %s: parser panic: %v\n%s", path, v, debug.Stack())
			err = fmt.Errorf("%w: %v", errParserPanic, v)
		}
	}()
	return extractMetadata(roots, path, filesize, coversDir, songTags, suffix)
}

// limitedReadSeeker fails reads once n bytes have been read in total. Seeks
// are free, so a tag at the end of a large file is still reached.
type limitedReadSeeker struct {
	rs   io.ReadSeeker
	left int64
}

var errTagTooLarge = errors.New("tags larger than the scanner reads")

func (l *limitedReadSeeker) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, errTagTooLarge
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.rs.Read(p)
	l.left -= int64(n)
	return n, err
}

func (l *limitedReadSeeker) Seek(off int64, whence int) (int64, error) {
	return l.rs.Seek(off, whence)
}

// knownFailure reports whether path failed before at this size and mtime.
func knownFailure(pool *sql.DB, path string, size, mtimeMs int64) (bool, error) {
	var n int
	err := pool.QueryRow("SELECT COUNT(*) FROM scan_failures WHERE path = ? AND file_size = ? AND file_mtime_ms = ?",
		path, size, mtimeMs).Scan(&n)
	return n > 0, err
}

// recordFailure stores why path could not be indexed.
func recordFailure(pool *sql.DB, path string, size, mtimeMs int64, reason error) {
	if _, err := pool.Exec(`INSERT INTO scan_failures (path, file_size, file_mtime_ms, reason, failed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET file_size = excluded.file_size, file_mtime_ms = excluded.file_mtime_ms,
		  reason = excluded.reason, failed_at = excluded.failed_at`,
		path, size, mtimeMs, reason.Error(), time.Now().UTC().Format("2006-01-02T15:04:05Z")); err != nil {
		logging.Errorf("scan: recording failure of %s: %v", path, err)
	}
}

// clearFailure forgets a failure, after the file was read or removed.
func clearFailure(pool *sql.DB, path string) {
	if _, err := pool.Exec("DELETE FROM scan_failures WHERE path = ?", path); err != nil {
		logging.Errorf("scan: clearing failure of %s: %v", path, err)
	}
}

// pruneFailures forgets failures under the given roots whose files the scan
// did not see.
func pruneFailures(pool *sql.DB, roots []string, seen map[string]bool) error {
	for _, root := range roots {
		rows, err := pool.Query(`SELECT path FROM scan_failures WHERE path LIKE ? ESCAPE '\'`, likePrefix(root))
		if err != nil {
			return err
		}
		var gone []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			if !seen[p] {
				gone = append(gone, p)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range gone {
			if _, err := pool.Exec("DELETE FROM scan_failures WHERE path = ?", p); err != nil {
				return err
			}
		}
	}
	return nil
}

// likePrefix is a LIKE pattern for every path below dir.
func likePrefix(dir string) string {
	like := strings.ReplaceAll(dir, "\\", "\\\\")
	like = strings.ReplaceAll(like, "%", "\\%")
	like = strings.ReplaceAll(like, "_", "\\_")
	return like + string(filepath.Separator) + "%"
}

// Failure is a file the scanner could not index.
type Failure struct {
	Path     string
	Size     int64
	Reason   string
	FailedAt string
}

// CountFailures is the number of files currently recorded as failed.
func CountFailures(pool *sql.DB) (int64, error) {
	var n int64
	err := pool.QueryRow("SELECT COUNT(*) FROM scan_failures").Scan(&n)
	return n, err
}

// ListFailures returns up to limit failed files, newest first.
func ListFailures(pool *sql.DB, limit int) ([]Failure, error) {
	rows, err := pool.Query(`SELECT path, file_size, reason, failed_at FROM scan_failures
		ORDER BY failed_at DESC, path LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		var f Failure
		if err := rows.Scan(&f.Path, &f.Size, &f.Reason, &f.FailedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// sniffFile opens path and names its container.
func sniffFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return sniffFormat(f), nil
}
