# tinysonic

A tiny, self-contained [Subsonic](http://www.subsonic.org/pages/api.jsp)-compatible
music server written in **pure Go** (no cgo). Point it at a folder of music, give
it an account (created with `tinysonic user add`), and any
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
$EDITOR smolsonic.toml      # set music_dir; optionally a bootstrap admin

# 2. Run
./tinysonic --config smolsonic.toml
```

On first launch tinysonic scans `music_dir`, creates the SQLite database, and
starts the HTTP server. Point any Subsonic client at
`http://<host>:<port>/rest/…` using the credentials from your TOML file.

## Configuration

`smolsonic.toml` (all keys shown):

```toml
music_dir         = "/path/to/your/music"    # required

# Optional. When both are set and no users exist yet, they create the first
# admin account. After that, manage users with `tinysonic user …`.
username          = "admin"
password          = "changeme"

# Optional — defaults shown
port              = 4533
host              = "0.0.0.0"
database_path     = "smolsonic.db"
covers_dir        = "covers"

# Public address clients reach the server at. Required for passkeys and for
# QR-code pairing. Must be HTTPS when served beyond localhost.
base_url          = "https://music.example.com"

# Let anyone create an account. New accounts stay pending until an active
# user approves them. Off by default.
allow_registration = false
```

Configuration is checked at startup:

- **Unknown keys are an error**, so a typo cannot silently disable a setting.
- **Relative paths** (`music_dir`, `database_path`, `covers_dir`) resolve from
  the directory containing the config file, not from the working directory.
  Use absolute paths if you start tinysonic from elsewhere.
- **Loose permissions** on the config file produce a warning, because it can
  hold a password. Fix with `chmod 600 smolsonic.toml`.
- Changes take effect after a restart. There are no environment variable
  overrides.

| Key                  | Purpose                                                              |
| -------------------- | -------------------------------------------------------------------- |
| `music_dir`          | Root of your library. Walked recursively.                            |
| `username`/`password`| Optional bootstrap admin, imported once when the database has no users. |
| `port`               | TCP port to bind.                                                    |
| `host`               | Interface to bind (use `127.0.0.1` to keep it local).                |
| `database_path`      | Path to the SQLite file. Created if missing. Holds users and keys.   |
| `covers_dir`         | Where extracted album art is cached.                                 |
| `base_url`           | Public URL clients use. Embedded in pairing QR codes; host is the passkey RP ID. |
| `allow_registration` | Enables `POST /api/register`. Default `false`.                       |

## CLI

```
tinysonic [--config FILE] [--no-scan] [--no-watch]    run the server

tinysonic user add [-c FILE] [--admin] <username>     create an active account (prompts for password)
tinysonic user list [-c FILE]                         list accounts and their status
tinysonic user approve [-c FILE] <username>           activate a pending account
tinysonic user disable|enable [-c FILE] <username>
tinysonic user set-password [-c FILE] <username>
tinysonic user remove [-c FILE] <username>

tinysonic pair [-c FILE] --user <username>            print a pairing QR code (valid 5 min, single use)
```

Account commands work directly on the database, so run them on the server
host. When stdin is not a terminal, the password is read from one line of stdin.

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

Every `/rest/*` request is authenticated in this order:

1. **API key** (`apiKey=<key>`): the Android app uses this. Keys are created by
   scanning a pairing QR code, or with `POST /api/keys`. A key has the same
   rights as its user and can be revoked.
2. **Session token** (`Authorization: Bearer <token>`): issued by a passkey or
   password sign-in on the web page at `/login`.
3. **Subsonic credentials**: token auth (`u`, `t = md5(password + salt)`, `s`)
   or plaintext (`u`, `p`, or `p=enc:<hex>`). Stock Subsonic clients use this.

Credentials may be sent in the query string or as a form-encoded POST body.
Only active accounts authenticate; pending and disabled accounts are refused.

Passwords are stored so that token auth can verify them, so anyone with read
access to the database file can read them. Protect the database file, and use
passkeys for the web UI.

### Accounts and pairing

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| POST | `/api/register` | none | Create a pending account (needs `allow_registration`) |
| GET | `/api/users` | user | List accounts |
| POST | `/api/users/{id}/approve` | user | Activate a pending account |
| POST | `/api/users/{id}/reject` | user | Delete a pending account |
| POST | `/api/keys` | user | Create an API key (shown once) |
| GET | `/api/keys` | user | List own keys |
| DELETE | `/api/keys/{id}` | user | Revoke an own key |
| POST | `/api/pair/start` | user | Create a pairing token and QR payload |
| POST | `/api/pair/complete` | token | App redeems a pairing token for an API key |
| POST | `/api/passkey/register/begin` · `/finish` | user | Enrol a passkey |
| POST | `/api/passkey/login/begin` · `/finish` | none | Sign in with a passkey |
| POST | `/api/login` | none | Sign in with username and password |
| POST | `/api/logout` | Bearer | End a session |

The QR code encodes `tinysonic://pair?server=<base_url>&token=<token>`. The app
posts the token to `/api/pair/complete` and stores the returned key.

Passkeys need HTTPS (or `localhost`) and `base_url`. Without `base_url` the
passkey endpoints return 503, and every other method keeps working.

## Running over HTTPS

tinysonic serves plain HTTP. Subsonic clients send the password (or a token
derived from it) on every request, and passkeys require HTTPS. Put tinysonic
behind a TLS-terminating reverse proxy (nginx, Caddy, Traefik) when it is
reachable from outside a trusted network, bind it to `127.0.0.1` so only the
proxy can reach it, and set `base_url` to the proxy's public address.

Request logs record the path only; credentials in the query string or form
body are never written to the log.

## Deployment

Example files are in [`deploy/`](deploy/):

- `tinysonic.service`: systemd unit with a dedicated `tinysonic` user, a state
  directory under `/var/lib/tinysonic`, and sandboxing. The install steps are
  in the file header. Verify with `systemd-analyze verify deploy/tinysonic.service`.
- `nginx.conf`: TLS reverse proxy with streaming enabled and the same 1 MiB
  body limit as tinysonic.
- `Caddyfile`: the same proxy with automatic certificates.

Set `base_url` to the public address the proxy serves (for example
`https://music.example.com`), and keep `host` at `127.0.0.1` so only the proxy
can reach tinysonic.

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
