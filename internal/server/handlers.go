package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tsirysndr/tinysonic/internal/cli"
	"github.com/tsirysndr/tinysonic/internal/models"
	"github.com/tsirysndr/tinysonic/internal/scanner"
	"github.com/tsirysndr/tinysonic/internal/users"
)

// ── Root index ───────────────────────────────────────────────────────────────

func (s *State) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body := cli.Banner + fmt.Sprintf(`
  tinysonic v%s
  a tiny Subsonic-compatible music server

Supported endpoints
  System
    GET  /rest/ping             auth check
    GET  /rest/getUser          single-user response
    GET  /rest/getMusicFolders  one folder
    GET  /rest/getScanStatus    library scan progress
    GET  /rest/startScan        trigger a library rescan

  Library — ID3 tag browsing
    GET  /rest/getArtists       alphabetical artist index
    GET  /rest/getArtist        ?id=ar-…   albums for an artist
    GET  /rest/getAlbum         ?id=al-…   songs for an album
    GET  /rest/getSong          ?id=so-…   single song lookup
    GET  /rest/getAlbumList2    ?type=alphabeticalByName|alphabeticalByArtist|newest|random
    GET  /rest/getAlbumList     alias of getAlbumList2

  Library — folder browsing
    GET  /rest/getIndexes        flat A-Z artist index
    GET  /rest/getMusicDirectory ?id=1|ar-…|al-…

  Genres
    GET  /rest/getGenres
    GET  /rest/getSongsByGenre  ?genre=…&count=&offset=

  Lists
    GET  /rest/getRandomSongs   ?size=&fromYear=&toYear=&genre=
    GET  /rest/getStarred2      starred artists / albums / songs
    GET  /rest/getStarred       alias of getStarred2

  Playback
    GET  /rest/stream           ?id=so-…   raw audio, Range supported
    GET  /rest/download         ?id=so-…   alias for /rest/stream
    GET  /rest/getCoverArt      ?id=al-…|ar-…|so-…   cached album art
    GET  /rest/scrobble         ?id=so-…
    GET  /rest/getNowPlaying
    GET  /rest/updateNowPlaying

  Search
    GET  /rest/search3          ?query=…&artistCount=&albumCount=&songCount=
    GET  /rest/search2          legacy alias

  Playlists
    GET  /rest/getPlaylists
    GET  /rest/getPlaylist      ?id=pl-…
    GET  /rest/createPlaylist   ?name=…&songId=…&songId=…   (or ?playlistId=… to replace)
    GET  /rest/updatePlaylist   ?playlistId=…&name=&comment=&songIdToAdd=&songIndexToRemove=
    GET  /rest/deletePlaylist   ?id=pl-…

  Starring
    GET  /rest/star             ?id=so-…|albumId=al-…|artistId=ar-…
    GET  /rest/unstar           ?id=so-…|albumId=al-…|artistId=ar-…

  Artist / album info  (minimal stubs — no Last.fm lookup)
    GET  /rest/getArtistInfo    ?id=ar-…
    GET  /rest/getArtistInfo2   ?id=ar-…
    GET  /rest/getAlbumInfo     ?id=al-…
    GET  /rest/getAlbumInfo2    ?id=al-…
    GET  /rest/getSimilarSongs  ?id=…
    GET  /rest/getSimilarSongs2 ?id=…
    GET  /rest/getTopSongs      ?artist=…&count=
    GET  /rest/getLyrics        ?artist=&title=

Auth (all /rest/* endpoints)
  Token:     u=<user>&t=md5(password+salt)&s=<salt>
  Plaintext: u=<user>&p=<password>      or  p=enc:<hex>

Every endpoint also accepts the `+"`"+`.view`+"`"+` suffix and POST.
Responses are JSON with the Subsonic envelope: { "subsonic-response": … }
`, cli.Version)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// ── Mappers ──────────────────────────────────────────────────────────────────

func nsToAny(n sql.NullString) any {
	if n.Valid {
		return n.String
	}
	return nil
}

