# tinysonic

A tiny, self-contained [Subsonic](http://www.subsonic.org/pages/api.jsp)-compatible
music server written in **pure Go** (no cgo). Point it at a folder of music, give
it a username and a password in a TOML file, and any
[Subsonic client](https://www.navidrome.org/apps/) can browse and stream your library.

```
 _   _                             _
| |_(_)_ __  _   _ ___  ___  _ __ (_) ___
| __| | '_ \| | | / __|/ _ \| '_ \| |/ __|
| |_| | | | | |_| \__ \ (_) | | | | | (__
 \__|_|_| |_|\__, |___/\___/|_| |_|_|\___|
             |___/    a tiny Subsonic server in Go
```

This is a pure-Go port of [smolsonic](https://github.com/tsirysndr/smolsonic).
Same TOML config, same SQLite schema, same Subsonic endpoints — no C
dependencies, so `CGO_ENABLED=0 go build` produces a fully static binary you can
drop on any Linux/macOS/Windows host.

## Features

- One binary, one TOML file, one SQLite database. No external services.
- Built on Go's `net/http` with [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)
  (pure-Go SQLite, FTS5 enabled).
- Library scanner powered by [dhowden/tag](https://pkg.go.dev/github.com/dhowden/tag)
  — extracts ID3/Vorbis/MP4 tags and embedded cover art from `mp3`, `flac`,
  `ogg`, `opus`, `m4a`, `wav`, and more.
- Duration + bitrate probed per format with pure-Go decoders
  ([tcolgate/mp3](https://pkg.go.dev/github.com/tcolgate/mp3),
  [mewkiz/flac](https://pkg.go.dev/github.com/mewkiz/flac),
  [abema/go-mp4](https://pkg.go.dev/github.com/abema/go-mp4),
  [go-audio/wav](https://pkg.go.dev/github.com/go-audio/wav),
  inline OGG container parser for Vorbis + Opus).
- Falls back to `cover.jpg` / `folder.jpg` / `front.jpg` next to the audio file
  if there's no embedded picture.
- Stable IDs (`ar-…` / `al-…` / `so-…`) derived from tag content, so re-scans
  are idempotent and clients don't lose their bookmarks.
- HTTP `Range` support for proper seeking (via `http.ServeContent`).
- Subsonic **token auth** (`t = md5(password + salt)`) and plaintext (`p=…`
  or `p=enc:<hex>`) both supported.
- CORS is permissive — works directly from web clients.

## Install

Build from source:

```sh
go build -o tinysonic ./cmd/tinysonic
```

For a fully static binary (handy for containers):

```sh
CGO_ENABLED=0 go build -o tinysonic ./cmd/tinysonic
```

## Quick start

```sh
# 1. Create a config
cp smolsonic.example.toml smolsonic.toml
$EDITOR smolsonic.toml      # set music_dir, username, password

# 2. Run
./tinysonic --config smolsonic.toml
```

On first launch tinysonic scans `music_dir`, creates the SQLite database, and
starts the HTTP server. Point any Subsonic client at
`http://<host>:<port>/rest/…` using the credentials from your TOML file.

## Configuration

`smolsonic.toml` (all keys shown):

```toml
music_dir     = "/path/to/your/music"   # required
username      = "admin"                  # required
password      = "changeme"               # required

# Optional — defaults shown
port          = 4533
host          = "0.0.0.0"
database_path = "smolsonic.db"
covers_dir    = "covers"
```

| Key             | Purpose                                                   |
| --------------- | --------------------------------------------------------- |
| `music_dir`     | Root of your library. Walked recursively.                 |
| `username`      | The single Subsonic user.                                 |
| `password`      | Cleartext on disk; used for both token and plaintext auth.|
| `port`          | TCP port to bind.                                         |
| `host`          | Interface to bind (use `127.0.0.1` to keep it local).     |
| `database_path` | Path to the SQLite file. Created if missing.              |
| `covers_dir`    | Where extracted album art is cached.                      |

## CLI

```
Usage: tinysonic [OPTIONS]

Options:
  -c, --config <CONFIG>  Path to the TOML config file (default: smolsonic.toml)
      --no-scan          Skip the startup library scan
```

Trigger a rescan from a running server with the standard Subsonic endpoint:

```
GET /rest/startScan.view?u=…&t=…&s=…
GET /rest/getScanStatus.view?u=…&t=…&s=…
```

## Supported endpoints

Full navidrome-style endpoint coverage. Both `.view`-suffixed and plain paths
are accepted, on both `GET` and `POST`. Responses are JSON with the Subsonic
envelope (`{"subsonic-response": …}`).

**System** — `ping`, `getUser`, `getMusicFolders`, `getScanStatus`, `startScan`

**Library (ID3 tag browsing)** — `getArtists`, `getArtist`, `getAlbum`,
`getSong`, `getAlbumList2`, `getAlbumList` (alias)

**Library (folder browsing)** — `getIndexes`, `getMusicDirectory`

**Genres** — `getGenres`, `getSongsByGenre`

**Lists** — `getRandomSongs`, `getStarred2`, `getStarred` (alias)

**Playback** — `stream`, `download`, `getCoverArt`, `scrobble`,
`getNowPlaying`, `updateNowPlaying`

**Search** — `search3`, `search2` (alias)

**Playlists** — `getPlaylists`, `getPlaylist`, `createPlaylist`,
`updatePlaylist`, `deletePlaylist`

**Starring** — `star`, `unstar`

**Artist / album info** — `getArtistInfo`, `getArtistInfo2`, `getAlbumInfo`,
`getAlbumInfo2`, `getSimilarSongs`, `getSimilarSongs2`, `getTopSongs`,
`getLyrics`. These return minimal stub shapes (no Last.fm or external lookups).

`GET /` returns a plain-text index of every endpoint and its query params.

## Tested clients

Anything that speaks Subsonic API 1.16.x and prefers tag-based browsing should
work, including Substreamer, play:sub, Symfonium, Tempo, and Sonixd.

## How auth works

Subsonic clients send credentials as query parameters:

- **Token auth** (preferred): `u=<user>&t=<token>&s=<salt>` where
  `token = md5(password + salt)`.
- **Plaintext**: `u=<user>&p=<password>` or `p=enc:<hex-encoded password>`.

`tinysonic` accepts both. The single user/password come from the TOML config.
There's no user database.

## Running over HTTPS

tinysonic serves plain HTTP. Subsonic clients send the password (or a token
derived from it) on every request, so anyone on the network path can read it.
Put tinysonic behind a TLS-terminating reverse proxy (nginx, Caddy, Traefik)
when it is reachable from outside a trusted network, and bind it to
`127.0.0.1` so only the proxy can reach it.

Request logs record the path only; credentials in the query string or form
body are never written to the log.

## Format coverage

Tag extraction (title, artist, album, genre, year, track, disc, embedded art)
covers everything dhowden/tag supports: `mp3`, `m4a`/`mp4`/`aac`/`alac`,
`flac`, `ogg` (Vorbis).

Duration + bitrate are decoded per format:

| Format             | Duration / bitrate |
| ------------------ | ------------------ |
| `mp3`              | ✅ frame scan      |
| `flac`             | ✅ StreamInfo      |
| `ogg` (Vorbis)     | ✅ OGG granule     |
| `opus`             | ✅ OGG granule (48 kHz) |
| `m4a`/`mp4`/`aac`/`alac` | ✅ moov/mvhd  |
| `wav`              | ✅ format chunk    |
| `aiff`/`wma`/`ape`/`wv`/`mpc` | ⚠️ duration shown as 0 — files still index + stream fine |

For the unsupported formats the file is still registered and streamed; only the
UI duration display degrades.

## Development

```sh
go run ./cmd/tinysonic --config smolsonic.toml
go test ./...
go vet ./...
```

Project layout:

```
cmd/tinysonic/main.go          entry point
internal/
  cli/                         flag parsing, synthwave banner
  config/                      TOML loader
  db/                          schema + migrations + FTS5 backfill
  models/                      Artist / Album / Song / Playlist row types
  audioinfo/                   per-format duration + bitrate probers
  scanner/                     filepath.WalkDir + tag extraction + cover art
  server/
    server.go                  net/http server, routing, CORS
    auth.go                    Subsonic token / plaintext auth
    response.go                JSON envelope helpers
    repo.go                    database/sql queries
    handlers.go                Subsonic endpoint handlers
```

## License

MIT.
