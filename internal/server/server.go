package server

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/scanner"
)

// State is shared by all handlers.
type State struct {
	Pool      *sql.DB
	Username  string
	Password  string
	MusicDir  string
	CoversDir string
	Progress  *scanner.Progress

	// songCache maps a song id to its (path, content_type). Populated lazily
	// on cache miss and warmed in the background at startup. Bypassing the DB
	// for streams is what keeps first-byte latency under client timeouts on
	// slow hardware (WASM SQLite on armv6 is the dominant cost otherwise).
	songCache sync.Map
}

type songRef struct {
	Path        string
	ContentType string
}

func (s *State) lookupSong(ctx context.Context, id string) (path, contentType string, found bool, err error) {
	if v, ok := s.songCache.Load(id); ok {
		r := v.(songRef)
		return r.Path, r.ContentType, true, nil
	}
	song, err := repoFindSong(ctx, s.Pool, id)
	if err != nil {
		return "", "", false, err
	}
	if song == nil {
		return "", "", false, nil
	}
	s.songCache.Store(id, songRef{Path: song.Path, ContentType: song.ContentType})
	return song.Path, song.ContentType, true, nil
}

func (s *State) evictSong(id string) { s.songCache.Delete(id) }

func (s *State) preloadSongs(ctx context.Context) {
	rows, err := s.Pool.QueryContext(ctx, "SELECT id, path, content_type FROM songs")
	if err != nil {
		log.Printf("song cache preload: %v", err)
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, path, ct string
		if err := rows.Scan(&id, &path, &ct); err != nil {
			continue
		}
		s.songCache.Store(id, songRef{Path: path, ContentType: ct})
		n++
	}
	log.Printf("song cache: %d entries preloaded", n)
}

func Start(ctx context.Context, cfg *config.Config, pool *sql.DB, progress *scanner.Progress) error {
	s := &State{
		Pool:      pool,
		Username:  cfg.Username,
		Password:  cfg.Password,
		MusicDir:  cfg.MusicDir,
		CoversDir: cfg.CoversDir,
		Progress:  progress,
	}

	go s.preloadSongs(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)

	register := func(name string, h http.HandlerFunc) {
		mux.HandleFunc("/rest/"+name, h)
		mux.HandleFunc("/rest/"+name+".view", h)
	}

	// System
	register("ping", s.Ping)
	register("getUser", s.GetUser)
	register("getMusicFolders", s.GetMusicFolders)
	register("getScanStatus", s.GetScanStatus)
	register("startScan", s.StartScan)

	// Library — tag browsing
	register("getArtists", s.GetArtists)
	register("getArtist", s.GetArtist)
	register("getAlbum", s.GetAlbum)
	register("getSong", s.GetSong)
	register("getAlbumList2", s.GetAlbumList2)
	register("getAlbumList", s.GetAlbumList)

	// Folder browsing
	register("getIndexes", s.GetIndexes)
	register("getMusicDirectory", s.GetMusicDirectory)

	// Genres
	register("getGenres", s.GetGenres)
	register("getSongsByGenre", s.GetSongsByGenre)

	// Lists
	register("getRandomSongs", s.GetRandomSongs)
	register("getStarred", s.GetStarred)
	register("getStarred2", s.GetStarred2)

	// Playback
	register("stream", s.Stream)
	register("download", s.Stream)
	register("getCoverArt", s.GetCoverArt)
	register("scrobble", s.Scrobble)
	register("getNowPlaying", s.GetNowPlaying)
	register("updateNowPlaying", s.UpdateNowPlaying)

	// Search
	register("search3", s.Search3)
	register("search2", s.Search2)

	// Playlists
	register("getPlaylists", s.GetPlaylists)
	register("getPlaylist", s.GetPlaylist)
	register("createPlaylist", s.CreatePlaylist)
	register("updatePlaylist", s.UpdatePlaylist)
	register("deletePlaylist", s.DeletePlaylist)

	// Starring
	register("star", s.Star)
	register("unstar", s.Unstar)

	// Artist / album info stubs
	register("getArtistInfo", s.GetArtistInfo)
	register("getArtistInfo2", s.GetArtistInfo2)
	register("getAlbumInfo", s.GetAlbumInfo)
	register("getAlbumInfo2", s.GetAlbumInfo)
	register("getSimilarSongs", s.GetSimilarSongs)
	register("getSimilarSongs2", s.GetSimilarSongs2)
	register("getTopSongs", s.GetTopSongs)
	register("getLyrics", s.GetLyrics)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           requestLogger(corsMiddleware(limitBody(mux))),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// corsMiddleware allows any origin, method and header. Credentials travel in
// the query string or form body, never in cookies, so the response does not
// need Access-Control-Allow-Credentials. Reflecting the origin together with
// that header would let any site read responses for anyone who holds the
// credentials, which this avoids.
func corsMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// limitBody caps request bodies. Subsonic only needs small form posts (playlist
// edits), so anything larger is rejected before it is read into memory.
func limitBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h.ServeHTTP(w, r)
	})
}

// requestLogger emits one log line per request: method, path, status, bytes,
// duration. Wraps the inner handler so middleware ordering matters — keep this
// outermost so the recorded status reflects what the client sees.
func requestLogger(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &recordedResponse{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rw, r)
		// Path only: the query string carries p=, t= and s= credentials and
		// must never reach the log.
		log.Printf("%s %s %d %dB %s", r.Method, r.URL.Path, rw.status, rw.bytes, time.Since(start))
	})
}

type recordedResponse struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recordedResponse) WriteHeader(s int) {
	if r.wroteHeader {
		return
	}
	r.status = s
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(s)
}

func (r *recordedResponse) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Flush passes through to the underlying ResponseWriter for http.ServeContent
// streaming and chunked responses.
func (r *recordedResponse) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom delegates to the underlying ResponseWriter's ReadFrom when present,
// which lets net/http use sendfile(2) for stream/cover responses. Without this
// passthrough, io.Copy in http.ServeContent falls back to userspace Read/Write
// loops — a major CPU hit on slow hardware.
func (r *recordedResponse) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.bytes += n
		return n, err
	}
	n, err := io.Copy(writerOnly{r.ResponseWriter}, src)
	r.bytes += n
	return n, err
}

// writerOnly hides the ResponseWriter's ReadFrom so io.Copy doesn't recurse
// when we fall back to the non-sendfile path.
type writerOnly struct{ io.Writer }

// requireAuth returns true when the request authenticated; it has already
// written the error response when this returns false.
func (s *State) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	q := params(r)
	if !authCheck(s.Username, s.Password, q.Get("u"), q.Get("p"), q.Get("t"), q.Get("s")) {
		writeError(w, 40, "Wrong username or password")
		return false
	}
	return true
}

// queryFirst is a convenience for endpoints that take many params on the
// query string — both GET and POST are routed identically and the Subsonic
// spec puts params in the query string regardless of method.
func queryFirst(r *http.Request, key string) string {
	return strings.TrimSpace(params(r).Get(key))
}

// queryAll returns every value for the given key, in the order they appeared.
func queryAll(r *http.Request, key string) []string {
	return params(r)[key]
}

// params returns the request parameters from the query string and, for POST,
// the form-encoded body. Subsonic clients may send either, so every handler
// reads through here rather than r.URL.Query().
func params(r *http.Request) url.Values {
	_ = r.ParseForm() // on error, r.Form still holds the query-string values
	return r.Form
}
