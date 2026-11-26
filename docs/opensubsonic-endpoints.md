# Supported OpenSubsonic endpoints

tinysonic implements part of the Subsonic/OpenSubsonic API. This file lists
which endpoints are implemented and which are not. Until the full API is
implemented, treat this list as the authority; the README links here.

Every implemented endpoint is registered at both `/rest/<name>` and
`/rest/<name>.view`, for `GET` and `POST`. Responses use the Subsonic JSON
envelope and carry `"openSubsonic": true`.

Endpoints that change state (`startScan`, `scrobble`, `updateNowPlaying`,
`createPlaylist`, `updatePlaylist`, `deletePlaylist`, `star`, `unstar`,
`setRating`, `createBookmark`, `deleteBookmark`, `savePlayQueue`,
`savePlayQueueByIndex`) take `POST`, and `GET` as the compatibility alias
nearly every Subsonic client uses. Other endpoints also take `HEAD`. Any
other method, and `HEAD` on an endpoint that changes state, gets HTTP 405
with `Allow`, in the envelope (#192).

A numeric parameter (`size`, `count`, `offset`, `fromYear`, `toYear`, and the
`…Count`/`…Offset` of `search3`) that is not a whole number gets error 10.
`size=0` or `count=0` is an empty list (#190).

Optional fields without a value are omitted rather than sent as `null`.
Songs carry the standard `replayGain` object and `bpm` field when the files
have the values (BPM can also be calculated on the library page). They also
carry `playCount` (0 when the caller has no plays) and, once there is one,
`played`: the caller's own plays, counted by `scrobble`. See the README's
"Play counts".
Song `path` is relative to its music directory, `created` is when a file was
first added, and `getAlbumList2?type=newest` sorts by that date. See the
README's "Response conventions".

## Implemented (55)

Endpoints marked *stub* return the expected shape with minimal content. They
make no Last.fm or other external lookups.

**System**
- `ping`
- `getLicense` (always valid, no expiry)
- `tokenInfo` (the apiKeyAuthentication extension: the username of the key that signed the request; error 44 without a valid key)
- `getUser`
- `getMusicFolders`
- `getScanStatus`
- `startScan` (librarians only)
- `getOpenSubsonicExtensions` (no sign-in required)

**Library**
- `getArtists`, `getArtist`
- `getAlbum`, `getAlbumList`, `getAlbumList2`
- `getSong`
- `getIndexes`, `getMusicDirectory`

**Genres**
- `getGenres`, `getSongsByGenre`

**Lists**
- `getRandomSongs` (filters genre, fromYear and toYear)
- `getStarred`, `getStarred2`
- `getAlbumList2` and `getAlbumList` types: alphabeticalByName, alphabeticalByArtist, newest, random, frequent and recent (the caller's own plays), byYear (fromYear and toYear; reversed when fromYear is greater) and byGenre (genre, case-insensitive). starred (the caller's starred albums, newest star first) and highest (albums with songs the caller rated, by their average rating; setRating rates songs). An unknown type, or a missing fromYear, toYear or genre, gets error 10.

**Lyrics**
- `getLyrics` (the one song with this artist and title, ignoring case; see *Deviations*)
- `getLyricsBySongId` (OpenSubsonic extension songLyrics, version 1: unsynchronised lines only)

**Similar and top songs**
- `getSimilarSongs`, `getSimilarSongs2`: songs whose BPM is within 3 % of the seed song's, nearest first; empty when the seed has no BPM (see *Deviations*)
- `getTopSongs`: the artist's songs by the caller's play count (see *Deviations*)

**Playback**
- `stream`, `download`
- `getCoverArt`
- `scrobble` (counts plays for the caller; `submission=false` is accepted and not counted)
- `getNowPlaying` (the caller's own players only, see *Deviations*)
- `updateNowPlaying` (not in the OpenSubsonic list checked; kept as registered)

**Search**
- `search2`, `search3`

**Playlists**
- `getPlaylists`, `getPlaylist`, `createPlaylist`, `updatePlaylist`, `deletePlaylist`

**Starring**
- `star`, `unstar`

**Personal rating** (0 to 5, separate from stars and from shoes; #140)
- `setRating` (0 removes the rating; songs only, so id must be a song)

**Bookmarks** (one per user and song)
- `getBookmarks`, `createBookmark`, `deleteBookmark`

**Play queue** (one per user)
- `getPlayQueue`, `savePlayQueue`
- `getPlayQueueByIndex`, `savePlayQueueByIndex` (OpenSubsonic indexBasedQueue extension)

**tinysonic vendor extensions** (see below)
- `tinysonicLibraryInfo`
- `getLibraryChanges` (extension tinysonicLibraryChanges)

**Artist and album information** (stubs)
- `getArtistInfo`, `getArtistInfo2`
- `getAlbumInfo`, `getAlbumInfo2`

## Not implemented (35)

These names appear in the OpenSubsonic endpoint list checked for this file, and
tinysonic does not register them. A request for any of them, or any other
unknown `/rest` path, gets HTTP 404 with the Subsonic envelope and error 70
(#193).

- `addChatMessage`
- `changePassword`
- `createInternetRadioStation`
- `createPodcastChannel`
- `createShare`
- `createUser`
- `deleteInternetRadioStation`
- `deletePodcastChannel`
- `deletePodcastEpisode`
- `deleteShare`
- `deleteUser`
- `downloadPodcastEpisode`
- `findSonicPath`
- `getAvatar`
- `getCaptions`
- `getChatMessages`
- `getInternetRadioStations`
- `getNewestPodcasts`
- `getPodcastEpisode`
- `getPodcasts`
- `getShares`
- `getSonicSimilarTracks`
- `getTranscodeDecision`
- `getTranscodeStream`
- `getUsers`
- `getVideoInfo`
- `getVideos`
- `hls`
- `jukeboxControl`
- `refreshPodcasts`
- `reportPlayback`
- `search`
- `updateInternetRadioStation`
- `updateShare`
- `updateUser`

## Deviations from the OpenSubsonic description

These follow the decisions in the issues and differ from the endpoint pages.

- `getSimilarSongs2` takes a **song** id as the seed. The description takes an artist id.
- `getNowPlaying` lists only the **caller's** players. The description lists what every user plays.
- `getTopSongs` requires `artist`. The `id` form of the `topSongsByArtistId` extension is not implemented, and songs never played are left out instead of falling back to search order.
- `playerId` in `updateNowPlaying` and `getNowPlaying` is sent back as a string.
- `getLyrics` answers without a value when two or more songs share the artist and title, or when the one song has no lyrics. Lyrics are not searched by any other key.
- `getLyricsBySongId` reports every lyrics text as unsynchronised, including LRC text with timestamps, and always gives the language as `und`, since the tags are read for their text only.
- `getAlbumList2` and `getAlbumList` answer error 10 for an unknown `type`. Earlier versions listed albums alphabetically instead.
- `getUser` reports `adminRole: true` for a librarian. A librarian may read another account with `username`; anyone else gets error 50 for an account not their own (#190).
- Playlists report `public: false`: they are read by their owner only, and there is no sharing (#190).
- `search2` and `search3` need `query` (error 10 without it). An empty value, or `""`, lists the whole library, paged, which apps use to sync; a query with no letters or digits finds nothing (#195).
- `getCoverArt` without `id` is error 10; an unknown id, or one without a cover, is HTTP 404 with error 70 in the envelope (#193).

## Source and limits

The list above was compiled from the OpenSubsonic endpoint documentation at
https://opensubsonic.netlify.app/docs/endpoints/, read when this file was
written. That page lists 87 endpoint names. The OpenSubsonic site groups them
into categories (Bookmarks, Browsing, Chat, Internet Radio, Jukebox, Lists,
Media Annotation, Media Library Scanning, Media Retrieval, Playlists, Podcast,
Searching, Sharing, System, Transcoding, User Management); this file does not
reproduce those groupings. The implemented list is checked against the code by
`go test`: a registered name that is not in the list is reported.

## Vendor extensions

Advertised by `getOpenSubsonicExtensions`; details in the README.

| Extension | What it adds |
|---|---|
| `tinysonicPaging` v1 | `size` (max 500) / `offset` on every list endpoint and nested list; aliases on `getSongsByGenre`, `search2/3`, `getStarred(2)` (standard parameters win); `X-Total-Count` or `X-Total-Count-Artist`/`-Album`/`-Song` on every response; `HEAD` returns the headers only; stable order with id as tie-breaker. |
| `tinysonicLibraryInfo` v1 | Endpoint `tinysonicLibraryInfo`: `artists`, `albums`, `songs`, `duration`, `lastModified` (last library change). |
| `tinysonicMinimal` v1 | `Prefer: tinysonic-minimal` removes optional derivable fields. Song: `parent` (kept in folder browsing), `isVideo`, `type`, `coverArt`, `path`, `suffix`. Album: `title`, `coverArt`. Artist: `coverArt`. Response header `Preference-Applied: tinysonic-minimal`; `Vary: Prefer` on all JSON responses. |
| `tinysonicLibraryChanges` v1 | Endpoint `getLibraryChanges?since=<ms>`: ids of artists, albums, songs, playlists and starred items changed since `since` (`{id, updated}`), and `deleted` tombstones (`{type, id, deleted}`), kept 90 days. `full: true` when `since` is missing or older than the floor, or the library was reset. `size` and `cursor` page one snapshot. Playlists and starred items are per account. `serverTime` lags the clock by 2 s. |
| `userRating` (songs) | The caller's own star rating, 1 to 5, set with `setRating`. Absent when unrated. It is personal and not shared; it is not the dancing shoes value, which stays on DanceBrainz. |
| `tinysonicMeter` v1 | `tinysonicMeter` on songs: the text of the `METER` / `TIME_SIGNATURE` / `TIMESIGNATURE` tag, e.g. `3/4`. Kept in minimal mode. |
| `tinysonicTempo` v1 | `tinysonicTempo` on songs, each key only when known: `confidence` (0–1, of a calculated `bpm`), `barsPerMin`, `barPulses`. Ignore unknown keys; new ones raise the version. Kept in minimal mode. |
| `tinysonicSongTags` v1 | `tinysonicTags` on songs, when `song_tags` is configured. |
| `tinysonicStreamUrl` v1 | `GET /api/stream-url?id=` (session or device grant) returns a signed `/rest/stream.view` link for one song, valid 2 hours. Accepted only on `stream` and `download`. |
| `tinysonicDeviceGrant` v1 | Our apps sign in with a short code (RFC 8628) and get DPoP-bound tokens: `/api/device/authorize`, `approve`, `token`, `refresh`, `revoke`. The access token is sent as `Authorization: DPoP` with a proof from the same key, valid 15 minutes, for `/rest/*` and `/api/*`. Subsonic clients do not use it. |
