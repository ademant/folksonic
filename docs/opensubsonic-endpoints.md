# Supported OpenSubsonic endpoints

tinysonic implements part of the Subsonic/OpenSubsonic API. This file lists
which endpoints are implemented and which are not. Until the full API is
implemented, treat this list as the authority; the README links here.

Every implemented endpoint is registered at both `/rest/<name>` and
`/rest/<name>.view`, for `GET` and `POST`. Responses use the Subsonic JSON
envelope and carry `"openSubsonic": true`.

Optional fields without a value are omitted rather than sent as `null`.
Songs carry the standard `replayGain` object and `bpm` field when the files
have the values (BPM can also be calculated on the library page).
Song `path` is relative to its music directory, `created` is when a file was
first added, and `getAlbumList2?type=newest` sorts by that date. See the
README's "Response conventions".

## Implemented (43)

Endpoints marked *stub* return the expected shape with minimal content. They
make no Last.fm or other external lookups.

**System**
- `ping`
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
- `getRandomSongs`
- `getStarred`, `getStarred2`

**Playback**
- `stream`, `download`
- `getCoverArt`
- `scrobble`
- `getNowPlaying`
- `updateNowPlaying` (not in the OpenSubsonic list checked; kept as registered)

**Search**
- `search2`, `search3`

**Playlists**
- `getPlaylists`, `getPlaylist`, `createPlaylist`, `updatePlaylist`, `deletePlaylist`

**Starring**
- `star`, `unstar`

**tinysonic vendor extensions** (see below)
- `tinysonicLibraryInfo`

**Artist and album information** (stubs)
- `getArtistInfo`, `getArtistInfo2`
- `getAlbumInfo`, `getAlbumInfo2`
- `getSimilarSongs`, `getSimilarSongs2`
- `getTopSongs`
- `getLyrics`

## Not implemented (46)

These names appear in the OpenSubsonic endpoint list checked for this file, and
tinysonic does not register them. A request for any of them gets HTTP 404 with
a plain-text body, not a Subsonic response.

- `addChatMessage`
- `changePassword`
- `createBookmark`
- `createInternetRadioStation`
- `createPodcastChannel`
- `createShare`
- `createUser`
- `deleteBookmark`
- `deleteInternetRadioStation`
- `deletePodcastChannel`
- `deletePodcastEpisode`
- `deleteShare`
- `deleteUser`
- `downloadPodcastEpisode`
- `findSonicPath`
- `getAvatar`
- `getBookmarks`
- `getCaptions`
- `getChatMessages`
- `getInternetRadioStations`
- `getLicense`
- `getLyricsBySongId`
- `getNewestPodcasts`
- `getPlayQueue`
- `getPlayQueueByIndex`
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
- `savePlayQueue`
- `savePlayQueueByIndex`
- `search`
- `setRating`
- `tokenInfo`
- `updateInternetRadioStation`
- `updateShare`
- `updateUser`

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
| `tinysonicMeter` v1 | `tinysonicMeter` on songs: the text of the `METER` / `TIME_SIGNATURE` / `TIMESIGNATURE` tag, e.g. `3/4`. Kept in minimal mode. |
| `tinysonicSongTags` v1 | `tinysonicTags` on songs, when `song_tags` is configured. |
