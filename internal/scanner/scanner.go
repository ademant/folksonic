package scanner

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dhowden/tag"

	"github.com/tsirysndr/tinysonic/internal/audioinfo"
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
}

type processResult int

const (
	resultInserted processResult = iota
	resultUpdated
	resultSkipped
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
	defer progress.running.Store(false)

	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		return Stats{}, fmt.Errorf("creating covers dir %s: %w", coversDir, err)
	}

	var stats Stats
	seen := make(map[string]bool)
	visited := make(map[string]bool)
	var completeRoots []string
	for _, root := range roots {
		failures := walkAudio(root, visited, func(path string) {
			seen[path] = true
			stats.Scanned++
			progress.count.Store(int64(stats.Scanned))
			switch res, perr := processFile(pool, path, coversDir, songTags); {
			case perr != nil:
				log.Printf("scan %s: %v", path, perr)
				stats.Skipped++
			case res == resultInserted:
				stats.Inserted++
			case res == resultUpdated:
				stats.Updated++
			case res == resultSkipped:
				stats.Skipped++
			}
		})
		if failures == 0 {
			completeRoots = append(completeRoots, root)
		} else {
			log.Printf("scan: %s: %d unreadable directories, its missing songs are kept", root, failures)
		}
	}
	// Songs whose files vanished while the server was down are removed only
	// under roots that were read completely.
	pruned, err := pruneMissing(pool, completeRoots, seen)
	if err != nil {
		return stats, fmt.Errorf("pruning missing songs: %w", err)
	}
	stats.Pruned = pruned
	if _, err := pool.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		LastScanKey, time.Now().UTC().Format("2006-01-02T15:04:05Z")); err != nil {
		log.Printf("scan: recording scan time: %v", err)
	}
	log.Printf("scan complete: %d scanned, %d inserted, %d updated, %d skipped, %d removed",
		stats.Scanned, stats.Inserted, stats.Updated, stats.Skipped, stats.Pruned)
	return stats, nil
}

// pruneMissing removes songs under root whose paths were not seen during the
// walk, along with any album or artist left empty by the removal.
func pruneMissing(pool *sql.DB, roots []string, seen map[string]bool) (int, error) {
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
			if err := DeletePath(pool, p); err != nil {
				log.Printf("prune %s: %v", p, err)
				continue
			}
			n++
		}
	}
	return n, nil
}

// walkAudio recursively walks dir, following symlinks for both files and
// directories. Cycles are broken by tracking the canonical (symlink-resolved)
// path of every directory we descend into. It returns how many directories
// could not be read; a non-zero count means the walk is incomplete.
func walkAudio(dir string, visited map[string]bool, fn func(path string)) int {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 1
	}
	if visited[real] {
		return 0
	}
	visited[real] = true
	log.Printf("scan: walking %s", dir)

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
			failures += walkAudio(full, visited, fn)
		case info.Mode().IsRegular() && hasAudioExt(full):
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
	Audio         audioTags
}

func processFile(pool *sql.DB, path, coversDir string, songTags []string) (processResult, error) {
	meta, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	mtime := meta.ModTime().Unix()
	filesize := meta.Size()

	var existingMtime sql.NullInt64
	err = pool.QueryRow("SELECT mtime FROM songs WHERE path = ?", path).Scan(&existingMtime)
	switch {
	case err == nil:
		if existingMtime.Valid && existingMtime.Int64 == mtime {
			return resultSkipped, nil
		}
	case errors.Is(err, sql.ErrNoRows):
		// new file
	default:
		return 0, err
	}

	ex, err := extractMetadata(path, filesize, coversDir, songTags)
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
		    bpm, bpm_source, bpm_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
		   -- A BPM tag always wins. A calculated BPM survives a re-read while
		   -- the audio length is unchanged (tag edits, forced re-reads), and
		   -- is cleared when the recording changed.
		   bpm = CASE WHEN excluded.bpm IS NOT NULL THEN excluded.bpm
		              WHEN songs.bpm_source = 'calc' AND songs.duration_ms = excluded.duration_ms THEN songs.bpm END,
		   bpm_source = CASE WHEN excluded.bpm IS NOT NULL THEN 'tag'
		              WHEN songs.bpm_source = 'calc' AND songs.duration_ms = excluded.duration_ms THEN 'calc' END,
		   bpm_confidence = CASE WHEN excluded.bpm IS NULL AND songs.bpm_source = 'calc'
		              AND songs.duration_ms = excluded.duration_ms THEN songs.bpm_confidence END,
		   bpm_at = CASE WHEN excluded.bpm IS NOT NULL THEN
		                   CASE WHEN songs.bpm_source = 'tag' AND songs.bpm = excluded.bpm THEN songs.bpm_at ELSE excluded.bpm_at END
		              WHEN songs.bpm_source = 'calc' AND songs.duration_ms = excluded.duration_ms THEN songs.bpm_at END`,
		songID, path, ex.Title, ex.Artist, artistID, ex.Album, albumID,
		ex.Genre, ex.Track, ex.Disc, yearOpt, ex.DurationMs, ex.Bitrate,
		filesize, ex.Suffix, ex.ContentType, ex.CoverFilename, mtime, ex.TrackMBID, ex.Tags,
		now,
		au.TrackGain, au.AlbumGain, au.TrackPeak, au.AlbumPeak, au.BaseGain, au.Meter,
		au.BPM, bpmSource, bpmAt,
	); err != nil {
		return 0, err
	}
	if err := markModified(tx); err != nil {
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

func extractMetadata(path string, filesize int64, coversDir string, songTags []string) (extracted, error) {
	suffix := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
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

	m, terr := tag.ReadFrom(f)
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
		if name, ok := findDirCover(path, coversDir, albumKey); ok {
			ex.CoverFilename = sql.NullString{String: name, Valid: true}
		}
	}
	return ex, nil
}

func savePicture(coversDir, albumKey string, pic *tag.Picture) (string, error) {
	ext := "jpg"
	switch strings.ToLower(pic.Ext) {
	case "jpg", "jpeg":
		ext = "jpg"
	case "png":
		ext = "png"
	case "gif":
		ext = "gif"
	case "bmp":
		ext = "bmp"
	case "tiff":
		ext = "tiff"
	default:
		// dhowden/tag also exposes pic.MIMEType — handle that path.
		switch strings.ToLower(pic.MIMEType) {
		case "image/jpeg", "image/jpg":
			ext = "jpg"
		case "image/png":
			ext = "png"
		case "image/gif":
			ext = "gif"
		case "image/bmp":
			ext = "bmp"
		case "image/tiff":
			ext = "tiff"
		}
	}
	filename := albumKey + "." + ext
	full := filepath.Join(coversDir, filename)
	// Always rewrite: this runs only for files whose mtime changed, so the
	// cover follows the newest embedded artwork instead of a stale cache entry.
	if err := os.WriteFile(full, pic.Data, 0o644); err != nil {
		return "", err
	}
	return filename, nil
}

var coverCandidates = []string{
	"cover.jpg", "cover.jpeg", "cover.png",
	"folder.jpg", "folder.jpeg", "folder.png",
	"front.jpg", "front.png",
	"album.jpg", "album.png",
}

func findDirCover(audioPath, coversDir, albumKey string) (string, bool) {
	dir := filepath.Dir(audioPath)
	for _, cand := range coverCandidates {
		p := filepath.Join(dir, cand)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(cand), "."))
		filename := albumKey + "." + ext
		dest := filepath.Join(coversDir, filename)
		if err := copyFile(p, dest); err != nil {
			continue
		}
		return filename, true
	}
	return "", false
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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
