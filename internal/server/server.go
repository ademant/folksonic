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

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/tsirysndr/tinysonic/internal/config"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// State is shared by all handlers.
type State struct {
	Pool      *sql.DB
	MusicDirs []string
	// SongTags is the allowlist of file tags exposed on songs (song_tags).
	SongTags  []string
	CoversDir string
	Progress  *scanner.Progress

	// BaseURL is the public address clients reach the server at; it is
	// embedded in pairing QR codes. Empty means derive it from the request.
	BaseURL string
	// AllowRegistration enables self-registration. New accounts stay pending
	// until an administrator approves them with `tinysonic user approve`.
	AllowRegistration bool

	webAuthn   *webauthn.WebAuthn
	ceremonies *ceremonyStore
	// sso is nil unless the oidc block is configured.
	sso *oidcService

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
		Pool:              pool,
		BaseURL:           cfg.BaseURL,
		AllowRegistration: cfg.AllowRegistration,
		MusicDirs:         cfg.MusicDirs,
		SongTags:          cfg.SongTags,
		CoversDir:         cfg.CoversDir,
		Progress:          progress,
	}

	go s.preloadSongs(ctx)

	// A bad base_url disables passkeys but must not stop the server: password
	// and API-key access keep working.
	wa, err := newWebAuthn(cfg.BaseURL)
	if err != nil {
		log.Printf("warning: passkeys disabled: %v", err)
	}
	s.webAuthn = wa
	s.ceremonies = &ceremonyStore{m: map[string]ceremony{}}
	if cfg.OIDC.Enabled() {
		s.sso = newOIDCService(cfg.OIDC, cfg.BaseURL)
	}

	handler := mountAt(cfg.BasePath, s.routes())

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           requestLogger(corsMiddleware(limitBody(handler))),
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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
// requireCaller is requireAuth for handlers that act on behalf of a specific
// account (stars, playlists). It returns that account.
func (s *State) requireCaller(w http.ResponseWriter, r *http.Request) (users.User, bool) {
	u, ok := s.authenticate(r)
	if !ok {
		writeError(w, 40, "Wrong username or password")
		return users.User{}, false
	}
	return u, true
}

func (s *State) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.authenticate(r); !ok {
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

// routes registers every endpoint at its root-relative path. Start mounts the
// result under base_path when one is set.
func (s *State) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)

	register := func(name string, h http.HandlerFunc) {
		mux.HandleFunc("/rest/"+name, h)
		mux.HandleFunc("/rest/"+name+".view", h)
	}

	// System
	register("ping", s.Ping)
	register("getOpenSubsonicExtensions", s.GetOpenSubsonicExtensions)
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

	// Account and device management (JSON, not Subsonic)
	mux.HandleFunc("POST /api/register", s.APIRegister)
	mux.HandleFunc("POST /api/keys", s.APICreateKey)
	mux.HandleFunc("GET /api/keys", s.APIListKeys)
	mux.HandleFunc("DELETE /api/keys/{id}", s.APIRevokeKey)
	mux.HandleFunc("POST /api/pair/start", s.APIPairStart)
	mux.HandleFunc("POST /api/pair/complete", s.APIPairComplete)
	mux.HandleFunc("POST /api/passkey/register/begin", s.APIPasskeyRegisterBegin)
	mux.HandleFunc("POST /api/passkey/register/finish", s.APIPasskeyRegisterFinish)
	mux.HandleFunc("POST /api/register/passkey/begin", s.APIRegisterPasskeyBegin)
	mux.HandleFunc("POST /api/register/passkey/finish", s.APIRegisterPasskeyFinish)
	mux.HandleFunc("POST /api/passkey/login/begin", s.APIPasskeyLoginBegin)
	mux.HandleFunc("POST /api/passkey/login/finish", s.APIPasskeyLoginFinish)
	mux.HandleFunc("POST /api/login", s.APILogin)
	mux.HandleFunc("POST /api/logout", s.APILogout)
	mux.HandleFunc("POST /api/pair/qr", s.APIPairQR)
	mux.HandleFunc("GET /api/passkeys", s.APIListPasskeys)
	mux.HandleFunc("DELETE /api/passkeys/{id}", s.APIRemovePasskey)
	mux.HandleFunc("GET /api/oidc/status", s.APIOIDCStatus)
	mux.HandleFunc("POST /api/oidc/begin", s.APIOIDCBegin)
	mux.HandleFunc("GET /api/oidc/callback", s.APIOIDCCallback)
	mux.HandleFunc("POST /api/oidc/exchange", s.APIOIDCExchange)
	mux.HandleFunc("GET /api/oidc/identities", s.APIListIdentities)
	mux.HandleFunc("DELETE /api/oidc/identities/{id}", s.APIUnlinkIdentity)
	mux.HandleFunc("GET /login", s.ServeLoginPage)
	mux.HandleFunc("GET /account", s.ServeAccountPage)
	return mux
}

// mountAt serves h under prefix, so /rest/ping becomes prefix+"/rest/ping". The
// bare prefix redirects to prefix+"/". An empty prefix returns h unchanged.
func mountAt(prefix string, h http.Handler) http.Handler {
	if prefix == "" {
		return h
	}
	outer := http.NewServeMux()
	outer.Handle(prefix+"/", http.StripPrefix(prefix, h))
	outer.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
	})
	return outer
}