func niToAny(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func songToChild(s models.Song) J {
	return J{
		"id":          s.ID,
		"parent":      s.AlbumID,
		"isDir":       false,
		"title":       s.Title,
		"album":       s.Album,
		"artist":      s.Artist,
		"track":       niToAny(s.TrackNumber),
		"year":        niToAny(s.Year),
		"genre":       nsToAny(s.Genre),
		"coverArt":    s.AlbumID,
		"size":        s.Filesize,
		"contentType": s.ContentType,
		"suffix":      s.Suffix,
		"duration":    s.DurationMs / 1000,
		"bitRate":     s.Bitrate,
		"path":        s.Path,
		"isVideo":     false,
		"discNumber":  niToAny(s.DiscNumber),
		"albumId":     s.AlbumID,
		"artistId":    s.ArtistID,
		"type":        "music",
	}
}

func albumToChild(a models.Album, songCount, durationS int64) J {
	var year any
	if a.Year > 0 {
		year = a.Year
	}
	return J{
		"id":        a.ID,
		"name":      a.Title,
		"title":     a.Title,
		"artist":    a.Artist,
		"artistId":  a.ArtistID,
		"songCount": songCount,
		"duration":  durationS,
		"year":      year,
		"coverArt":  a.ID,
		"created":   "2020-01-01T00:00:00Z",
	}
}

func artistToJSON(a models.Artist, albumCount int64) J {
	return J{
		"id":         a.ID,
		"name":       a.Name,
		"albumCount": albumCount,
		"coverArt":   a.ID,
	}
}

func buildArtistIndex(artists []models.Artist, counts map[string]int64) []J {
	groups := map[string][]J{}
	for _, a := range artists {
		key := "#"
		for _, r := range a.Name {
			if unicode.IsLetter(r) {
				key = string(unicode.ToUpper(r))
			}
			break
		}
		groups[key] = append(groups[key], artistToJSON(a, counts[a.ID]))
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]J, 0, len(keys))
	for _, k := range keys {
		out = append(out, J{"name": k, "artist": groups[k]})
	}
	return out
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func intParam(r *http.Request, key string, def int64) int64 {
	v := queryFirst(r, key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func intParamPtr(r *http.Request, key string) *int64 {
	v := queryFirst(r, key)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func clamp(n, lo, hi int64) int64 {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func max0(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

func nowISO8601() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func newPlaylistID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "pl-" + hex.EncodeToString(b[:])
}

// ── System ───────────────────────────────────────────────────────────────────

func (s *State) Ping(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{})
}

func (s *State) GetUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticate(r)
	if !ok {
		writeError(w, 40, "Wrong username or password")
		return
	}
	writeOK(w, J{
		"user": J{
			"username":          u.Username,
			"email":             "",
			"scrobblingEnabled": false,
			"adminRole":         false, // no API user administers the server
			"settingsRole":      false,
			"downloadRole":      true,
			"uploadRole":        false,
			"playlistRole":      true,
			"coverArtRole":      true,
			"commentRole":       false,
			"podcastRole":       false,
			"streamRole":        true,
			"jukeboxRole":       false,
			"shareRole":         false,
			"folder":            []int{1},
		},
	})
}

func (s *State) GetMusicFolders(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{
		"musicFolders": J{
			"musicFolder": []J{{"id": 1, "name": "Music"}},
		},
	})
}

func (s *State) GetScanStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{
		"scanStatus": J{
			"scanning":      s.Progress.Running(),
			"count":         s.Progress.Count(),
			"watchFailures": s.Progress.WatchFailures(),
		},
	})
}

func (s *State) StartScan(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.authenticate(r)
	if !ok {
		writeError(w, 40, "Wrong username or password")
		return
	}
	if caller.Role != users.RoleLibrarian {
		writeError(w, 50, "only librarians can start a library scan")
		return
	}
	if s.Progress.Running() {
		writeOK(w, J{
			"scanStatus": J{
				"scanning": true,
				"count":    s.Progress.Count(),
			},
		})
		return
	}
	go func() {
		if _, err := scanner.Scan(s.Pool, s.MusicDirs, s.CoversDir, s.Progress); err != nil {
			log.Printf("scan: %v", err)
		}
	}()
	writeOK(w, J{"scanStatus": J{"scanning": true, "count": 0}})
}

// ── Library (tag browsing) ───────────────────────────────────────────────────

func (s *State) GetArtists(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	ctx := r.Context()
	artists, err := repoAllArtists(ctx, s.Pool)
	if err != nil {
		log.Printf("getArtists: %v", err)
		writeError(w, 0, "database error")
		return
	}
	counts, _ := repoAlbumCountsByArtist(ctx, s.Pool)
	writeOK(w, J{
		"artists": J{
			"ignoredArticles": "The An A Die Das Ein",
			"index":           buildArtistIndex(artists, counts),
		},
	})
}

