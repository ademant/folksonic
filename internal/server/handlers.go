package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
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
    GET  /rest/tinysonicLibraryInfo  library totals and last change (vendor)

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

List endpoints accept size/offset and send X-Total-Count headers; HEAD
returns only the headers. Prefer: tinysonic-minimal trims derivable fields.
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

// mbidValue is the MusicBrainz ID a client should see, or "" when the file has none.
func mbidValue(n sql.NullString) string {
	if n.Valid {
		return n.String
	}
	return ""
}

func (st *State) songToChild(s models.Song) J {
	out := J{
		"id":            s.ID,
		"musicBrainzId": mbidValue(s.MBID),
		"mediaType":     "song",
		"parent":        s.AlbumID,
		"isDir":         false,
		"title":         s.Title,
		"album":         s.Album,
		"artist":        s.Artist,
		"track":         niToAny(s.TrackNumber),
		"year":          niToAny(s.Year),
		"genre":         nsToAny(s.Genre),
		"coverArt":      s.AlbumID,
		"size":          s.Filesize,
		"contentType":   s.ContentType,
		"suffix":        s.Suffix,
		"duration":      s.DurationMs / 1000,
		"bitRate":       s.Bitrate,
		"path":          relativePath(st.MusicDirs, s.Path),
		"isVideo":       false,
		"discNumber":    niToAny(s.DiscNumber),
		"albumId":       s.AlbumID,
		"artistId":      s.ArtistID,
		"type":          "music",
		"created":       nsToAny(s.Created),
	}
	if rg := replayGain(s); rg != nil {
		out["replayGain"] = rg
	}
	if s.BPM.Valid {
		out["bpm"] = int64(math.Round(s.BPM.Float64))
	}
	if s.Meter.Valid && s.Meter.String != "" {
		out["tinysonicMeter"] = s.Meter.String // vendor extension tinysonicMeter
	}
	if s.Tags.Valid {
		var tags map[string][]string
		if json.Unmarshal([]byte(s.Tags.String), &tags) == nil && len(tags) > 0 {
			out["tinysonicTags"] = tags
		}
	}
	return out
}

// replayGain is the OpenSubsonic replayGain object with the known values
// only, or nil when no gain is known. An Opus output gain of 0 alone is not
// worth an object.
func replayGain(s models.Song) J {
	rg := J{}
	for k, v := range map[string]sql.NullFloat64{
		"trackGain": s.TrackGain, "albumGain": s.AlbumGain,
		"trackPeak": s.TrackPeak, "albumPeak": s.AlbumPeak,
	} {
		if v.Valid {
			rg[k] = roundGain(v.Float64)
		}
	}
	if s.BaseGain.Valid && (len(rg) > 0 || s.BaseGain.Float64 != 0) {
		rg["baseGain"] = roundGain(s.BaseGain.Float64)
	}
	if len(rg) == 0 {
		return nil
	}
	return rg
}

// roundGain keeps gains and peaks to the precision the tags carry.
func roundGain(f float64) float64 { return math.Round(f*1e6) / 1e6 }

// albumToChild is the album object shared by every endpoint, including
// getAlbum, so song count, duration and created agree everywhere.
func albumToChild(a models.Album) J {
	var year any
	if a.Year > 0 {
		year = a.Year
	}
	return J{
		"id":            a.ID,
		"musicBrainzId": mbidValue(a.MBID),
		"mediaType":     "album",
		"name":          a.Title,
		"title":         a.Title,
		"artist":        a.Artist,
		"artistId":      a.ArtistID,
		"songCount":     a.SongCount,
		"duration":      a.DurationMs / 1000,
		"year":          year,
		"coverArt":      a.ID,
		"created":       nsToAny(a.Created),
	}
}

func artistToJSON(a models.Artist, albumCount int64) J {
	return J{
		"id":            a.ID,
		"musicBrainzId": mbidValue(a.MBID),
		"mediaType":     "artist",
		"name":          a.Name,
		"albumCount":    albumCount,
		"coverArt":      a.ID,
	}
}

// indexedArtist is an artist with its index letter and sort key, the name
// without a leading article ("Les Frères Morvan" under F).
type indexedArtist struct {
	letter, key string
	artist      models.Artist
}

