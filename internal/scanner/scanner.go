package scanner

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dhowden/tag"

	"github.com/tsirysndr/tinysonic/internal/audioinfo"
	"github.com/tsirysndr/tinysonic/internal/logging"
)

var audioExts = map[string]bool{
	"mp3": true, "ogg": true, "flac": true, "m4a": true, "aac": true,
	"mp4": true, "alac": true, "wav": true, "wv": true, "mpc": true,
	"aiff": true, "aif": true, "opus": true, "ape": true, "wma": true,
}

// Progress is the live counter exposed through getScanStatus.
type Progress struct {
	running       atomic.Bool
	count         atomic.Int64
	watchFailures atomic.Int64
	// afterChange is called after a scan finishes and after the watcher
	// added or updated a song, so background analysis can pick up the work.
	afterChange atomic.Pointer[func()]
	// songChange is called, synchronously, with the id of each song a scan
	// or the watcher inserted, updated or removed, so the server can drop
	// what it cached about it (#197).
	songChange atomic.Pointer[func(id string)]
}

// SetSongChanged registers fn to run with the id of every song that was
// inserted, updated or removed. It runs in the scanning goroutine, after the
// change is committed, so it must be quick.
func (p *Progress) SetSongChanged(fn func(id string)) { p.songChange.Store(&fn) }

func (p *Progress) songChanged(id string) {
	if p == nil || id == "" {
		return
	}
	if fn := p.songChange.Load(); fn != nil {
		(*fn)(id)
	}
}

// SetAfterChange registers fn to run (in its own goroutine) after a scan
// and after each song the watcher adds or updates.
func (p *Progress) SetAfterChange(fn func()) { p.afterChange.Store(&fn) }

func (p *Progress) changed() {
	if fn := p.afterChange.Load(); fn != nil {
		go (*fn)()
	}
}

func (p *Progress) Running() bool { return p.running.Load() }

// Begin claims the scan slot for a caller that will run Scan in the
// background. It returns false when a scan is already running, so two
// requests cannot start two scans.
func (p *Progress) Begin() bool          { return p.running.CompareAndSwap(false, true) }
func (p *Progress) Count() int64         { return p.count.Load() }
func (p *Progress) WatchFailures() int64 { return p.watchFailures.Load() }

// Stats summarises a completed scan run.
type Stats struct {
	Scanned  int
	Inserted int
	Updated  int
	Skipped  int
	Pruned   int
	// Failed counts files that are no audio or that a parser failed on,
	// including those recorded by an earlier run (#219).
	Failed int
}

type processResult int

const (
	resultInserted processResult = iota
	resultUpdated
	resultSkipped
	// resultFailed is a file recorded in scan_failures (#219).
	resultFailed
)

// Scan indexes every audio file under each root directory. A file reached
// through two roots (nested roots, or a symlink) is indexed once.
//
// Songs are pruned per root: a song is removed only if its root was read
// completely and its file was not seen. A root that cannot be read blocks
// pruning for its own songs only, so one unavailable disk does not cost the
// rest of the library its entries.
func Scan(pool *sql.DB, roots []string, coversDir string, songTags []string, progress *Progress) (Stats, error) {
	progress.running.Store(true)
	progress.count.Store(0)
	defer progress.changed() // runs after the slot is released
	defer progress.running.Store(false)

	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		return Stats{}, fmt.Errorf("creating covers dir %s: %w", coversDir, err)
	}

	var stats Stats
	seen := make(map[string]bool)
	visited := make(map[string]bool)
	var completeRoots []string
	for _, root := range roots {
		failures := walkAudio(roots, root, visited, func(path string) {
			seen[path] = true
			stats.Scanned++
			progress.count.Store(int64(stats.Scanned))
			switch res, perr := processFile(pool, roots, path, coversDir, songTags); {
			case res == resultFailed:
				if perr != nil {
					logging.Warnf("scan %s: %v", path, perr)
				}
				stats.Failed++
			case perr != nil:
				logging.Warnf("scan %s: %v", path, perr)
				stats.Skipped++
			case res == resultInserted:
				stats.Inserted++
				progress.songChanged(SongID(path))
			case res == resultUpdated:
				stats.Updated++
				progress.songChanged(SongID(path))
			case res == resultSkipped:
				stats.Skipped++
			}
		})
		if failures == 0 {
			completeRoots = append(completeRoots, root)
		} else {
			logging.Warnf("scan: %s: %d unreadable directories, its missing songs are kept", root, failures)
		}
	}
	// Songs whose files vanished while the server was down are removed only
	// under roots that were read completely.
	pruned, err := pruneMissing(pool, completeRoots, seen, progress)
	if err != nil {
		return stats, fmt.Errorf("pruning missing songs: %w", err)
	}
	stats.Pruned = pruned
	if err := pruneFailures(pool, completeRoots, seen); err != nil {
		logging.Errorf("scan: pruning recorded failures: %v", err)
	}
	if _, err := pool.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		LastScanKey, time.Now().UTC().Format("2006-01-02T15:04:05Z")); err != nil {
		logging.Errorf("scan: recording scan time: %v", err)
	}
	logging.Infof("scan complete: %d scanned, %d inserted, %d updated, %d skipped, %d removed, %d not readable",
		stats.Scanned, stats.Inserted, stats.Updated, stats.Skipped, stats.Pruned, stats.Failed)
	return stats, nil
}