func (s *State) GetArtist(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	ctx := r.Context()
	artist, err := repoFindArtist(ctx, s.Pool, id)
	if err != nil {
		log.Printf("getArtist: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if artist == nil {
		writeError(w, 70, "Artist not found")
		return
	}
	albums, _ := repoAlbumsByArtist(ctx, s.Pool, id)
	albumJSONs := make([]J, 0, len(albums))
	for _, a := range albums {
		count, _ := repoSongCountForAlbum(ctx, s.Pool, a.ID)
		dur, _ := repoSongDurationForAlbum(ctx, s.Pool, a.ID)
		albumJSONs = append(albumJSONs, albumToChild(a, count, dur))
	}
	writeOK(w, J{
		"artist": J{
			"id":         artist.ID,
			"name":       artist.Name,
			"albumCount": len(albumJSONs),
			"coverArt":   artist.ID,
			"album":      albumJSONs,
		},
	})
}

func (s *State) GetAlbum(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	ctx := r.Context()
	album, err := repoFindAlbum(ctx, s.Pool, id)
	if err != nil {
		log.Printf("getAlbum: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if album == nil {
		writeError(w, 70, "Album not found")
		return
	}
	songs, _ := repoSongsByAlbum(ctx, s.Pool, id)
	var totalDur int64
	songJSONs := make([]J, 0, len(songs))
	for _, sg := range songs {
		totalDur += sg.DurationMs / 1000
		songJSONs = append(songJSONs, songToChild(sg))
	}
	var year any
	if album.Year > 0 {
		year = album.Year
	}
	writeOK(w, J{
		"album": J{
			"id":        album.ID,
			"name":      album.Title,
			"title":     album.Title,
			"artist":    album.Artist,
			"artistId":  album.ArtistID,
			"coverArt":  album.ID,
			"songCount": len(songJSONs),
			"duration":  totalDur,
			"year":      year,
			"song":      songJSONs,
		},
	})
}

func (s *State) GetSong(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	song, err := repoFindSong(r.Context(), s.Pool, id)
	if err != nil {
		log.Printf("getSong: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if song == nil {
		writeError(w, 70, "Song not found")
		return
	}
	writeOK(w, J{"song": songToChild(*song)})
}

func (s *State) GetAlbumList2(w http.ResponseWriter, r *http.Request) {
	s.albumList(w, r, "albumList2")
}

func (s *State) GetAlbumList(w http.ResponseWriter, r *http.Request) {
	s.albumList(w, r, "albumList")
}

func (s *State) albumList(w http.ResponseWriter, r *http.Request, wrap string) {
	if !s.requireAuth(w, r) {
		return
	}
	listType := queryFirst(r, "type")
	if listType == "" {
		listType = "alphabeticalByName"
	}
	size := clamp(intParam(r, "size", 10), 1, 500)
	offset := max0(intParam(r, "offset", 0))
	ctx := r.Context()
	albums, _ := repoAlbumsPaginated(ctx, s.Pool, listType, size, offset)
	out := make([]J, 0, len(albums))
	for _, a := range albums {
		count, _ := repoSongCountForAlbum(ctx, s.Pool, a.ID)
		dur, _ := repoSongDurationForAlbum(ctx, s.Pool, a.ID)
		out = append(out, albumToChild(a, count, dur))
	}
	writeOK(w, J{wrap: J{"album": out}})
}

// ── Folder browsing ─────────────────────────────────────────────────────────

func (s *State) GetIndexes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	ctx := r.Context()
	artists, _ := repoAllArtists(ctx, s.Pool)
	counts, _ := repoAlbumCountsByArtist(ctx, s.Pool)
	writeOK(w, J{
		"indexes": J{
			"lastModified":    0,
			"ignoredArticles": "The An A Die Das Ein",
			"index":           buildArtistIndex(artists, counts),
		},
	})
}

func (s *State) GetMusicDirectory(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	ctx := r.Context()

	if id == "1" {
		artists, _ := repoAllArtists(ctx, s.Pool)
		children := make([]J, 0, len(artists))
		for _, a := range artists {
			children = append(children, J{
				"id":       a.ID,
				"parent":   "1",
				"isDir":    true,
				"title":    a.Name,
				"album":    a.Name,
				"artist":   a.Name,
				"coverArt": a.ID,
			})
		}
		writeOK(w, J{
			"directory": J{
				"id":    "1",
				"name":  "Music",
				"child": children,
			},
		})
		return
	}

	if artist, _ := repoFindArtist(ctx, s.Pool, id); artist != nil {
		albums, _ := repoAlbumsByArtist(ctx, s.Pool, id)
		children := make([]J, 0, len(albums))
		for _, a := range albums {
			var year any
			if a.Year > 0 {
				year = a.Year
			}
			children = append(children, J{
				"id":       a.ID,
				"parent":   artist.ID,
				"isDir":    true,
				"title":    a.Title,
				"album":    a.Title,
				"artist":   a.Artist,
				"year":     year,
				"coverArt": a.ID,
			})
		}
		writeOK(w, J{
			"directory": J{
				"id":    artist.ID,
				"name":  artist.Name,
				"child": children,
			},
		})
		return
	}

	if album, _ := repoFindAlbum(ctx, s.Pool, id); album != nil {
		songs, _ := repoSongsByAlbum(ctx, s.Pool, id)
		children := make([]J, 0, len(songs))
		for _, sg := range songs {
			children = append(children, songToChild(sg))
		}
		writeOK(w, J{
			"directory": J{
				"id":    album.ID,
				"name":  album.Title,
				"child": children,
			},
		})
		return
	}

	writeError(w, 70, "Directory not found")
}

// ── Genres ──────────────────────────────────────────────────────────────────

func (s *State) GetGenres(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	rows, _ := repoDistinctGenres(r.Context(), s.Pool)
	genres := make([]J, 0, len(rows))
	for _, g := range rows {
		genres = append(genres, J{"value": g.Name, "songCount": g.SongCount, "albumCount": g.AlbumCount})
	}
	writeOK(w, J{"genres": J{"genre": genres}})
}

func (s *State) GetSongsByGenre(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	genre := queryFirst(r, "genre")
	if genre == "" {
		writeError(w, 10, "Required parameter is missing: genre")
		return
	}
	count := clamp(intParam(r, "count", 10), 1, 500)
	offset := max0(intParam(r, "offset", 0))
	songs, _ := repoSongsByGenre(r.Context(), s.Pool, genre, count, offset)
	children := make([]J, 0, len(songs))
	for _, sg := range songs {
		children = append(children, songToChild(sg))
	}
	writeOK(w, J{"songsByGenre": J{"song": children}})
}

// ── Random + Starred ────────────────────────────────────────────────────────

func (s *State) GetRandomSongs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	size := clamp(intParam(r, "size", 10), 1, 500)
	from := intParamPtr(r, "fromYear")
	to := intParamPtr(r, "toYear")
	genre := queryFirst(r, "genre")
	songs, _ := repoRandomSongs(r.Context(), s.Pool, size, from, to, genre)
	children := make([]J, 0, len(songs))
	for _, sg := range songs {
		children = append(children, songToChild(sg))
	}
	writeOK(w, J{"randomSongs": J{"song": children}})
}

func (s *State) GetStarred(w http.ResponseWriter, r *http.Request) {
	// Same payload as starred2 — matches the original behaviour.
	s.GetStarred2(w, r)
}

func (s *State) GetStarred2(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	songs, _ := repoStarredSongs(ctx, s.Pool, caller.ID)
	albums, _ := repoStarredAlbums(ctx, s.Pool, caller.ID)
	artists, _ := repoStarredArtists(ctx, s.Pool, caller.ID)

	songJSONs := make([]J, 0, len(songs))
	for _, p := range songs {
		v := songToChild(p.Song)
		v["starred"] = p.When
		songJSONs = append(songJSONs, v)
	}
	albumJSONs := make([]J, 0, len(albums))
	for _, p := range albums {
		count, _ := repoSongCountForAlbum(ctx, s.Pool, p.Album.ID)
		dur, _ := repoSongDurationForAlbum(ctx, s.Pool, p.Album.ID)
		v := albumToChild(p.Album, count, dur)
		v["starred"] = p.When
		albumJSONs = append(albumJSONs, v)
	}
	artistJSONs := make([]J, 0, len(artists))
	for _, p := range artists {
		v := artistToJSON(p.Artist, 0)
		v["starred"] = p.When
		artistJSONs = append(artistJSONs, v)
	}
	writeOK(w, J{
		"starred2": J{
			"artist": artistJSONs,
			"album":  albumJSONs,
			"song":   songJSONs,
		},
	})
}

func (s *State) Star(w http.ResponseWriter, r *http.Request) {
	s.starOp(w, r, true)
}

func (s *State) Unstar(w http.ResponseWriter, r *http.Request) {
	s.starOp(w, r, false)
}

func (s *State) starOp(w http.ResponseWriter, r *http.Request, star bool) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	target := queryFirst(r, "id")
	if target == "" {
		target = queryFirst(r, "albumId")
	}
	if target == "" {
		target = queryFirst(r, "artistId")
	}
	if target == "" {
		writeOK(w, J{})
		return
	}
	var err error
	if star {
		err = repoStar(r.Context(), s.Pool, caller.ID, target, nowISO8601())
	} else {
		err = repoUnstar(r.Context(), s.Pool, caller.ID, target)
	}
	if err != nil {
		log.Printf("starOp: %v", err)
		writeError(w, 0, "database error")
		return
	}
	writeOK(w, J{})
}