// sortArtistsForIndex orders artists by index letter, then name without
// article, then id, so the flat order is the order of the grouped index and
// paging through it is stable. The names themselves are not changed.
func sortArtistsForIndex(artists []models.Artist, articles []string) []indexedArtist {
	out := make([]indexedArtist, 0, len(artists))
	for _, a := range artists {
		key := strings.ToLower(stripArticle(a.Name, articles))
		letter := "#"
		for _, r := range key {
			if unicode.IsLetter(r) {
				letter = string(unicode.ToUpper(r))
			}
			break
		}
		out = append(out, indexedArtist{letter, key, a})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.letter != b.letter {
			return a.letter < b.letter
		}
		if a.key != b.key {
			return a.key < b.key
		}
		return a.artist.ID < b.artist.ID
	})
	return out
}

// groupArtistIndex groups sorted artists by letter.
func groupArtistIndex(sorted []indexedArtist, counts map[string]int64) []J {
	out := []J{}
	var cur []J
	for i, e := range sorted {
		cur = append(cur, artistToJSON(e.artist, counts[e.artist.ID]))
		if i == len(sorted)-1 || sorted[i+1].letter != e.letter {
			out = append(out, J{"name": e.letter, "artist": cur})
			cur = nil
		}
	}
	return out
}

func buildArtistIndex(artists []models.Artist, counts map[string]int64, articles []string) []J {
	return groupArtistIndex(sortArtistsForIndex(artists, articles), counts)
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
	if s.jobs.running() || !s.Progress.Begin() {
		writeOK(w, J{
			"scanStatus": J{
				"scanning": s.Progress.Running(),
				"count":    s.Progress.Count(),
			},
		})
		return
	}
	go func() {
		if _, err := scanner.Scan(s.Pool, s.MusicDirs, s.CoversDir, s.SongTags, s.Progress); err != nil {
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
	setTotal(w, len(artists))
	if headDone(w, r) {
		return
	}
	counts, _ := repoAlbumCountsByArtist(ctx, s.Pool)
	sorted := pageSlice(sortArtistsForIndex(artists, s.ignoredArticles()), pageParams(r))
	writeOK(w, J{
		"artists": J{
			"ignoredArticles": strings.Join(s.ignoredArticles(), " "),
			"index":           groupArtistIndex(sorted, counts),
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
	setTotal(w, len(albums))
	if headDone(w, r) {
		return
	}
	paged := pageSlice(albums, pageParams(r))
	albumJSONs := make([]J, 0, len(paged))
	for _, a := range paged {
		albumJSONs = append(albumJSONs, albumToChild(a))
	}
	writeOK(w, J{
		"artist": J{
			"id":            artist.ID,
			"musicBrainzId": mbidValue(artist.MBID),
			"mediaType":     "artist",
			"name":          artist.Name,
			"albumCount":    len(albums),
			"coverArt":      artist.ID,
			"album":         albumJSONs,
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
	setTotal(w, len(songs))
	if headDone(w, r) {
		return
	}
	songs = pageSlice(songs, pageParams(r))
	songJSONs := make([]J, 0, len(songs))
	for _, sg := range songs {
		songJSONs = append(songJSONs, s.songToChild(sg))
	}
	out := albumToChild(*album)
	out["song"] = songJSONs
	writeOK(w, J{"album": out})
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
	writeOK(w, J{"song": s.songToChild(*song)})
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
	if total, err := repoCount(ctx, s.Pool, "SELECT COUNT(*) FROM albums"); err == nil {
		setTotal(w, total)
	}
	if headDone(w, r) {
		return
	}
	albums, _ := repoAlbumsPaginated(ctx, s.Pool, listType, size, offset)
	out := make([]J, 0, len(albums))
	for _, a := range albums {
		out = append(out, albumToChild(a))
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
	setTotal(w, len(artists))
	if headDone(w, r) {
		return
	}
	counts, _ := repoAlbumCountsByArtist(ctx, s.Pool)
	sorted := pageSlice(sortArtistsForIndex(artists, s.ignoredArticles()), pageParams(r))
	writeOK(w, J{
		"indexes": J{
			"lastModified":    0,
			"ignoredArticles": strings.Join(s.ignoredArticles(), " "),
			"index":           groupArtistIndex(sorted, counts),
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
		setTotal(w, len(children))
		if headDone(w, r) {
			return
		}
		children = pageSlice(children, pageParams(r))
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
		setTotal(w, len(children))
		if headDone(w, r) {
			return
		}
		children = pageSlice(children, pageParams(r))
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
			children = append(children, s.songToChild(sg))
		}
		setTotal(w, len(children))
		if headDone(w, r) {
			return
		}
		children = pageSlice(children, pageParams(r))
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
	setTotal(w, len(rows))
	if headDone(w, r) {
		return
	}
	rows = pageSlice(rows, pageParams(r))
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
	count := clamp(countParam(r, "count", 10), 1, 500)
	offset := max0(intParam(r, "offset", 0))
	if total, err := repoCount(r.Context(), s.Pool, "SELECT COUNT(*) FROM songs WHERE genre = ? COLLATE NOCASE", genre); err == nil {
		setTotal(w, total)
	}
	if headDone(w, r) {
		return
	}
	songs, _ := repoSongsByGenre(r.Context(), s.Pool, genre, count, offset)
	children := make([]J, 0, len(songs))
	for _, sg := range songs {
		children = append(children, s.songToChild(sg))
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
	if total, err := repoCountRandomSongs(r.Context(), s.Pool, from, to, genre); err == nil {
		setTotal(w, total)
	}
	if headDone(w, r) {
		return
	}
	songs, _ := repoRandomSongs(r.Context(), s.Pool, size, from, to, genre)
	children := make([]J, 0, len(songs))
	for _, sg := range songs {
		children = append(children, s.songToChild(sg))
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
	setTotals(w, len(artists), len(albums), len(songs))
	if headDone(w, r) {
		return
	}
	p := pageParams(r)
	songs, albums, artists = pageSlice(songs, p), pageSlice(albums, p), pageSlice(artists, p)

	songJSONs := make([]J, 0, len(songs))
	for _, p := range songs {
		v := s.songToChild(p.Song)
		v["starred"] = p.When
		songJSONs = append(songJSONs, v)
	}
	albumJSONs := make([]J, 0, len(albums))
	for _, p := range albums {
		v := albumToChild(p.Album)
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
	// The standard parameters win; size/offset (tinysonicPaging) apply to
	// every list that has no parameter of its own.
	artistLimit := countParam(r, "artistCount", 20)
	albumLimit := countParam(r, "albumCount", 20)
	songLimit := countParam(r, "songCount", 20)
	artistOffset := offsetParam(r, "artistOffset")
	albumOffset := offsetParam(r, "albumOffset")
	songOffset := offsetParam(r, "songOffset")

	na, _ := repoCountSearch(ctx, s.Pool, "artists", term)
	nb, _ := repoCountSearch(ctx, s.Pool, "albums", term)
	nc, _ := repoCountSearch(ctx, s.Pool, "songs", term)
	setTotals(w, na, nb, nc)
	if headDone(w, r) {
		return
	}

	artists, _ := repoSearchArtists(ctx, s.Pool, term, artistLimit, artistOffset)
	albums, _ := repoSearchAlbums(ctx, s.Pool, term, albumLimit, albumOffset)
	songs, _ := repoSearchSongs(ctx, s.Pool, term, songLimit, songOffset)

	artistJSONs := make([]J, 0, len(artists))
	for _, a := range artists {
		artistJSONs = append(artistJSONs, artistToJSON(a, 0))
	}
	albumJSONs := make([]J, 0, len(albums))
	for _, a := range albums {
		albumJSONs = append(albumJSONs, albumToChild(a))
	}
	songJSONs := make([]J, 0, len(songs))
	for _, sg := range songs {
		songJSONs = append(songJSONs, s.songToChild(sg))
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
	var durMs int64
	for _, sg := range songs {
		durMs += sg.DurationMs
	}
	return J{
		"id":        pl.ID,
		"name":      pl.Name,
		"comment":   nsToAny(pl.Comment),
		"songCount": len(songs),
		"duration":  durMs / 1000,
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
	setTotal(w, len(playlists))
	if headDone(w, r) {
		return
	}
	playlists = pageSlice(playlists, pageParams(r))
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
	setTotal(w, len(songs))
	if headDone(w, r) {
		return
	}
	var durMs int64
	for _, sg := range songs {
		durMs += sg.DurationMs
	}
	paged := pageSlice(songs, pageParams(r))
	entry := make([]J, 0, len(paged))
	for _, sg := range paged {
		entry = append(entry, s.songToChild(sg))
	}
	writeOK(w, J{
		"playlist": J{
			"id":        pl.ID,
			"name":      pl.Name,
			"comment":   nsToAny(pl.Comment),
			"songCount": len(songs),
			"duration":  durMs / 1000,
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
	var durMs int64
	entry := make([]J, 0, len(songs))
	for _, sg := range songs {
		durMs += sg.DurationMs
		entry = append(entry, s.songToChild(sg))
	}
	writeOK(w, J{
		"playlist": J{
			"id":        pl.ID,
			"name":      pl.Name,
			"comment":   nsToAny(pl.Comment),
			"songCount": len(entry),
			"duration":  durMs / 1000,
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
			children = append(children, s.songToChild(sg))
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