// pruneMissing removes songs under root whose paths were not seen during the
// walk, along with any album or artist left empty by the removal.
func pruneMissing(pool *sql.DB, roots []string, seen map[string]bool, progress *Progress) (int, error) {
	n := 0
	for _, root := range roots {
		paths, err := songPathsUnder(pool, root)
		if err != nil {
			return n, err
		}
		for _, p := range paths {
			if seen[p] {
				continue
			}
			id, err := DeletePath(pool, p)
			if err != nil {
				logging.Errorf("prune %s: %v", p, err)
				continue
			}
			progress.songChanged(id)
			n++
		}
	}
	return n, nil
}

// walkAudio recursively walks dir, following symlinks for both files and
// directories as long as they stay inside the music folder (Contained,
// #177); a link that leads outside is skipped with a warning. Cycles are
// broken by tracking the canonical (symlink-resolved) path of every
// directory we descend into. It returns how many directories could not be
// read; a non-zero count means the walk is incomplete.
func walkAudio(roots []string, dir string, visited map[string]bool, fn func(path string)) int {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 1
	}
	if visited[real] {
		return 0
	}
	if !Contained(roots, dir) {
		logging.Warnf("scan: skipping %s: it leads outside the music folders", dir)
		return 0
	}
	visited[real] = true
	logging.Debugf("scan: walking %s", dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}
	failures := 0
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		info, err := os.Stat(full)
		if err != nil {
			// Usually a dangling symlink, which is not a read failure.
			continue
		}
		switch {
		case info.IsDir():
			failures += walkAudio(roots, full, visited, fn)
		case info.Mode().IsRegular() && hasAudioExt(full):
			// The directory is inside; only a file link can lead out.
			if entry.Type()&os.ModeSymlink != 0 && !Contained(roots, full) {
				logging.Warnf("scan: skipping %s: it links outside the music folders", full)
				continue
			}
			fn(full)
		}
	}
	return failures
}

func hasAudioExt(path string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	return audioExts[ext]
}

type extracted struct {
	Title         string
	Artist        string
	AlbumArtist   string
	Album         string
	Genre         sql.NullString
	Year          int64
	Track         sql.NullInt64
	Disc          sql.NullInt64
	DurationMs    int64
	Bitrate       int64
	Suffix        string
	ContentType   string
	CoverFilename sql.NullString
	Tags          sql.NullString
	TrackMBID     sql.NullString
	AlbumMBID     sql.NullString
	ArtistMBID    sql.NullString
	Lyrics        sql.NullString
	Audio         audioTags
}