// ── Playback ────────────────────────────────────────────────────────────────

func (s *State) Stream(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	path, contentType, found, err := s.lookupSong(r.Context(), id)
	if err != nil {
		log.Printf("stream lookup %s: %v", id, err)
		writeError(w, 0, "database error")
		return
	}
	if !found {
		writeError(w, 70, "Song not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.evictSong(id)
		log.Printf("stream open %s: %v", path, err)
		writeError(w, 0, "could not read file")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		log.Printf("stream stat %s: %v", path, err)
		writeError(w, 0, "could not read file")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, path, st.ModTime(), f)
}

func (s *State) GetCoverArt(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var filename string
	switch {
	case strings.HasPrefix(id, "al-"):
		if a, _ := repoFindAlbum(ctx, s.Pool, id); a != nil && a.CoverArt.Valid {
			filename = a.CoverArt.String
		}
	case strings.HasPrefix(id, "so-"):
		if song, _ := repoFindSong(ctx, s.Pool, id); song != nil {
			if song.CoverArt.Valid {
				filename = song.CoverArt.String
			} else if a, _ := repoFindAlbum(ctx, s.Pool, song.AlbumID); a != nil && a.CoverArt.Valid {
				filename = a.CoverArt.String
			}
		}
	case strings.HasPrefix(id, "ar-"):
		albums, _ := repoAlbumsByArtist(ctx, s.Pool, id)
		for _, a := range albums {
			if a.CoverArt.Valid {
				filename = a.CoverArt.String
				break
			}
		}
	}
	if filename == "" {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.CoversDir, filename)
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, full)
}

