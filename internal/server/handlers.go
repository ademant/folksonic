package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/tsirysndr/tinysonic/internal/logging"
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
	// The page and the plain-text banner share the address (#201).
	w.Header().Set("Vary", "Accept")
	if acceptsHTML(r) {
		s.serveInfoPage(w, r)
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
  Password:  u=<user>&p=<password>      or  p=enc:<hex>
  (token auth with t and s is not supported; use an API key or the password)

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

// songToChild is the song object. plays holds the caller's counts, so
// playCount is always present (0 when the song has no plays) and played only
// once it has one.
func (st *State) songToChild(s models.Song, plays playStats) J {
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
	if t := songTempo(s); t != nil {
		out["tinysonicTempo"] = t // vendor extension tinysonicTempo, see tempo.go
	}
	if s.Meter.Valid && s.Meter.String != "" {
		out["tinysonicMeter"] = s.Meter.String // vendor extension tinysonicMeter
	}
	p := plays[s.ID]
	out["playCount"] = p.Count
	if p.Count > 0 {
		out["played"] = time.UnixMilli(p.LastMs).UTC().Format("2006-01-02T15:04:05Z")
	}
	if p.Rating > 0 {
		out["userRating"] = p.Rating // the caller's star rating, set by setRating (#140); absent when unrated
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

// indexedIDs is the artist ids of a page of the index.
func indexedIDs(list []indexedArtist) []string {
	ids := make([]string, len(list))
	for i, a := range list {
		ids[i] = a.artist.ID
	}
	return ids
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

// GetUser answers for the caller, or for another account when a librarian
// asks; anyone else asking about another account gets error 50 (#190). A
// librarian has adminRole, as clients use it to show administration.
func (s *State) GetUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.authenticate(r)
	if !ok {
		s.writeAuthFailure(w, r)
		return
	}
	if name := queryFirst(r, "username"); name != "" && name != u.Username {
		if u.Role != users.RoleLibrarian {
			writeError(w, 50, "Not allowed to read another user")
			return
		}
		other, err := users.ByName(r.Context(), s.Pool, name)
		if errors.Is(err, users.ErrNotFound) {
			writeError(w, 70, "User not found")
			return
		}
		if err != nil {
			s.dbFailed(w, "getUser", err)
			return
		}
		u = other
	}
	writeOK(w, J{
		"user": J{
			"username":          u.Username,
			"email":             "",
			"scrobblingEnabled": false,
			"adminRole":         u.Role == users.RoleLibrarian,
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
		s.writeAuthFailure(w, r)
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
			logging.Errorf("scan: %v", err)
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
		logging.Errorf("getArtists: %v", err)
		writeError(w, 0, "database error")
		return
	}
	setTotal(w, len(artists))
	if headDone(w, r) {
		return
	}
	// The sort strips ignored articles, so it runs here on the small
	// artist rows; album counts are read for the page only (#200).
	sorted := pageSlice(sortArtistsForIndex(artists, s.ignoredArticles()), pageParams(r))
	counts, err := repoAlbumCountsByArtist(ctx, s.Pool, indexedIDs(sorted))
	if err != nil {
		s.dbFailed(w, "GetArtists", err)
		return
	}
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
		logging.Errorf("getArtist: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if artist == nil {
		writeError(w, 70, "Artist not found")
		return
	}
	albums, err := repoAlbumsByArtist(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "GetArtist", err)
		return
	}
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
	caller, ok := s.requireCaller(w, r)
	if !ok {
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
		logging.Errorf("getAlbum: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if album == nil {
		writeError(w, 70, "Album not found")
		return
	}
	songs, err := repoSongsByAlbum(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "GetAlbum", err)
		return
	}
	setTotal(w, len(songs))
	if headDone(w, r) {
		return
	}
	songs = pageSlice(songs, pageParams(r))
	plays, err := s.playsFor(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "GetAlbum", err)
		return
	}
	songJSONs := make([]J, 0, len(songs))
	for _, sg := range songs {
		songJSONs = append(songJSONs, s.songToChild(sg, plays))
	}
	out := albumToChild(*album)
	out["song"] = songJSONs
	writeOK(w, J{"album": out})
}

func (s *State) GetSong(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	song, err := repoFindSong(r.Context(), s.Pool, id)
	if err != nil {
		logging.Errorf("getSong: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if song == nil {
		writeError(w, 70, "Song not found")
		return
	}
	plays, err := s.playsFor(r.Context(), caller.ID, []models.Song{*song})
	if err != nil {
		s.dbFailed(w, "GetSong", err)
		return
	}
	writeOK(w, J{"song": s.songToChild(*song, plays)})
}

func (s *State) GetAlbumList2(w http.ResponseWriter, r *http.Request) {
	s.albumList(w, r, "albumList2")
}

func (s *State) GetAlbumList(w http.ResponseWriter, r *http.Request) {
	s.albumList(w, r, "albumList")
}

// ── Folder browsing ─────────────────────────────────────────────────────────

func (s *State) GetIndexes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	ctx := r.Context()
	artists, err := repoAllArtists(ctx, s.Pool)
	if err != nil {
		s.dbFailed(w, "GetIndexes", err)
		return
	}
	setTotal(w, len(artists))
	if headDone(w, r) {
		return
	}
	// The sort strips ignored articles, so it runs here on the small
	// artist rows; album counts are read for the page only (#200).
	sorted := pageSlice(sortArtistsForIndex(artists, s.ignoredArticles()), pageParams(r))
	counts, err := repoAlbumCountsByArtist(ctx, s.Pool, indexedIDs(sorted))
	if err != nil {
		s.dbFailed(w, "GetIndexes", err)
		return
	}
	writeOK(w, J{
		"indexes": J{
			"lastModified":    0,
			"ignoredArticles": strings.Join(s.ignoredArticles(), " "),
			"index":           groupArtistIndex(sorted, counts),
		},
	})
}

func (s *State) GetMusicDirectory(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	ctx := r.Context()

	if id == "1" {
		artists, err := repoAllArtists(ctx, s.Pool)
		if err != nil {
			s.dbFailed(w, "GetMusicDirectory", err)
			return
		}
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

	artist, err := repoFindArtist(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "GetMusicDirectory", err)
		return
	}
	if artist != nil {
		albums, err := repoAlbumsByArtist(ctx, s.Pool, id)
		if err != nil {
			s.dbFailed(w, "GetMusicDirectory", err)
			return
		}
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

	album, err := repoFindAlbum(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "GetMusicDirectory", err)
		return
	}
	if album != nil {
		songs, err := repoSongsByAlbum(ctx, s.Pool, id)
		if err != nil {
			s.dbFailed(w, "GetMusicDirectory", err)
			return
		}
		plays, err := s.playsFor(ctx, caller.ID, songs)
		if err != nil {
			s.dbFailed(w, "GetMusicDirectory", err)
			return
		}
		children := make([]J, 0, len(songs))
		for _, sg := range songs {
			children = append(children, s.songToChild(sg, plays))
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
	rows, err := repoDistinctGenres(r.Context(), s.Pool)
	if err != nil {
		s.dbFailed(w, "GetGenres", err)
		return
	}
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
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	genre := queryFirst(r, "genre")
	if genre == "" {
		writeError(w, 10, "Required parameter is missing: genre")
		return
	}
	count := clamp(countParam(r, "count", 10), 0, 500) // count=0 stays 0 (#190)
	offset := max0(intParam(r, "offset", 0))
	if total, err := repoCount(r.Context(), s.Pool, "SELECT COUNT(*) FROM songs WHERE genre = ? COLLATE NOCASE", genre); err == nil {
		setTotal(w, total)
	}
	if headDone(w, r) {
		return
	}
	songs, err := repoSongsByGenre(r.Context(), s.Pool, genre, count, offset)
	if err != nil {
		s.dbFailed(w, "GetSongsByGenre", err)
		return
	}
	plays, err := s.playsFor(r.Context(), caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "GetSongsByGenre", err)
		return
	}
	children := make([]J, 0, len(songs))
	for _, sg := range songs {
		children = append(children, s.songToChild(sg, plays))
	}
	writeOK(w, J{"songsByGenre": J{"song": children}})
}

// ── Random + Starred ────────────────────────────────────────────────────────

func (s *State) GetRandomSongs(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	size := clamp(intParam(r, "size", 10), 0, 500) // size=0 stays 0 (#190)
	from := intParamPtr(r, "fromYear")
	to := intParamPtr(r, "toYear")
	genre := queryFirst(r, "genre")
	if total, err := repoCountRandomSongs(r.Context(), s.Pool, from, to, genre); err == nil {
		setTotal(w, total)
	}
	if headDone(w, r) {
		return
	}
	songs, err := repoRandomSongs(r.Context(), s.Pool, size, from, to, genre)
	if err != nil {
		s.dbFailed(w, "GetRandomSongs", err)
		return
	}
	plays, err := s.playsFor(r.Context(), caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "GetRandomSongs", err)
		return
	}
	children := make([]J, 0, len(songs))
	for _, sg := range songs {
		children = append(children, s.songToChild(sg, plays))
	}
	writeOK(w, J{"randomSongs": J{"song": children}})
}

// GetStarred answers the legacy getStarred: the same payload as getStarred2
// under the key "starred", which legacy clients expect (#196).
func (s *State) GetStarred(w http.ResponseWriter, r *http.Request) {
	s.writeStarred(w, r, "starred")
}

func (s *State) GetStarred2(w http.ResponseWriter, r *http.Request) {
	s.writeStarred(w, r, "starred2")
}

// writeStarred builds the starred lists and writes them under key.
func (s *State) writeStarred(w http.ResponseWriter, r *http.Request, key string) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	songs, err := repoStarredSongs(ctx, s.Pool, caller.ID)
	if err != nil {
		s.dbFailed(w, "GetStarred2", err)
		return
	}
	albums, err := repoStarredAlbums(ctx, s.Pool, caller.ID)
	if err != nil {
		s.dbFailed(w, "GetStarred2", err)
		return
	}
	artists, err := repoStarredArtists(ctx, s.Pool, caller.ID)
	if err != nil {
		s.dbFailed(w, "GetStarred2", err)
		return
	}
	setTotals(w, len(artists), len(albums), len(songs))
	if headDone(w, r) {
		return
	}
	p := pageParams(r)
	songs, albums, artists = pageSlice(songs, p), pageSlice(albums, p), pageSlice(artists, p)

	plain := make([]models.Song, 0, len(songs))
	for _, p := range songs {
		plain = append(plain, p.Song)
	}
	plays, err := s.playsFor(ctx, caller.ID, plain)
	if err != nil {
		s.dbFailed(w, "GetStarred2", err)
		return
	}
	songJSONs := make([]J, 0, len(songs))
	for _, p := range songs {
		v := s.songToChild(p.Song, plays)
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
		key: J{
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
		// Only an existing song, album or artist can be starred (#185):
		// junk ids would pile up per user and go out in the change feed.
		// Unstarring an unknown id stays allowed, so old junk can be removed.
		exists, xerr := repoLibraryItemExists(r.Context(), s.Pool, target)
		if xerr != nil {
			logging.Errorf("starOp: %v", xerr)
			writeError(w, 0, "database error")
			return
		}
		if !exists {
			writeError(w, 70, "Item not found")
			return
		}
		err = repoStar(r.Context(), s.Pool, caller.ID, target, nowISO8601())
	} else {
		err = repoUnstar(r.Context(), s.Pool, caller.ID, target)
	}
	if err != nil {
		logging.Errorf("starOp: %v", err)
		writeError(w, 0, "database error")
		return
	}
	writeOK(w, J{})
}

// ── Playback ────────────────────────────────────────────────────────────────

func (s *State) Stream(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	// Refused rather than queued: a waiting stream would hold its connection
	// open anyway.
	release, ok := s.streams.acquire(caller.ID, s.maxStreams, s.maxStreamsPerUser)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeErrorStatus(w, http.StatusServiceUnavailable, 0, "Too many streams are open; try again shortly")
		return
	}
	defer release()
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	path, contentType, found, err := s.lookupSong(r.Context(), id)
	if err != nil {
		logging.Errorf("stream lookup %s: %v", id, err)
		writeError(w, 0, "database error")
		return
	}
	if !found {
		writeError(w, 70, "Song not found")
		return
	}
	// Opened through the music folder's os.Root: a symlink that leads
	// outside the music folders, even one changed after the scan, is not
	// served (#177).
	f, err := scanner.OpenContained(s.MusicDirs, path)
	if err != nil {
		s.evictSong(id)
		logging.Errorf("stream open %s: %v", path, err)
		writeError(w, 0, "could not read file")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		logging.Errorf("stream stat %s: %v", path, err)
		writeError(w, 0, "could not read file")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(idleWriter{ResponseWriter: w, rc: http.NewResponseController(w)}, r, path, st.ModTime(), f)
}

func (s *State) GetCoverArt(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	// Every failure stays in the Subsonic envelope (#193); a miss keeps
	// HTTP 404 so a client does not take the body for an image.
	id := queryFirst(r, "id")
	if id == "" {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	ctx := r.Context()
	var filename string
	// A database error is a 500, not a 404: a client must not cache "no cover".
	var err error
	switch {
	case strings.HasPrefix(id, "al-"):
		var a *models.Album
		a, err = repoFindAlbum(ctx, s.Pool, id)
		if a != nil && a.CoverArt.Valid {
			filename = a.CoverArt.String
		}
	case strings.HasPrefix(id, "so-"):
		var song *models.Song
		song, err = repoFindSong(ctx, s.Pool, id)
		if err == nil && song != nil {
			if song.CoverArt.Valid {
				filename = song.CoverArt.String
			} else {
				var a *models.Album
				a, err = repoFindAlbum(ctx, s.Pool, song.AlbumID)
				if a != nil && a.CoverArt.Valid {
					filename = a.CoverArt.String
				}
			}
		}
	case strings.HasPrefix(id, "ar-"):
		var albums []models.Album
		albums, err = repoAlbumsByArtist(ctx, s.Pool, id)
		for _, a := range albums {
			if a.CoverArt.Valid {
				filename = a.CoverArt.String
				break
			}
		}
	}
	if err != nil {
		logging.Errorf("GetCoverArt %s: %v", id, err)
		writeErrorStatus(w, http.StatusInternalServerError, 0, "database error")
		return
	}
	if filename == "" {
		writeErrorStatus(w, http.StatusNotFound, 70, "Cover art not found")
		return
	}
	full := filepath.Join(s.CoversDir, filename)
	if _, err := os.Stat(full); err != nil {
		writeErrorStatus(w, http.StatusNotFound, 70, "Cover art not found")
		return
	}
	http.ServeFile(w, r, full)
}

// Scrobble counts plays (submission=true, the default) for the caller. The
// n-th id takes the n-th time, in ms since the epoch, when one is given; it
// falls back to the server clock. A now-playing report (submission=false) is
// accepted and not counted.
func (s *State) Scrobble(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ids := queryAll(r, "id")
	if len(ids) == 0 {
		writeError(w, 10, "Required parameter is missing: id")
		return
	}
	if sub := queryFirst(r, "submission"); sub != "" {
		if submit, err := strconv.ParseBool(sub); err == nil && !submit {
			writeOK(w, J{})
			return
		}
	}
	ctx := r.Context()
	now := time.Now().UnixMilli()
	times := queryAll(r, "time")
	playedMs := make([]int64, len(ids))
	for i := range ids {
		playedMs[i] = now
		if i < len(times) {
			t, err := strconv.ParseInt(times[i], 10, 64)
			if err != nil {
				writeError(w, 10, "Invalid parameter: time")
				return
			}
			if t > 0 {
				playedMs[i] = t
			}
		}
	}
	// Check every song before counting any, so a bad id records nothing.
	for _, id := range ids {
		song, err := repoFindSong(ctx, s.Pool, id)
		if err != nil {
			s.dbFailed(w, "scrobble", err)
			return
		}
		if song == nil {
			writeError(w, 70, "Song not found")
			return
		}
	}
	for i, id := range ids {
		// The retry key is the time as sent, so a retry dedupes even when
		// the stored time is clamped (#185).
		if err := repoRecordPlay(ctx, s.Pool, caller.ID, id, playedMs[i], clampPlayTime(playedMs[i], now), now); err != nil {
			s.dbFailed(w, "scrobble", err)
			return
		}
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
	caller, ok := s.requireCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// query is required (#195). An empty value, or "" as some clients send
	// it, is the documented browse mode: the whole library, paged, which
	// apps use to sync. A query with nothing searchable in it ("?!") finds
	// nothing, rather than the whole library.
	if _, present := params(r)["query"]; !present {
		writeError(w, 10, "Required parameter is missing: query")
		return
	}
	term := queryFirst(r, "query")
	if term == `""` {
		term = ""
	}
	if term != "" && fts5Match(term) == "" {
		setTotals(w, 0, 0, 0)
		if headDone(w, r) {
			return
		}
		writeOK(w, J{wrap: J{"artist": []J{}, "album": []J{}, "song": []J{}}})
		return
	}
	// The standard parameters win; size/offset (tinysonicPaging) apply to
	// every list that has no parameter of its own.
	artistLimit := countParam(r, "artistCount", 20)
	albumLimit := countParam(r, "albumCount", 20)
	songLimit := countParam(r, "songCount", 20)
	artistOffset := offsetParam(r, "artistOffset")
	albumOffset := offsetParam(r, "albumOffset")
	songOffset := offsetParam(r, "songOffset")

	na, err := repoCountSearch(ctx, s.Pool, "artists", term)
	if err != nil {
		s.dbFailed(w, "searchN", err)
		return
	}
	nb, err := repoCountSearch(ctx, s.Pool, "albums", term)
	if err != nil {
		s.dbFailed(w, "searchN", err)
		return
	}
	nc, err := repoCountSearch(ctx, s.Pool, "songs", term)
	if err != nil {
		s.dbFailed(w, "searchN", err)
		return
	}
	setTotals(w, na, nb, nc)
	if headDone(w, r) {
		return
	}

	artists, err := repoSearchArtists(ctx, s.Pool, term, artistLimit, artistOffset)
	if err != nil {
		s.dbFailed(w, "searchN", err)
		return
	}
	albums, err := repoSearchAlbums(ctx, s.Pool, term, albumLimit, albumOffset)
	if err != nil {
		s.dbFailed(w, "searchN", err)
		return
	}
	songs, err := repoSearchSongs(ctx, s.Pool, term, songLimit, songOffset)
	if err != nil {
		s.dbFailed(w, "searchN", err)
		return
	}
	plays, err := s.playsFor(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "searchN", err)
		return
	}

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
		songJSONs = append(songJSONs, s.songToChild(sg, plays))
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

func playlistJSON(pl *models.Playlist, t playlistTotals) J {
	return J{
		"id":        pl.ID,
		"name":      pl.Name,
		"comment":   nsToAny(pl.Comment),
		"songCount": t.songs,
		"duration":  t.durationMs / 1000,
		"public":    false, // playlists are private to their owner (#190)
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
	playlists, err := repoAllPlaylists(r.Context(), s.Pool, caller.ID)
	if err != nil {
		s.dbFailed(w, "GetPlaylists", err)
		return
	}
	setTotal(w, len(playlists))
	if headDone(w, r) {
		return
	}
	playlists = pageSlice(playlists, pageParams(r))
	totals, err := repoPlaylistTotals(r.Context(), s.Pool, caller.ID)
	if err != nil {
		s.dbFailed(w, "GetPlaylists", err)
		return
	}
	out := make([]J, 0, len(playlists))
	for i := range playlists {
		out = append(out, playlistJSON(&playlists[i], totals[playlists[i].ID]))
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
		logging.Errorf("getPlaylist: %v", err)
		writeError(w, 0, "database error")
		return
	}
	if pl == nil {
		writeError(w, 70, "Playlist not found")
		return
	}
	songs, err := repoPlaylistSongs(r.Context(), s.Pool, id)
	if err != nil {
		s.dbFailed(w, "GetPlaylist", err)
		return
	}
	setTotal(w, len(songs))
	if headDone(w, r) {
		return
	}
	var durMs int64
	for _, sg := range songs {
		durMs += sg.DurationMs
	}
	paged := pageSlice(songs, pageParams(r))
	plays, err := s.playsFor(r.Context(), caller.ID, paged)
	if err != nil {
		s.dbFailed(w, "GetPlaylist", err)
		return
	}
	entry := make([]J, 0, len(paged))
	for _, sg := range paged {
		entry = append(entry, s.songToChild(sg, plays))
	}
	writeOK(w, J{
		"playlist": J{
			"id":        pl.ID,
			"name":      pl.Name,
			"comment":   nsToAny(pl.Comment),
			"songCount": len(songs),
			"duration":  durMs / 1000,
			"public":    false, // playlists are private to their owner (#190)
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

	// Every step runs in one transaction and every failure is reported, so
	// a client never gets a success for an edit that was not stored, or a
	// half-applied one (#173).
	id := existingID
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if existingID == "" {
			id = newPlaylistID()
			if err := repoCreatePlaylist(ctx, tx, caller.ID, id, name, now); err != nil {
				return err
			}
			return repoAppendPlaylistSongs(ctx, tx, id, songIDs)
		}
		pl, err := repoFindPlaylist(ctx, tx, caller.ID, id)
		if err != nil {
			return err
		}
		if pl == nil {
			err = repoCreatePlaylist(ctx, tx, caller.ID, id, name, now)
		} else {
			err = repoRenamePlaylist(ctx, tx, caller.ID, id, name, now)
		}
		if err != nil || len(songIDs) == 0 {
			return err
		}
		return repoReplacePlaylistSongs(ctx, tx, id, songIDs)
	})
	if err != nil {
		s.dbFailed(w, "createPlaylist", err)
		return
	}

	pl, err := repoFindPlaylist(ctx, s.Pool, caller.ID, id)
	if err != nil || pl == nil {
		s.dbFailed(w, "createPlaylist", fmt.Errorf("playlist %s missing after commit: %v", id, err))
		return
	}
	songs, err := repoPlaylistSongs(ctx, s.Pool, id)
	if err != nil {
		s.dbFailed(w, "CreatePlaylist", err)
		return
	}
	var durMs int64
	plays, err := s.playsFor(ctx, caller.ID, songs)
	if err != nil {
		s.dbFailed(w, "CreatePlaylist", err)
		return
	}
	entry := make([]J, 0, len(songs))
	for _, sg := range songs {
		durMs += sg.DurationMs
		entry = append(entry, s.songToChild(sg, plays))
	}
	writeOK(w, J{
		"playlist": J{
			"id":        pl.ID,
			"name":      pl.Name,
			"comment":   nsToAny(pl.Comment),
			"songCount": len(entry),
			"duration":  durMs / 1000,
			"public":    false, // playlists are private to their owner (#190)
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
	pl, err := repoFindPlaylist(ctx, s.Pool, caller.ID, id)
	if err != nil {
		s.dbFailed(w, "UpdatePlaylist", err)
		return
	}
	if pl == nil {
		writeError(w, 70, "Playlist not found")
		return
	}
	idx := make([]int, 0)
	for _, raw := range queryAll(r, "songIndexToRemove") {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, 10, "songIndexToRemove must be a number")
			return
		}
		idx = append(idx, n)
	}
	now := nowISO8601()
	// One transaction, every error reported (#173). Removals use the indexes
	// of the playlist before this call, so they run before the additions.
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if v, ok := singleParam(r, "name"); ok {
			if err := repoRenamePlaylist(ctx, tx, caller.ID, id, v, now); err != nil {
				return err
			}
		}
		if v, ok := singleParam(r, "comment"); ok {
			if err := repoSetPlaylistComment(ctx, tx, caller.ID, id, v, now); err != nil {
				return err
			}
		}
		if len(idx) > 0 {
			current, err := repoPlaylistSongIDs(ctx, tx, id)
			if err != nil {
				return err
			}
			sort.Sort(sort.Reverse(sort.IntSlice(idx)))
			for _, i := range idx {
				if i >= 0 && i < len(current) {
					current = append(current[:i], current[i+1:]...)
				}
			}
			if err := repoReplacePlaylistSongs(ctx, tx, id, current); err != nil {
				return err
			}
		}
		return repoAppendPlaylistSongs(ctx, tx, id, queryAll(r, "songIdToAdd"))
	})
	if err != nil {
		s.dbFailed(w, "updatePlaylist", err)
		return
	}
	writeOK(w, J{})
}

// inTx runs fn in a transaction, committed when fn returns nil.
func (s *State) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
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
		logging.Errorf("deletePlaylist: %v", err)
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

// GetAlbumInfo answers getAlbumInfo; GetAlbumInfo2 the same under the key
// "albumInfo2" (#196).
func (s *State) GetAlbumInfo(w http.ResponseWriter, r *http.Request) {
	s.writeAlbumInfo(w, r, "albumInfo")
}

func (s *State) GetAlbumInfo2(w http.ResponseWriter, r *http.Request) {
	s.writeAlbumInfo(w, r, "albumInfo2")
}

func (s *State) writeAlbumInfo(w http.ResponseWriter, r *http.Request, key string) {
	if !s.requireAuth(w, r) {
		return
	}
	writeOK(w, J{
		key: J{
			"notes":          "",
			"musicBrainzId":  "",
			"lastFmUrl":      "",
			"smallImageUrl":  "",
			"mediumImageUrl": "",
			"largeImageUrl":  "",
		},
	})
}

// keep imports tidy when functions are unused in some build modes
var _ = errors.Is

// dbFailed answers a Subsonic request whose database read failed. The client
// gets an error, never an empty list it might act on (#110): a sync that reads
// "no songs" can delete local data.
func (s *State) dbFailed(w http.ResponseWriter, op string, err error) {
	logging.Errorf("%s: %v", op, err)
	writeError(w, 0, "database error")
}