func processFile(pool *sql.DB, roots []string, path, coversDir string, songTags []string) (processResult, error) {
	// The watcher hands in paths the walk never saw (#177).
	if !Contained(roots, path) {
		return 0, fmt.Errorf("%s: %w", path, ErrOutsideMusic)
	}
	meta, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	mtime := meta.ModTime().Unix()
	filesize := meta.Size()

	var existingMtime, oldSize, oldMtimeMs sql.NullInt64
	err = pool.QueryRow("SELECT mtime, file_size, file_mtime_ms FROM songs WHERE path = ?", path).
		Scan(&existingMtime, &oldSize, &oldMtimeMs)
	switch {
	case err == nil:
		// Unchanged only if the second-level mtime, and the size and
		// millisecond mtime where they are recorded, all match: a same-size
		// rewrite within the same second is still read again (#198).
		if existingMtime.Valid && existingMtime.Int64 == mtime &&
			(!oldSize.Valid || oldSize.Int64 == filesize) &&
			(!oldMtimeMs.Valid || oldMtimeMs.Int64 == meta.ModTime().UnixMilli()) {
			return resultSkipped, nil
		}
	case errors.Is(err, sql.ErrNoRows):
		// new file
	default:
		return 0, err
	}

	// The audio counts as changed unless size and mtime (ms) are known and
	// equal. Only then are analysis results kept: a re-read forced by a
	// migration (mtime = 0) or a tag-only edit that keeps both does not cost
	// the track its analysis. An unknown fingerprint (songs added before it
	// was recorded) also counts as changed; such songs have no results yet.
	mtimeMs := meta.ModTime().UnixMilli()
	audioChanged := !(oldSize.Valid && oldMtimeMs.Valid && oldSize.Int64 == filesize && oldMtimeMs.Int64 == mtimeMs)

	// A file that failed before is retried only once it changed (#219).
	if known, err := knownFailure(pool, path, filesize, mtimeMs); err != nil {
		return 0, err
	} else if known {
		return resultFailed, nil
	}
	// Its content must be audio, whatever its name says, before a parser
	// sees it; audio under another format's extension is indexed as what it
	// is (#219).
	kind, err := sniffFile(path)
	if err != nil {
		return 0, err
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	suffix, isAudio, mismatch := resolveSuffix(ext, kind)
	if !isAudio {
		recordFailure(pool, path, filesize, mtimeMs, ErrNotAudio)
		return resultFailed, fmt.Errorf("%w: its content is no audio format", ErrNotAudio)
	}
	if mismatch {
		logging.Warnf("scan %s: named .%s but holds %s; indexed as %s", path, ext, kind, suffix)
	}

	ex, err := safeExtract(roots, path, filesize, coversDir, songTags, suffix)
	if errors.Is(err, errParserPanic) {
		recordFailure(pool, path, filesize, mtimeMs, err)
		return resultFailed, err
	}
	if err != nil {
		return 0, err
	}

	displayArtist := ex.AlbumArtist
	if displayArtist == "" {
		displayArtist = ex.Artist
	}
	artistID := ArtistID(displayArtist)
	albumID := AlbumID(displayArtist, ex.Album, ex.Year)
	songID := SongID(path)

	tx, err := pool.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO artists (id, name, name_lower, mbid) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name_lower) DO UPDATE SET
		   name = excluded.name,
		   mbid = COALESCE(excluded.mbid, artists.mbid)`,
		artistID, displayArtist, strings.ToLower(displayArtist), ex.ArtistMBID,
	); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(
		`INSERT INTO albums (id, title, artist, artist_id, year, cover_art, mbid)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = excluded.title,
		   artist = excluded.artist,
		   artist_id = excluded.artist_id,
		   year = excluded.year,
		   cover_art = COALESCE(excluded.cover_art, albums.cover_art),
		   mbid = COALESCE(excluded.mbid, albums.mbid)`,
		albumID, ex.Album, displayArtist, artistID, ex.Year, ex.CoverFilename, ex.AlbumMBID,
	); err != nil {
		return 0, err
	}

	var existedCount int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM songs WHERE id = ?", songID).Scan(&existedCount); err != nil {
		return 0, err
	}
	existed := existedCount > 0

	yearOpt := sql.NullInt64{}
	if ex.Year > 0 {
		yearOpt = sql.NullInt64{Int64: ex.Year, Valid: true}
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	au := ex.Audio
	var bpmSource, bpmAt sql.NullString
	if au.BPM.Valid {
		bpmSource = sql.NullString{String: "tag", Valid: true}
		bpmAt = sql.NullString{String: now, Valid: true}
	}
	if _, err := tx.Exec(
		`INSERT INTO songs
		   (id, path, title, artist, artist_id, album, album_id, genre, track_number,
		    disc_number, year, duration_ms, bitrate, filesize, suffix, content_type, cover_art, mtime, mbid, tags, created_at,
		    rg_track_gain, rg_album_gain, rg_track_peak, rg_album_peak, rg_base_gain, meter,
		    bpm, bpm_source, bpm_at, file_size, file_mtime_ms)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   path = excluded.path,
		   title = excluded.title,
		   artist = excluded.artist,
		   artist_id = excluded.artist_id,
		   album = excluded.album,
		   album_id = excluded.album_id,
		   genre = excluded.genre,
		   track_number = excluded.track_number,
		   disc_number = excluded.disc_number,
		   year = excluded.year,
		   duration_ms = excluded.duration_ms,
		   bitrate = excluded.bitrate,
		   filesize = excluded.filesize,
		   suffix = excluded.suffix,
		   content_type = excluded.content_type,
		   cover_art = COALESCE(excluded.cover_art, songs.cover_art),
		   mtime = excluded.mtime,
		   mbid = COALESCE(excluded.mbid, songs.mbid),
		   tags = excluded.tags,
		   created_at = COALESCE(songs.created_at, excluded.created_at),
		   rg_track_gain = excluded.rg_track_gain,
		   rg_album_gain = excluded.rg_album_gain,
		   rg_track_peak = excluded.rg_track_peak,
		   rg_album_peak = excluded.rg_album_peak,
		   rg_base_gain = excluded.rg_base_gain,
		   meter = excluded.meter,
		   file_size = excluded.file_size,
		   file_mtime_ms = excluded.file_mtime_ms,
		   -- A BPM from an assigned analysis tool (bpm_source is the tool's
		   -- name) is kept while the audio is unchanged (?33 is 1 when it
		   -- changed); the tool runs again otherwise. Apart from that a BPM tag
		   -- always wins. A calculated BPM survives a re-read while the audio
		   -- length is unchanged (tag edits, forced re-reads), and is cleared
		   -- when the recording changed.
		   bpm = CASE WHEN ?33 = 0 AND songs.bpm_source NOT IN ('tag', 'calc') THEN songs.bpm
		              WHEN excluded.bpm IS NOT NULL THEN excluded.bpm
		              WHEN songs.bpm_source = 'calc' AND songs.duration_ms = excluded.duration_ms THEN songs.bpm END,
		   bpm_source = CASE WHEN ?33 = 0 AND songs.bpm_source NOT IN ('tag', 'calc') THEN songs.bpm_source
		              WHEN excluded.bpm IS NOT NULL THEN 'tag'
		              WHEN songs.bpm_source = 'calc' AND songs.duration_ms = excluded.duration_ms THEN 'calc' END,
		   bpm_confidence = CASE WHEN ?33 = 0 AND songs.bpm_source NOT IN ('tag', 'calc') THEN songs.bpm_confidence
		              WHEN excluded.bpm IS NULL AND songs.bpm_source = 'calc'
		              AND songs.duration_ms = excluded.duration_ms THEN songs.bpm_confidence END,
		   bpm_at = CASE WHEN ?33 = 0 AND songs.bpm_source NOT IN ('tag', 'calc') THEN songs.bpm_at
		              WHEN excluded.bpm IS NOT NULL THEN
		                   CASE WHEN songs.bpm_source = 'tag' AND songs.bpm = excluded.bpm THEN songs.bpm_at ELSE excluded.bpm_at END
		              WHEN songs.bpm_source = 'calc' AND songs.duration_ms = excluded.duration_ms THEN songs.bpm_at END`,
		songID, path, ex.Title, ex.Artist, artistID, ex.Album, albumID,
		ex.Genre, ex.Track, ex.Disc, yearOpt, ex.DurationMs, ex.Bitrate,
		filesize, ex.Suffix, ex.ContentType, ex.CoverFilename, mtime, ex.TrackMBID, ex.Tags,
		now,
		au.TrackGain, au.AlbumGain, au.TrackPeak, au.AlbumPeak, au.BaseGain, au.Meter,
		au.BPM, bpmSource, bpmAt, filesize, mtimeMs, boolInt(audioChanged),
	); err != nil {
		return 0, err
	}
	if err := storeTagValues(tx, songID, au, existed && audioChanged); err != nil {
		return 0, err
	}
	if err := storeLyrics(tx, songID, ex.Lyrics); err != nil {
		return 0, err
	}
	if err := markModified(tx); err != nil {
		return 0, err
	}
	// Readable now, so a failure recorded for an earlier version is done.
	if _, err := tx.Exec("DELETE FROM scan_failures WHERE path = ?", path); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if existed {
		return resultUpdated, nil
	}
	return resultInserted, nil
}

// extractMetadata reads tags, duration and cover of the file at path, which
// holds audio of the given suffix (from sniffFormat, not only the name).
// Call it through safeExtract.
func extractMetadata(roots []string, path string, filesize int64, coversDir string, songTags []string, suffix string) (extracted, error) {
	ex := extracted{
		Title:       fileStem(path),
		Artist:      "Unknown Artist",
		Album:       "Unknown Album",
		Suffix:      suffix,
		ContentType: mimeForSuffix(suffix),
	}

	f, err := os.Open(path)
	if err != nil {
		return ex, err
	}
	defer f.Close()

	m, terr := readTags(&limitedReadSeeker{rs: f, left: maxTagBytes})
	if errors.Is(terr, errTagTooLarge) {
		logging.Warnf("scan %s: tags over %d bytes are ignored", path, maxTagBytes)
	}
	if terr == nil {
		if v := m.Title(); v != "" {
			ex.Title = v
		}
		if v := m.Artist(); v != "" {
			ex.Artist = v
		}
		if v := m.AlbumArtist(); v != "" {
			ex.AlbumArtist = v
		}
		if v := m.Album(); v != "" {
			ex.Album = v
		}
		if v := m.Genre(); v != "" {
			ex.Genre = sql.NullString{String: v, Valid: true}
		}
		if y := m.Year(); y > 0 {
			ex.Year = int64(y)
		}
		if tn, _ := m.Track(); tn > 0 {
			ex.Track = sql.NullInt64{Int64: int64(tn), Valid: true}
		}
		if dn, _ := m.Disc(); dn > 0 {
			ex.Disc = sql.NullInt64{Int64: int64(dn), Valid: true}
		}
		raw := m.Raw()
		ex.TrackMBID = readMBID(raw, trackMBID)
		ex.AlbumMBID = readMBID(raw, albumMBID)
		ex.ArtistMBID = readMBID(raw, artistMBID)
		ex.Lyrics = lyricsValue(m.Lyrics())
	}
	var meta tag.Metadata
	if terr == nil {
		meta = m
	}
	if len(songTags) > 0 {
		ex.Tags = encodeSongTags(songTagValues(path, suffix, meta, allowMap(songTags)))
	}
	ex.Audio = readAudioTags(path, suffix, meta)

	info := audioinfo.Probe(path, suffix, filesize)
	ex.DurationMs = info.DurationMs
	ex.Bitrate = info.BitrateKbps

	// Cover art — prefer embedded, fall back to a sibling cover.jpg.
	albumArtistForKey := ex.AlbumArtist
	if albumArtistForKey == "" {
		albumArtistForKey = ex.Artist
	}
	albumKey := AlbumID(albumArtistForKey, ex.Album, ex.Year)

	if terr == nil {
		if pic := m.Picture(); pic != nil && len(pic.Data) > 0 {
			if name, err := savePicture(coversDir, albumKey, pic); err == nil {
				ex.CoverFilename = sql.NullString{String: name, Valid: true}
			}
		}
	}
	if !ex.CoverFilename.Valid {
		if name, ok := findDirCover(roots, path, coversDir, albumKey); ok {
			ex.CoverFilename = sql.NullString{String: name, Valid: true}
		}
	}
	return ex, nil
}

// maxCoverBytes bounds a cover image, embedded or in the folder (#198).
const maxCoverBytes = 16 << 20

// errNotImage is a cover that is too large or not an image format we serve.
var errNotImage = errors.New("not a cover image")

// imageExt is the file extension for an image, from its first bytes; the
// tag's claimed type is not trusted (#198).
func imageExt(head []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg", true
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "png", true
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "gif", true
	case bytes.HasPrefix(head, []byte("BM")):
		return "bmp", true
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		return "tiff", true
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "webp", true
	}
	return "", false
}

// savePicture stores an embedded picture as the album's cover. It is
// rewritten each time, since this runs only for files that changed, so the
// cover follows the newest embedded artwork.
func savePicture(coversDir, albumKey string, pic *tag.Picture) (string, error) {
	if len(pic.Data) > maxCoverBytes {
		return "", fmt.Errorf("%w: %d bytes", errNotImage, len(pic.Data))
	}
	ext, ok := imageExt(pic.Data)
	if !ok {
		return "", errNotImage
	}
	filename := albumKey + "." + ext
	if err := writeCover(filepath.Join(coversDir, filename), bytes.NewReader(pic.Data)); err != nil {
		return "", err
	}
	return filename, nil
}

// writeCover writes at most maxCoverBytes from r to dst through a temporary
// file and a rename, so a reader never sees half a cover (#198).
func writeCover(dst string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".cover-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op after the rename
	n, err := io.Copy(tmp, io.LimitReader(r, maxCoverBytes+1))
	if err == nil && n > maxCoverBytes {
		err = fmt.Errorf("%w: over %d bytes", errNotImage, maxCoverBytes)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

var coverCandidates = []string{
	"cover.jpg", "cover.jpeg", "cover.png",
	"folder.jpg", "folder.jpeg", "folder.png",
	"front.jpg", "front.png",
	"album.jpg", "album.png",
}

// findDirCover copies a cover image from the song's folder. It is opened
// through the music folder's os.Root, so a cover.jpg linking outside it is
// ignored (#177).
func findDirCover(roots []string, audioPath, coversDir, albumKey string) (string, bool) {
	dir := filepath.Dir(audioPath)
	for _, cand := range coverCandidates {
		p := filepath.Join(dir, cand)
		// Regular files only: a FIFO named cover.jpg would block the scan
		// on open (#198). An in-folder symlink to a regular file is fine.
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() > maxCoverBytes {
			continue
		}
		if filename, err := copyCover(roots, p, coversDir, albumKey); err == nil {
			return filename, true
		}
	}
	return "", false
}

// copyCover copies a folder cover into coversDir, named for its real image
// format, through OpenContained (#177) and writeCover (#198).
func copyCover(roots []string, src, coversDir, albumKey string) (string, error) {
	in, err := OpenContained(roots, src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if st, err := in.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", errNotImage
	}
	br := bufio.NewReader(in)
	head, _ := br.Peek(12)
	ext, ok := imageExt(head)
	if !ok {
		return "", errNotImage
	}
	filename := albumKey + "." + ext
	if err := writeCover(filepath.Join(coversDir, filename), br); err != nil {
		return "", err
	}
	return filename, nil
}

func fileStem(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem == "" {
		return "Unknown Title"
	}
	return stem
}

func mimeForSuffix(suffix string) string {
	switch suffix {
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "ogg":
		return "audio/ogg"
	case "m4a", "aac", "mp4", "alac":
		return "audio/mp4"
	case "wav":
		return "audio/wav"
	case "wma":
		return "audio/x-ms-wma"
	case "opus":
		return "audio/opus"
	case "aiff", "aif":
		return "audio/aiff"
	case "wv":
		return "audio/x-wavpack"
	case "mpc":
		return "audio/x-musepack"
	case "ape":
		return "audio/x-ape"
	}
	return "application/octet-stream"
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