func (s *State) Scrobble(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{})
}

func (s *State) GetNowPlaying(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{"nowPlaying": J{"entry": []J{}}})
}

func (s *State) UpdateNowPlaying(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{})
}

// ── Search ──────────────────────────────────────────────────────────────────

func (s *State) Search3(w http.ResponseWriter, r *http.Request) {
	s.searchN(w, r, "searchResult3")
}

func (s *State) Search2(w http.ResponseWriter, r *http.Request) {
	s.searchN(w, r, "searchResult2")
}

func (s *State) searchN(w http.ResponseWriter, r *http.Request, wrap string) {
	if !s.requireAuth(w, r) {
		return
	}
	ctx := r.Context()
	term := queryFirst(r, "query")
	artistLimit := intParam(r, "artistCount", 20)
	albumLimit := intParam(r, "albumCount", 20)
	songLimit := intParam(r, "songCount", 20)
	artistOffset := intParam(r, "artistOffset", 0)
	albumOffset := intParam(r, "albumOffset", 0)
	songOffset := intParam(r, "songOffset", 0)

	artists, _ := repoSearchArtists(ctx, s.Pool, term, artistLimit, artistOffset)
	albums, _ := repoSearchAlbums(ctx, s.Pool, term, albumLimit, albumOffset)
	songs, _ := repoSearchSongs(ctx, s.Pool, term, songLimit, songOffset)

	artistJSONs := make([]J, 0, len(artists))
	for _, a := range artists {
		artistJSONs = append(artistJSONs, artistToJSON(a, 0))
	}
	albumJSONs := make([]J, 0, len(albums))
	for _, a := range albums {
		count, _ := repoSongCountForAlbum(ctx, s.Pool, a.ID)
		dur, _ := repoSongDurationForAlbum(ctx, s.Pool, a.ID)
		albumJSONs = append(albumJSONs, albumToChild(a, count, dur))
	}
	songJSONs := make([]J, 0, len(songs))
	for _, sg := range songs {
		songJSONs = append(songJSONs, songToChild(sg))
	}
	writeOK(w, J{
		wrap: J{
			"artist": artistJSONs,
			"album":  albumJSONs,
			"song":   songJSONs,
		},
	})
}

