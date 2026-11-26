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
	running atomic.Bool
	count   atomic.Int64
}

func (p *Progress) Running() bool { return p.running.Load() }
func (p *Progress) Count() int64  { return p.count.Load() }

// Stats summarises a completed scan run.
type Stats struct {
	Scanned  int
	Inserted int
	Updated  int
	Skipped  int
}

type processResult int

const (
	resultInserted processResult = iota
	resultUpdated
	resultSkipped
)

func Scan(pool *sql.DB, musicDir, coversDir string, progress *Progress) (Stats, error) {
	progress.running.Store(true)
	progress.count.Store(0)
	defer progress.running.Store(false)

	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		return Stats{}, fmt.Errorf("creating covers dir %s: %w", coversDir, err)
	}

	var stats Stats
	visited := make(map[string]bool)
	walkAudio(musicDir, musicDir, visited, func(path string) {
		stats.Scanned++
		progress.count.Store(int64(stats.Scanned))
		switch res, perr := processFile(pool, musicDir, path, coversDir); {
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
	log.Printf("scan complete: %d scanned, %d inserted, %d updated, %d skipped",
		stats.Scanned, stats.Inserted, stats.Updated, stats.Skipped)
	return stats, nil
}

// walkAudio recursively walks dir, following symlinks for both files and
// directories. Cycles are broken by tracking the canonical (symlink-resolved)
// path of every directory we descend into.
func walkAudio(root, dir string, visited map[string]bool, fn func(path string)) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return
	}
	if visited[real] {
		return
	}
	if !Contained(root, dir) {
		log.Printf("scan: skipping %s: it leads outside the music folder", dir)
		return
	}
	visited[real] = true
	log.Printf("scan: walking %s", dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		switch {
		case info.IsDir():
			walkAudio(root, full, visited, fn)
		case info.Mode().IsRegular() && hasAudioExt(full):
			if entry.Type()&os.ModeSymlink != 0 && !Contained(root, full) {
				log.Printf("scan: skipping %s: it links outside the music folder", full)
				continue
			}
			fn(full)
		}
	}
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
}

func processFile(pool *sql.DB, root, path, coversDir string) (processResult, error) {
	if !Contained(root, path) {
		return 0, fmt.Errorf("%s: %w", path, ErrOutsideMusic)
	}
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

	ex, err := extractMetadata(root, path, filesize, coversDir)
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
		`INSERT INTO artists (id, name, name_lower) VALUES (?, ?, ?)
		 ON CONFLICT(name_lower) DO UPDATE SET name = excluded.name`,
		artistID, displayArtist, strings.ToLower(displayArtist),
	); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(
		`INSERT INTO albums (id, title, artist, artist_id, year, cover_art)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = excluded.title,
		   artist = excluded.artist,
		   artist_id = excluded.artist_id,
		   year = excluded.year,
		   cover_art = COALESCE(excluded.cover_art, albums.cover_art)`,
		albumID, ex.Album, displayArtist, artistID, ex.Year, ex.CoverFilename,
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

	if _, err := tx.Exec(
		`INSERT INTO songs
		   (id, path, title, artist, artist_id, album, album_id, genre, track_number,
		    disc_number, year, duration_ms, bitrate, filesize, suffix, content_type, cover_art, mtime)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
		   mtime = excluded.mtime`,
		songID, path, ex.Title, ex.Artist, artistID, ex.Album, albumID,
		ex.Genre, ex.Track, ex.Disc, yearOpt, ex.DurationMs, ex.Bitrate,
		filesize, ex.Suffix, ex.ContentType, ex.CoverFilename, mtime,
	); err != nil {
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

func extractMetadata(root, path string, filesize int64, coversDir string) (extracted, error) {
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
	}

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
		if name, ok := findDirCover(root, path, coversDir, albumKey); ok {
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
	if _, err := os.Stat(full); err == nil {
		return filename, nil
	}
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

func findDirCover(root, audioPath, coversDir, albumKey string) (string, bool) {
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
		if _, err := os.Stat(dest); err == nil {
			return filename, true
		}
		if err := copyFile(root, p, dest); err != nil {
			continue
		}
		return filename, true
	}
	return "", false
}

func copyFile(root, src, dst string) error {
	in, err := OpenContained(root, src)
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
