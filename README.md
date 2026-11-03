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
$EDITOR smolsonic.toml      # set music_dirs; optionally a bootstrap account

# 2. Run
./tinysonic --config smolsonic.toml
```

On first launch tinysonic scans `music_dirs`, creates the SQLite database, and
starts the HTTP server. Point any Subsonic client at
`http://<host>:<port>/rest/…` using the credentials from your TOML file.

## Configuration

`smolsonic.toml` (all keys shown):

```toml
music_dirs       = ["/path/to/your/music", "/mnt/nas/albums"]   # required

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

# Let anyone create an account. New accounts stay pending until an
# administrator approves them with `tinysonic user approve`. Off by default.
allow_registration = false
```

Configuration is checked at startup:

- **Unknown keys are an error**, so a typo cannot silently disable a setting.
- **Relative paths** (`music_dirs`, `database_path`, `covers_dir`) resolve from
  the directory containing the config file, not from the working directory.
  Use absolute paths if you start tinysonic from elsewhere.
- **Loose permissions** on the config file produce a warning, because it can
  hold a password. Fix with `chmod 600 smolsonic.toml`.
- Changes take effect after a restart. There are no environment variable
  overrides.

| Key                  | Purpose                                                              |
| -------------------- | -------------------------------------------------------------------- |
| `music_dirs`         | Library roots, each walked recursively. `music_dir = "…"` is accepted as a one-entry list. |
| `username`/`password`| Optional bootstrap librarian, imported once when the database has no users. |
| `port`               | TCP port to bind.                                                    |
| `host`               | Interface to bind (use `127.0.0.1` to keep it local).                |
| `database_path`      | Path to the SQLite file. Created if missing. Holds users and keys.   |
| `covers_dir`         | Where extracted album art is cached.                                 |
| `base_url`           | Public URL clients use. Embedded in pairing QR codes; host is the passkey RP ID. |
| `base_path`         | URL prefix to serve under, such as `/folksonic`. Empty (default) serves from the root. See [Serving under a subpath](#serving-under-a-subpath). |
| `allow_registration` | Enables `POST /api/register` without an invitation code. Default `false`. Invitation codes work either way. |
| `song_tags`          | File tags exposed on songs, such as `["DANCE", "USE"]`. Only these names are read. Each song then carries `tinysonicTags`, and the `tinysonicSongTags` extension is advertised. Default empty. |
| `[oidc]` table       | Optional single sign-on with an OpenID Connect provider: `issuer`, `client_id` and optional `client_secret`. Needs `base_url`. See [Single sign-on](#single-sign-on). Off unless `issuer` is set. |

## Roles

There are two roles:

- **`librarian`** may start a library rescan (`startScan`). Nothing else in
  the API is role-gated.
- **`user`** has no extra rights. Stars, playlists and API keys belong to each
  account.

Account administration is CLI-only, because it needs shell access to the
server: creating, approving, disabling and removing accounts, setting
passwords, issuing invites and pairing. Pending registrations are approved
with `tinysonic user approve` and rejected with `tinysonic user remove`. The
API has no endpoint for either. Subsonic `getUser` reports `adminRole` as
false for everyone.

## CLI

```
tinysonic [--config FILE] [--no-scan] [--no-watch]    run the server

tinysonic user add [-c FILE] [--librarian] <username>  create an active account (prompts for password)
tinysonic user list [-c FILE]                         list accounts and their status
tinysonic user approve [-c FILE] <username>           activate a pending account
tinysonic user disable|enable [-c FILE] <username>
tinysonic user set-password [-c FILE] <username>
tinysonic user remove [-c FILE] <username>

tinysonic invite create [-c FILE] [--expires 7d] [--librarian] [--note TEXT] [--username NAME]
                                                      print a single-use invitation code; --username
                                                      binds it to one name, for a passkey-only account
tinysonic invite list [-c FILE]                       list invites and their state (never the codes)
tinysonic invite revoke [-c FILE] <id>                withdraw an unused invite

tinysonic pair [-c FILE] --user <username>            print a pairing QR code (valid 5 min, single use)
```

Account commands work directly on the database, so run them on the server
host. When stdin is not a terminal, the password is read from one line of stdin.

### Invitations

An invitation code lets one person create their own account, without
`allow_registration` and without approval:

```
$ tinysonic invite create --note "for Anna" -c /etc/tinysonic/tinysonic.toml
invite 3 (user), valid until 2026-11-10T09:00:00Z, single use
inv-7KQ2-M9XD-4TPA-HR3C
https://cloud.ademant.de/folksonic/login?invite=inv-7KQ2-M9XD-4TPA-HR3C
```

The link opens the sign-in page with the registration form filled in. The
account is active as soon as it is created, with the invite's role (`--librarian`
for a librarian).

An invite can be redeemed in two ways. With a password, the account keeps that
password. Without one, the user creates a passkey, and the account is created
only once the passkey is verified; it has no password, so it signs in with the
passkey alone and reaches Subsonic apps through an API key from QR pairing.
`--username NAME` binds the invite to one username, which is how an admin
provisions a passkey-only account. An account without a password always has a
passkey (or, later, another strong method); there is no way to create one
without. Codes are single use, expire after 7 days unless
`--expires` says otherwise (`3d`, `12h`), and are stored hashed, so a lost code
cannot be shown again: revoke it and create a new one. Case and dashes do not
matter when typing a code.

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
(librarians only), `getOpenSubsonicExtensions`

**MusicBrainz IDs** — songs, albums and artists carry `musicBrainzId` and
`mediaType` (`song`, `album` or `artist`), read from the files'
`MUSICBRAINZ_TRACKID`, `MUSICBRAINZ_ALBUMID` and `MUSICBRAINZ_ARTISTID` tags
(or the `MusicBrainz Track Id` style fields in MP3 and M4A). The value is empty
when the file has none, and only well-formed UUIDs are kept. Upgrading an
existing library rescans every file once at the next start, so the IDs fill in
without a manual rescan.

**OpenSubsonic** — responses carry `"openSubsonic": true`.
`getOpenSubsonicExtensions` lists the extensions the server implements:
`apiKeyAuthentication` and `formPost`, plus `tinysonicSongTags` when `song_tags`
is set. It needs no sign-in.

**Song tags (vendor extension `tinysonicSongTags`)** — with `song_tags` set,
each song carries `tinysonicTags`, an object mapping each configured name to all
of its values in file order, for example `{"DANCE": ["Waltz", "Mazurka"]}`. Names
are case-insensitive in the file and upper-cased in the response. Only the listed
names are read, and they are read-only: the server never writes tags.

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

Passkey-only accounts have no password. They cannot use the password parameter
or token auth, and an empty password never matches.

### Accounts and pairing

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| POST | `/api/register` | none | Create an account. With `invite` and `password`: active and signed in (returns a session `token`). Without an invite: pending, needs `allow_registration`, and needs a password |
| POST | `/api/keys` | user | Create an API key (shown once) |
| GET | `/api/keys` | user | List own keys |
| DELETE | `/api/keys/{id}` | user | Revoke an own key |
| POST | `/api/pair/start` | user | Create a pairing token and QR payload |
| POST | `/api/pair/complete` | token | App redeems a pairing token for an API key |
| POST | `/api/passkey/register/begin` · `/finish` | user | Enrol a passkey |
| POST | `/api/passkey/login/begin` · `/finish` | none | Sign in with a passkey |
| POST | `/api/register/passkey/begin` · `/finish` | none | Sign up with an invite and a passkey, no password. The account is created only after the passkey is verified |
| POST | `/api/login` | none | Sign in with username and password |
| POST | `/api/logout` | Bearer | End a session |
| GET | `/account` | page | Account page: API keys, pairing QR, passkeys and linked identities. Redirects to sign-in when there is no session |
| POST | `/api/pair/qr` | user | Create a pairing token and return its QR code, SVG by default; PNG with `Accept: image/png` or `?format=png` |
| GET | `/api/passkeys` | user | List own passkeys (`id`, `label`, `createdAt`) |
| DELETE | `/api/passkeys/{id}` | user | Remove an own passkey. Refused (409) when it is the account's last sign-in method |
| GET | `/api/oidc/status` | none | `{"enabled": bool}`, so the page knows whether to offer single sign-on |
| POST | `/api/oidc/begin` | none (`link` needs user) | Start single sign-on. Body: `mode` (`signin`, `link` or `invite`), `client` (`web` or `app`), and `invite` and `username` for `invite`. Returns the provider's `url` |
| GET | `/api/oidc/callback` | state | Provider redirect target. Ends in the sign-in page with a one-time code, or with an error |
| POST | `/api/oidc/exchange` | one-time code | Trade the code for a session `token` (web) or a device `apiKey` (app) |
| GET | `/api/oidc/identities` | user | List linked identities |
| DELETE | `/api/oidc/identities/{id}` | user | Unlink an identity. Refused (409) when it is the account's last sign-in method |

The QR code encodes `tinysonic://pair?server=<base_url>&token=<token>`. The app
posts the token to `/api/pair/complete` and stores the returned key.

Passkeys need HTTPS (or `localhost`) and `base_url`. Without `base_url` the
passkey endpoints return 503, and every other method keeps working.

### Account page

`/account` is the place for a signed-in user to connect the Android app and
manage sign-in. After sign-in the browser goes there. It offers:

- **Pairing QR**, generated only when the button is pressed. It is an SVG
  (PNG as fallback) that works once and expires after five minutes.
- **API keys**: create one with a label and an optional expiry (shown once),
  list them, and revoke them.
- **Passkeys**: add one with a name, list them, and remove them.
- **Single sign-on identities**, when `oidc` is configured: link and unlink.

The page keeps the session in `sessionStorage` and calls the JSON API with a
Bearer token. It contains no secrets of its own.

### Single sign-on

Set the `[oidc]` table to sign in through an identity provider such as Keycloak
or Authentik. Discovery reads the endpoints from `issuer`, so only the issuer,
`client_id` and (for confidential clients) `client_secret` are needed:

```toml
base_url = "https://music.example.com"

[oidc]
issuer        = "https://auth.example.com/realms/main"
client_id     = "tinysonic"
client_secret = "…"   # optional for public clients
```

Register `<base_url>/api/oidc/callback` as the redirect URI with the provider.
The flow is Authorization Code with PKCE and a nonce, checked on the ID token.

Accounts are never created by the provider on its own:

- **Sign-in** works only for an identity already linked to an account.
- **Linking** (`mode=link`) attaches an identity to the signed-in account.
  An identity held by another account is refused.
- **Invitations** (`mode=invite`) create an account bound to the invited
  username. The invitation is spent only after the identity is verified. Such
  an account has no password and uses the identity, or a passkey, to sign in.

A successful sign-in does not hand tokens to the browser or app. The callback
gives a one-time code, valid for a minute, which the client exchanges: the web
page receives a session, and the app receives its own device API key, as with
QR pairing. The code is in the URL fragment for the web and in the
`tinysonic://oidc` redirect for the app. The provider's tokens are never stored
or put in a URL.

## Serving under a subpath

Set `base_path` to serve tinysonic under a prefix of an existing site, for
example `https://example.com/folksonic`:

```toml
base_url  = "https://example.com/folksonic"
base_path = "/folksonic"
```

tinysonic then answers at `/folksonic/rest/…`, `/folksonic/api/…` and
`/folksonic/login`. The reverse proxy must pass the prefix through unchanged
(see `deploy/nginx-subpath.conf` and `deploy/Caddyfile`); it does not need to
strip it.
Subsonic clients take the full address as the server URL. Passkeys are not
affected, since the RP ID is the hostname only.

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
  directory under `/var/lib/tinysonic`, and sandboxing. Verify with
  `systemd-analyze verify deploy/tinysonic.service`.
- `folksonic.toml`: config for serving under `/folksonic` of an existing site
  (`base_path`, `host = "127.0.0.1"`, `port = 4534`, state under
  `/var/lib/tinysonic`).
- `nginx-subpath.conf`: nginx locations to `include` in an existing site's
  `server` block, for `/folksonic` next to other services.
- `nginx.conf`: TLS reverse proxy for a domain of its own, with streaming
  enabled and the same 1 MiB body limit as tinysonic.
- `Caddyfile`: the same proxy with automatic certificates, plus the subpath
  block as a commented snippet.

The deploy examples use port 4534 so tinysonic can share a host with
Navidrome, whose default is 4533 (also tinysonic's built-in default). Keep
`host` at `127.0.0.1` so only the proxy can reach tinysonic, and set
`base_url` to the proxy's public address.

### Deploying with make

The `Makefile` builds a static Linux binary and installs it over ssh. The ssh
user needs sudo rights on the server; the server needs `curl` and systemd,
but no Go.

```bash
make setup  HOST=me@cloud.ademant.de   # once
make deploy HOST=me@cloud.ademant.de   # every release
```

- `setup` creates the `tinysonic` user and `/etc/tinysonic`, installs and
  enables the unit, and installs `deploy/folksonic.toml` as
  `/etc/tinysonic/tinysonic.toml` unless a config already exists. Edit
  `music_dirs` and `base_url` there, then add `deploy/nginx-subpath.conf` to
  the site's nginx config. Re-running it updates the unit and keeps the config.
- `deploy` runs `make check` (build, vet, gofmt, tests), builds
  `dist/tinysonic`, installs it as `/usr/local/bin/tinysonic` (keeping the old
  one as `tinysonic.prev`), restarts the service and waits for
  `<base_path>/rest/ping` to answer on the configured host and port. If it
  does not, the previous binary is restored.
- `rollback` restores `tinysonic.prev`; `status` and `logs` show the unit's
  state and journal.

Set `GOARCH=arm64` for an ARM server. The reverse proxy is never changed by
`make`. The server-side steps are in `deploy/remote.sh`.

The Makefile builds with `GOGC=20 GOFLAGS='-p=1 -gcflags=all=-c=1'`, which
keeps the compiler under about 750 MB of RAM. With Go's defaults, compiling the
SQLite package needs well over 1 GB and is killed on small servers. Settings
from the environment take precedence; `make build GOFLAGS= GOGC=` builds at
full speed on a bigger machine.

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