// ── Playlists ───────────────────────────────────────────────────────────────

func (s *State) playlistJSON(r *http.Request, pl *models.Playlist) J {
	songs, _ := repoPlaylistSongs(r.Context(), s.Pool, pl.ID)
	var dur int64
	for _, sg := range songs {
		dur += sg.DurationMs / 1000
	}
	return J{
		"id":        pl.ID,
		"name":      pl.Name,
		"comment":   nsToAny(pl.Comment),
		"songCount": len(songs),
		"duration":  dur,
		"public":    pl.Public != 0,
		"created":   pl.CreatedAt,
		"changed":   pl.UpdatedAt,
		"coverArt":  nil,
	}
}

func (s *State) GetPlaylists(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	playlists, _ := repoAllPlaylists(r.Context(), s.Pool, caller.ID)
	out := make([]J, 0, len(playlists))
	for i := range playlists {
		out = append(out, s.playlistJSON(r, &playlists[i]))
	}
	writeOK(w, J{"playlists": J{"playlist": out}})
}

func (s *State) GetPlaylist(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	pl, err := repoFindPlaylist(r.Context(), s.Pool, caller.ID, id)
	if err != nil {
		log.Printf("getPlaylist: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if pl == nil {
		writeError(w, 70, "Playlist not found")
		return
	}
	songs, _ := repoPlaylistSongs(r.Context(), s.Pool, id)
	var dur int64
	entry := make([]J, 0, len(songs))
	for _, sg := range songs {
		dur += sg.DurationMs / 1000
		entry = append(entry, songToChild(sg))
	}
	writeOK(w, J{
		"playlist": J{
			"id":        pl.ID,
			"name":      pl.Name,
			"comment":   nsToAny(pl.Comment),
			"songCount": len(entry),
			"duration":  dur,
			"public":    pl.Public != 0,
			"created":   pl.CreatedAt,
			"changed":   pl.UpdatedAt,
			"coverArt":  nil,
			"entry":     entry,
		},
	})
}

func (s *State) CreatePlaylist(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	existingID := queryFirst(r, "playlistId")
	name := queryFirst(r, "name")
	if name == "" {
		name = "Untitled"
	}
	songIDs := queryAll(r, "songId")
	now := nowISO8601()

	var id string
	if existingID != "" {
		id = existingID
		pl, _ := repoFindPlaylist(ctx, s.Pool, caller.ID, id)
		if pl == nil {
			if err := repoCreatePlaylist(ctx, s.Pool, caller.ID, id, name, now); err != nil {
				log.Printf("createPlaylist: %v", err)
				writeError(w, 0, "database error")
				return
			}
		} else {
			_ = repoRenamePlaylist(ctx, s.Pool, id, name, now)
		}
		if len(songIDs) > 0 {
			_ = repoReplacePlaylistSongs(ctx, s.Pool, id, songIDs)
		}
	} else {
		id = newPlaylistID()
		if err := repoCreatePlaylist(ctx, s.Pool, caller.ID, id, name, now); err != nil {
			log.Printf("createPlaylist: %v", err)
			writeError(w, 0, "database error")
			return
		}
		if len(songIDs) > 0 {
			_ = repoAppendPlaylistSongs(ctx, s.Pool, id, songIDs)
		}
	}

	pl, err := repoFindPlaylist(ctx, s.Pool, caller.ID, id)
	if err != nil || pl == nil {
		writeError(w, 0, "playlist persistence error")
		return
	}
	songs, _ := repoPlaylistSongs(ctx, s.Pool, id)
	var dur int64
	entry := make([]J, 0, len(songs))
	for _, sg := range songs {
		dur += sg.DurationMs / 1000
		entry = append(entry, songToChild(sg))
	}
	writeOK(w, J{
		"playlist": J{
			"id":        pl.ID,
			"name":      pl.Name,
			"comment":   nsToAny(pl.Comment),
			"songCount": len(entry),
			"duration":  dur,
			"public":    pl.Public != 0,
			"created":   pl.CreatedAt,
			"changed":   pl.UpdatedAt,
			"coverArt":  nil,
			"entry":     entry,
		},
	})
}

func (s *State) UpdatePlaylist(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := queryFirst(r, "playlistId")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: playlistId")
		return
	}
	pl, _ := repoFindPlaylist(ctx, s.Pool, caller.ID, id)
	if pl == nil {
		writeError(w, 70, "Playlist not found")
		return
	}
	now := nowISO8601()
	if v, ok := singleParam(r, "name"); ok {
		_ = repoRenamePlaylist(ctx, s.Pool, id, v, now)
	}
	if v, ok := singleParam(r, "comment"); ok {
		_ = repoSetPlaylistComment(ctx, s.Pool, id, v, now)
	}
	if toAdd := queryAll(r, "songIdToAdd"); len(toAdd) > 0 {
		_ = repoAppendPlaylistSongs(ctx, s.Pool, id, toAdd)
	}
	if rawRemoves := queryAll(r, "songIndexToRemove"); len(rawRemoves) > 0 {
		idx := make([]int, 0, len(rawRemoves))
		for _, raw := range rawRemoves {
			if n, err := strconv.Atoi(raw); err == nil {
				idx = append(idx, n)
			}
		}
		if len(idx) > 0 {
			current, err := repoPlaylistSongIDs(ctx, s.Pool, id)
			if err == nil {
				sort.Sort(sort.Reverse(sort.IntSlice(idx)))
				for _, i := range idx {
					if i >= 0 && i < len(current) {
						current = append(current[:i], current[i+1:]...)
					}
				}
				_ = repoReplacePlaylistSongs(ctx, s.Pool, id, current)
			}
		}
	}
	writeOK(w, J{})
}

func singleParam(r *http.Request, key string) (string, bool) {
	q := params(r)
	if _, ok := q[key]; !ok {
		return "", false
	}
	return q.Get(key), true
}

func (s *State) DeletePlaylist(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	if err := repoDeletePlaylist(r.Context(), s.Pool, caller.ID, id); err != nil {
		log.Printf("deletePlaylist: %v", err)
		writeError(w, 0, "database error")
		return
	}
	writeOK(w, J{})
}

// ── Artist / album info stubs ───────────────────────────────────────────────

func emptyArtistInfo() J {
	return J{
		"biography":      "",
		"musicBrainzId":  "",
		"lastFmUrl":      "",
		"smallImageUrl":  "",
		"mediumImageUrl": "",
		"largeImageUrl":  "",
		"similarArtist":  []J{},
	}
}

func (s *State) GetArtistInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{"artistInfo": emptyArtistInfo()})
}

func (s *State) GetArtistInfo2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{"artistInfo2": emptyArtistInfo()})
}

func (s *State) GetAlbumInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{
		"albumInfo": J{
			"notes":          "",
			"musicBrainzId":  "",
			"lastFmUrl":      "",
			"smallImageUrl":  "",
			"mediumImageUrl": "",
			"largeImageUrl":  "",
		},
	})
}

func (s *State) GetSimilarSongs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{"similarSongs": J{"song": []J{}}})
}

func (s *State) GetSimilarSongs2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{"similarSongs2": J{"song": []J{}}})
}

func (s *State) GetTopSongs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	artist := queryFirst(r, "artist")
	if artist == "" {
		writeOK(w, J{"topSongs": J{"song": []J{}}})
		return
	}
	count := clamp(intParam(r, "count", 50), 1, 500)
	songs, _ := repoSearchSongs(r.Context(), s.Pool, artist, count, 0)
	children := make([]J, 0, len(songs))
	for _, sg := range songs {
		if strings.EqualFold(sg.Artist, artist) {
			children = append(children, songToChild(sg))
		}
	}
	writeOK(w, J{"topSongs": J{"song": children}})
}

func (s *State) GetLyrics(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{
		"lyrics": J{
			"artist": queryFirst(r, "artist"),
			"title":  queryFirst(r, "title"),
			"value":  "",
		},
	})
}

// keep imports tidy when functions are unused in some build modes
var _ = errors.Is
