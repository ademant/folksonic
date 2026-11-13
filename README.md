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
drop on any Linux/macOS/Windows host. The one library added for the public
Impressum page is [goldmark](https://github.com/yuin/goldmark), also pure Go.

## Features

- One binary, one TOML file, one SQLite database. No external services.
  [ffmpeg](https://ffmpeg.org/) is an optional runtime dependency for BPM
  calculation and loudness analysis, and
  [folksentia](https://github.com/ademant/folksentia) is an optional analysis
  tool (beats, key, loudness, silence, waveform, fingerprint). See
  [Track analysis](#track-analysis).
- Built on Go's `net/http` with [ncruces/go-sqlite3](https://pkg.go.dev/github.com/ncruces/go-sqlite3)
  (SQLite compiled to WebAssembly, no cgo, FTS5 enabled).
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
- Stable IDs (`ar-…` / `al-…` / `so-…`) derived from tag content with SHA-256
  (32 hex characters), so re-scans are idempotent and clients keep their
  bookmarks. Changing the scheme clears the library once; the next scan rebuilds
  it, and starred items and playlist entries are removed. The current scheme is
  recorded in the `settings` table under `id_scheme`.
- HTTP `Range` support for proper seeking (via `http.ServeContent`).
- Subsonic **username and password** (`u` with `p=…` or `p=enc:<hex>`) are
  supported. Subsonic token auth (`t` with `s`) is not: such requests get error
  42, and clients should use an API key or the password instead.
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
| `ffmpeg_path`        | ffmpeg binary for BPM calculation and the `ffmpeg` analysis tool. Default: `ffmpeg` on `PATH`. Without ffmpeg, BPM calculation is unavailable and everything else works. |
| `[analysis]` table   | Assigns analysis features (`bpm`, `beats`, `key`, `loudness`, `silence`, `waveform`, `fingerprint`) to tools, with `[analysis.tools.<name>]` entries (`command`, `timeout`). Without the table, only BPM is calculated, by hand from the library page. See [Track analysis](#track-analysis). |
| `ignored_articles`   | Leading words skipped when indexing and sorting artists in `getArtists`/`getIndexes`, separated by spaces, and announced as `ignoredArticles`. An article ending in `'` (`L'`) matches without a following space. Default `"The El La Las Le Les Los Il L' An A Die Das Ein"`; `""` disables it. |
| `log_level`          | Minimum level written to the log: `debug`, `info` (default), `warn` or `error`. `--log-level` overrides it. See [Logging](#logging). |
| `max_streams`        | Audio streams (`stream`, `download`) served at once, across all accounts. Default 64; `0` disables the cap. A stream over the cap gets HTTP 503 with `Retry-After`. |
| `max_streams_per_user` | Streams one account may hold at once. Default 8; `0` disables the cap. |
| `legacy_password_auth` | Allow Subsonic clients to sign in with `u`/`p` on every request. Default `true`. Set `false` to require API keys (or a session) for all Subsonic calls. |
| `trusted_proxies`    | Addresses (IPs or CIDR ranges) of reverse proxies whose `X-Forwarded-For` is believed. Empty by default: the connecting address is the client. See [Sign-in limits](#sign-in-limits). |
| `api_key_ttl_days`   | Lifetime of an API key, in days (1–3650). Default 365. Every key expires; a key in use within its last 30 days is renewed to a full lifetime. A request can ask for its own `expires_days`. |
| `api_key_idle_days`  | A key unused for this many days is revoked (1–3650). Default 180. Revoked keys are logged as `api key revoked: idle`. |
| `require_https`      | Refuse plain HTTP on every endpoint. Default on, for a LAN install too. Set `false` only on a trusted test network. See [Running over HTTPS](#running-over-https). |
| `tls_cert`, `tls_key` | PEM certificate and private key: serve HTTPS directly, without a proxy. Both or neither. `tinysonic cert` makes them. When set, the server accepts only HTTPS. |
| `[site]` table       | Public description: `name`, `description`, `contact` (email or URL), `repository` (default `https://github.com/ademant/folksonic`) and `impressum_file` (a Markdown file). Empty fields are not shown. See [Public pages](#public-pages). |
| `[oidc]` table       | Optional single sign-on with an OpenID Connect provider: `issuer`, `client_id` and optional `client_secret`. Needs `base_url`. See [Single sign-on](#single-sign-on). Off unless `issuer` is set. |

## Roles

There are two roles:

- **`librarian`** may start a library rescan (`startScan`) and use the library
  page (`/library`, linked from the account page): statistics, rescans and BPM
  calculation. Nothing else in the API is role-gated.
- **`user`** has no extra rights. Stars, playlists and API keys belong to each
  account.

Account administration is CLI-only, because it needs shell access to the
server: creating, approving, disabling, renaming and removing accounts,
changing roles, setting passwords, revoking a user's API keys, issuing invites
and pairing. Pending registrations are approved
with `tinysonic user approve` and rejected with `tinysonic user remove`. The
API has no endpoint for either. Subsonic `getUser` reports `adminRole` as
false for everyone.

## CLI

```
tinysonic [--config FILE] [--no-scan] [--no-watch] [--log-level LEVEL]
                                                      run the server

tinysonic user add [-c FILE] [--librarian] <username>  create an active account (prompts for password)
tinysonic user list [-c FILE]                         list accounts: role, status and sign-in methods
                                                      (password yes/no, passkeys, OIDC identities,
                                                      active API keys)
tinysonic user approve [-c FILE] <username>           activate a pending account
tinysonic user disable|enable [-c FILE] <username>
tinysonic user set-password [-c FILE] <username>
tinysonic user set-role [-c FILE] <username> user|librarian
                                                      change the role; applies to the next request
tinysonic user rename [-c FILE] <old> <new>           rename an account; playlists, stars, keys and
                                                      passkeys stay. Apps that sign in with the old
                                                      username must be updated
tinysonic user keys [-c FILE] <username>              list the account's API keys, revoked ones included
tinysonic user revoke-key [-c FILE] <username> <id>   revoke one of them (e.g. after a lost phone)
tinysonic user remove [-c FILE] <username>

tinysonic invite create [-c FILE] [--expires 7d] [--librarian] [--note TEXT] [--username NAME]
                                                      print a single-use invitation code; --username
                                                      binds it to one name, for a passkey-only account
tinysonic invite list [-c FILE]                       list invites and their state (never the codes)
tinysonic invite revoke [-c FILE] <id>                withdraw an unused invite

tinysonic pair [-c FILE] --user <username>            print a pairing QR code (valid 15 min, single use)
tinysonic device list [-c FILE] --user <username>     list the account's paired devices
tinysonic device remove [-c FILE] --user <username> <id>   remove one; its next request fails

tinysonic stream rotate [-c FILE]                     new key for signed stream URLs; links made with the old key stop at the next restart

tinysonic cert [-c FILE] [--dir DIR] [--force]        make a self-signed certificate for the host in base_url (10 years, mode 0600 key)
tinysonic cert fingerprint [-c FILE]                  print the pin (sha256/…) of tls_cert, to confirm in the app

tinysonic analysis pending [-c FILE]                  analyse what has no result yet (as after a scan)
tinysonic analysis recalc [-c FILE] [-features key,beats] (-all | <song id>…)
                                                      run the assigned tools again, even for unchanged files
tinysonic analysis export [-c FILE] [-features …] (-all | -pending | <song id>…) -out jobs.json
                                                      write a folksentia job list for an offline run
tinysonic analysis import [-c FILE] [-dry-run] [-size-only] results.jsonl
                                                      store folksentia's results from an offline run
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

tinysonic implements 42 of the OpenSubsonic endpoints. The full list, and the
endpoints that are not implemented, is in
[`docs/opensubsonic-endpoints.md`](docs/opensubsonic-endpoints.md). Both
`.view`-suffixed and plain paths are accepted, on both `GET` and `POST`. Responses are JSON with the Subsonic
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
`apiKeyAuthentication`, `formPost`, the vendor extensions `tinysonicPaging`,
`tinysonicLibraryInfo`, `tinysonicLibraryChanges`, `tinysonicMinimal`, `tinysonicMeter` and `tinysonicStreamUrl` (below), plus `tinysonicSongTags`
when `song_tags` is set. It needs no sign-in.

**Song tags (vendor extension `tinysonicSongTags`)** — with `song_tags` set,
each song carries `tinysonicTags`, an object mapping each configured name to all
of its values in file order, for example `{"DANCE": ["Waltz", "Mazurka"]}`. Names
are case-insensitive in the file and upper-cased in the response. Only the listed
names are read, and they are read-only: the server never writes tags.

**Paging and totals (vendor extension `tinysonicPaging`)** — every list
endpoint accepts `size` (1–500) and `offset`: `getArtists`, `getIndexes`,
`getGenres`, `getStarred(2)`, `getPlaylists`, and the nested lists of
`getArtist` (albums), `getAlbum` (songs), `getPlaylist` (entries) and
`getMusicDirectory` (children). On `getSongsByGenre`, `size` is an alias for
`count`; on `search2/3` and `getStarred(2)`, `size`/`offset` apply to each list
that has no parameter of its own (`songCount`, `songOffset`, … win). Without
`size` and `offset` responses are unchanged. Totals are sent as headers on every
call: `X-Total-Count`, or `X-Total-Count-Artist`/`-Album`/`-Song` for the
search and starred endpoints. `HEAD` returns only these headers. Every list has
a stable order (id as last tie-breaker), so paging neither skips nor repeats
items; `getArtists`/`getIndexes` page over the article-free sorted list and
group each page by letter. Random lists ignore `offset` but report the number
of candidates. The standard `songCount`/`albumCount`/`artistCount` are not
capped, so existing clients that fetch everything at once keep working.

**Library info (vendor extension `tinysonicLibraryInfo`)** —
`GET /rest/tinysonicLibraryInfo` returns `artists`, `albums`, `songs`, total
`duration` in seconds and `lastModified`, the time of the last change a scan or
the watcher made. A client can compare it with its last sync and skip
unchanged libraries with one small request.

**Change feed (vendor extension `tinysonicLibraryChanges`)** —
`GET /rest/getLibraryChanges?since=<ms>` lists what changed since the previous
answer. Send the `serverTime` of the last answer as `since`; leave it out, or
send 0, for the whole library. The answer has `artists`, `albums`, `songs` and
`playlists`, each `{id, updated}`, and `starred`, each `{id, type, updated}`
where `type` is `artist`, `album` or `song`. `deleted` holds `{type, id, deleted}`
for removals, with `type` one of `artist`, `album`, `song`, `playlist` or
`starred`. The feed lists ids only: the client fetches changed items with the
usual endpoints. A tag change to a song also marks its album and artist, and a
playlist changes when its entries are added, removed or reordered. Playlists
and starred items are the caller's own; `deleted` covers them the same way.
Removals are kept for 90 days. If `since` is older than that, or the library
was reset, the answer has `full: true`: the client should discard its copy and
rebuild it from the answer. With `size` (1–500) the answer comes in pages linked
by `nextCursor`; pass it back as `cursor` to get the next page of the same
snapshot. `serverTime` lags the clock by 2 seconds, so a change committed just
after a sync is returned by the next one. With `include=items` (extension
version 2), each artist, album and song entry also carries `item`: the object
as `getArtist` (without its albums), `getAlbum` (without its songs) or
`getSong` returns it, minimal when `Prefer: tinysonic-minimal` is sent. An
entry has no `item` when the object was deleted after the snapshot; the next
sync reports the deletion. With `include=items` the answer is paged at 500
entries even without `size`, so a full sync is a few requests instead of one
per album. Authentication is as for every
endpoint; token auth (`t` with `s`) gets error 42.

**Minimal responses (vendor extension `tinysonicMinimal`)** — with the request
header `Prefer: tinysonic-minimal` (RFC 7240), objects omit optional fields that
are constant, duplicated or derivable: songs drop `parent`, `isVideo`, `type`,
`coverArt`, `path` and `suffix`; albums drop `title` (same as `name`) and
`coverArt`; artists drop `coverArt`. `replayGain`, `bpm` and `tinysonicMeter` stay. Cover art ids follow a fixed rule: an
album's or artist's is its `id`, a song's is its `albumId`. Required fields
(`id`, `isDir`, `title` on songs, `albumCount`, …), `mediaType` and all data
fields (`tinysonicTags`, …) always stay, and folder browsing keeps `parent`.
The response then carries `Preference-Applied: tinysonic-minimal`; all JSON
responses send `Vary: Prefer`. Other `Prefer` values are ignored. It applies to
every endpoint and combines with paging. On a 5,453-song library the full
song list shrinks from 2,954 to 2,163 KiB (688 to 559 KiB with gzip).

**Loudness, tempo and meter** — songs carry the standard OpenSubsonic
`replayGain` object and `bpm` field, and the vendor field `tinysonicMeter`
(extension `tinysonicMeter`), each only when known. They are read from the
files and never written back:

| Field | Tags | Notes |
|---|---|---|
| `replayGain.trackGain` / `albumGain` | Opus: `R128_TRACK_GAIN` / `R128_ALBUM_GAIN`; otherwise `REPLAYGAIN_TRACK_GAIN` / `REPLAYGAIN_ALBUM_GAIN` | R128 is Q7.8 relative to −23 LUFS and is converted to the ReplayGain reference of −18 LUFS: `value / 256 + 5` dB |
| `replayGain.trackPeak` / `albumPeak` | `REPLAYGAIN_TRACK_PEAK` / `REPLAYGAIN_ALBUM_PEAK` | Opus R128 tags carry no peaks |
| `replayGain.baseGain` | Opus header output gain | Sent when non-zero or together with other gains |
| `bpm` | `BPM` (Vorbis/FLAC/Opus, MP3 `TXXX`), MP3 `TBPM`, MP4 `tmpo` | Rounded to an integer. Missing values can be calculated on the library page; a tool assigned to `bpm` replaces the tag's value (see [Track analysis](#track-analysis)) |
| `tinysonicMeter` | `METER`, `TIME_SIGNATURE`, `TIMESIGNATURE` (first found) | The tag text as written, e.g. `3/4`, at most 32 characters; never guessed |

MP3 reads the names from `TXXX` frames, M4A from `----:com.apple.iTunes:` atoms.
Upgrading an existing library re-reads every file's tags once, in the
background scan after the first start.

**BPM calculation** — the library page calculates BPM for songs without a BPM
tag. ffmpeg decodes 90 seconds from the middle of each track (mono, 11 kHz) at
the lowest CPU priority; the tempo is estimated in Go from a spectral-flux
onset envelope by autocorrelation with harmonic summation and a tempo prior
around 120 BPM. It takes about one second per track, one track at a time, and
can be stopped. Values from tags are never overwritten. A calculated value is
kept when a file is re-read with the same audio length (tag edits) and
cleared when the recording changed. On a folk dance library, about 60% of the
estimates match the BPM tag within 4%; most others are off by a 3:4 or 2:1
ratio (cross-rhythms, half or double time), so treat calculated values as a
starting point. Each one stores a confidence (0–1).

### Track analysis

`GET /api/analysis?id=<song id>` (any signed-in account, extension
`tinysonicAnalysis`) returns the stored analysis of a track: beat grid and
downbeats, BPM with confidence, key and mode, integrated loudness and true
peak, lead and tail silence, waveform peaks and a Chromaprint fingerprint.
Every field is `null` when unknown. The schema is in [`docs/openapi.yaml`](docs/openapi.yaml):

```json
{
  "id": "so-…", "fileVersion": "sha256:…",
  "waveform": {"bins": 150, "peaks": [0, 12, 255]},
  "beats": {"timesMs": [0, 500, 1000], "downbeatIndexes": [0]},
  "bpm": {"value": 124.0, "confidence": 0.92},
  "key": {"tonic": "A", "mode": "minor", "confidence": 0.81},
  "loudness": {"integratedLufs": -9.4, "truePeakDbtp": -0.8},
  "silence": {"leadMs": 350, "tailMs": 1200},
  "fingerprint": {"algorithm": "chromaprint", "version": 2, "durationSec": 120, "value": "AQAD…"}
}
```

The endpoint only reads the database. 404 means no such song. 202 with
`Retry-After` means automatic analysis is on and some assigned features have
no result yet; `pending` lists them.

`GET /api/duplicates` (any signed-in account) lists groups of songs that hold
the same recording, `{"groups": [{"songIds": ["so-…", "so-…"], "similarity": 0.94}], …}`.
Fingerprints are decoded in Go and compared by bit error rate at their best
alignment; candidate pairs come from an index of sub-fingerprints, so the
library is not compared pair by pair. Re-encodes and copies on several albums
match, other recordings of the same tune do not. The search runs in the
background at startup and after analysis jobs that changed values (about 2 s
for 5,000 tracks); fingerprints added with the CLI while the server runs are
included after the next job or restart. 202 with `Retry-After` while the first
search runs.

Each feature is assigned to one tool in the config. A feature without a tool
is not computed:

```toml
[analysis]
bpm      = "folksentia"   # or "builtin", the pure-Go estimator (needs ffmpeg)
beats    = "folksentia"
key      = "folksentia"
loudness = "ffmpeg"       # or "folksentia"
silence  = "folksentia"
waveform = "folksentia"
fingerprint = "folksentia" # Chromaprint, for /api/duplicates

[analysis.tools.folksentia]
command = "/usr/local/bin/folksentia"   # absolute path, never looked up on PATH
timeout = "120s"                        # per track, default 2m
memory_limit = "2G"                     # optional, see below
max_duration = "45m"                    # optional, see below
```

Folksentia can also run on another machine, see
[Offline analysis](#offline-analysis). Exports work either way. To never run
it on this host, for example on a server with little memory, leave `command`
unset; the features stay assigned, and their results come in only through
`tinysonic analysis import`:

```toml
[analysis.tools.folksentia]             # no command: offline only
```

An offline-only folksentia is not run after scans, and the library page and
`/api/library/stats` report it as unavailable with the reason `offline only`.
Asking for one of its features explicitly (`POST /api/library/bpm`,
`POST /api/library/analysis` with `features`, `tinysonic analysis recalc
-features`) is refused (503 on the API) instead of starting an empty job. A
recalculation of all features skips them. Imported values are served by
`/api/analysis` like any other.

| Tool | Features | Notes |
|---|---|---|
| `builtin` | `bpm` | The estimator described above. Never replaces a BPM from tags |
| `ffmpeg` | `loudness` | ffmpeg's `ebur128` filter. Uses `ffmpeg_path`; `[analysis.tools.ffmpeg]` may set `command` and `timeout` |
| `folksentia` | `bpm`, `beats`, `key`, `loudness`, `silence`, `waveform`, `fingerprint` | [folksentia](https://github.com/ademant/folksentia) output schema 1. One call per track for all its features. `fingerprint` needs folksentia built with libchromaprint |

`[analysis.tools.<name>]` accepts two limits for tools run as child processes
(`folksentia`, and `ffmpeg` for loudness):

- `memory_limit` caps the memory the tool may allocate, as a size: `500M`,
  `2G` or plain bytes (binary units). On Linux it is `RLIMIT_DATA`, so a track
  the tool cannot handle fails with an error instead of pushing the host into
  the OOM killer. The limit is set just after the tool starts. The built-in
  BPM decoder is not limited.
- `max_duration` skips tracks longer than this for this tool, e.g. `"45m"`. A
  skipped track gets a stored reason (`longer than max_duration 45m0s`) and is
  not retried; raising the limit later does not analyse it until a recalculation.

Each tool child also gets `oom_score_adj` 1000 on Linux, so the kernel
chooses it before folksonic or another service when memory runs out.

- **When it runs.** With an `[analysis]` table, every scan and every file the
  watcher adds or changes queues a background job for what has no result
  yet, one track at a time at the lowest CPU priority. It never runs while a
  request is served. Without the table, behaviour is as before: only BPM,
  started from the library page.
- **Tags first.** Values found in tags (BPM, and the key from `INITIALKEY`,
  `KEY` or `TKEY`) are stored by the scan. A tool's result replaces the
  stored value of each feature assigned to it. Files are never written.
- **Rescans.** Each song records its file's size and modification time. A
  file whose size or mtime changed loses its results, and the tools run
  again. An unchanged file keeps them, also when its tags are re-read.
- **Failures** are stored with the error and not retried by later scans. A
  recalculation retries them.
- **Interrupted runs.** Before a tool starts, the attempt is counted in
  `analysis_attempts`, and the count is removed when the result is stored. A
  run that ends without a result, because the tool was killed or folksonic
  stopped, leaves the count behind. After two such attempts the track is
  recorded as failed (`gave up after 2 interrupted attempts`) and not retried
  by later scans. A stop on purpose (shutdown, cancelled job) does not count.
  A recalculation starts the track afresh.
- **Recalculation** reruns the tools regardless of earlier results:
  `POST /api/library/analysis` or `tinysonic analysis recalc`. For a bulk
  recalculation on a faster machine, use
  [offline analysis](#offline-analysis) instead.
- **Tool version.** Every stored result records the tool version: the tool's
  own report for ffmpeg, `1` for the built-in estimator, `schema-1` for a
  local folksentia run (folksentia has no `--version` yet), and the file's
  `toolVersion` for an import.
- **Progress.** A running job logs one line every 100 tracks or every 10
  minutes, with done, failed and pending counts, the rate and the track just
  done, e.g. `analysis: 1200 done, 3 failed, 4138 pending, 4.1 songs/min,
  current=so-…`. A job ends with a summary line (done, failed, updated, time,
  and `stopped` when cancelled). The library page shows the same numbers while
  a job runs, with the last error, and lists failed tracks with a retry button.
  `GET /api/library/stats` returns `job` (including `pending`, `current`,
  `songsPerMinute`, `lastError`) and `failedSongs`.
- **Logging.** Each tool run writes one `key=value` line with the song id,
  tool, version, duration and values (long lists as their length), e.g.
  `analysis song=so-… tool=folksentia version=schema-1 ms=4210 bpm=124.0 key="A minor" …`.
  Paths and credentials are not logged.

Values are stored in `song_values` (song, name, JSON value, when it changed)
and `analysis` (song, feature, tool, tool version, when it
ran, error), and `analysis_attempts` (interrupted runs, see above). All go
with the song when it is removed, and the attempts are cleared when its audio
changes.

**Response conventions** — optional fields without a value are omitted, never
sent as `null`. A song's `path` is relative to the music directory that holds
it (`Artist/Album/01.opus`); server paths are not exposed. `created` is when a
file was first added to the library (kept across rescans; on upgrade it starts
from the file's modification time), and an album's `created` is that of its
earliest song, so `getAlbumList2?type=newest` lists recently added albums
first, not recent release years. Album `duration` is the sum of its songs in
milliseconds, rounded down once, and the same in every endpoint. The artist
index skips a leading article from `ignored_articles` ("Les Frères Morvan" is
under F); names themselves are unchanged.

**Library (ID3 tag browsing)** — `getArtists`, `getArtist`, `getAlbum`,
`getSong`, `getAlbumList2`, `getAlbumList` (alias)

**Library (folder browsing)** — `getIndexes`, `getMusicDirectory`

**Genres** — `getGenres`, `getSongsByGenre`

**Lists** — `getRandomSongs`, `getStarred2`, `getStarred` (alias)

**Change feed** — `getLibraryChanges` (see *Change feed* above)

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

### Offline analysis

A bulk recalculation can run on a faster machine, for example a desktop with
a copy of the library, without any network connection to the server. It is a
round trip through two files:

```bash
# 1. On the server: the job list (song IDs, relative paths, size and mtime).
tinysonic analysis export -c /etc/tinysonic/tinysonic.toml -all -out jobs.json

# 2. On the desktop: folksentia's batch mode runs every job against the local
#    copy and writes one line per track. It can be stopped and resumed.
folksentia --jobs jobs.json --music-root ~/Musik --out results.jsonl

# 3. On the server: check, then store.
tinysonic analysis import -c /etc/tinysonic/tinysonic.toml -dry-run results.jsonl
tinysonic analysis import -c /etc/tinysonic/tinysonic.toml results.jsonl
```

`export` selects the features assigned to folksentia (`-features` narrows
them), every song with `-all`, only what folksentia has not analysed yet with
`-pending`, or the given song IDs. It works whether or not this host runs
folksentia itself. Paths in `jobs.json` are relative to the music folder and
`/`-separated. The file holds no secrets and no server paths.

**`jobs.json`** (`format` `tinysonic-analysis-jobs`, `version` 1):

```json
{
  "format": "tinysonic-analysis-jobs", "version": 1,
  "created": "2026-11-13T10:00:00Z",
  "tool": "folksentia", "schema": 1,
  "options": { "downbeats": true, "waveform": 150, "fingerprint": true },
  "jobs": [
    { "id": "so-…", "path": "Artist/Album/01 Tune.opus", "size": 1692335,
      "mtimeMs": 1730900000000, "durationMs": 113000,
      "features": ["bpm", "beats", "key", "loudness", "silence", "waveform", "fingerprint"] }
  ]
}
```

`options` are the folksentia flags a local run would pass for these features
(`--downbeats`, `--waveform=150`, `--fingerprint`).

**`results.jsonl`**, one JSON object per line:

```json
{"job": { …the job, unchanged… }, "local": {"size": 1692335, "mtimeMs": 1730900000000}, "toolVersion": "0.5.0", "result": { …folksentia schema 1… }}
{"job": { … }, "skipped": "missing"}
{"job": { … }, "local": { … }, "skipped": "changed"}
```

`import` stores each result exactly like a local run (same parsing, BPM
outside 20–300 refused, a folksentia `error` recorded as a failed run), and
records `toolVersion` as the tool version. It stores a line only when the song
still exists and its file on the server has the job's size and mtime
(`-size-only`: the size alone, for copies that did not keep modification
times), and only for features still assigned to folksentia. When a song
appears twice, the last line wins. Every other line is counted and skipped; a
bad line never stops the import. At the end it prints, for example:

```
5453 lines: stored 5440 (5212 changed, 3 failed runs); skipped 2 missing, 11 stale
```

Skip reasons: `missing` and `changed` (on the desktop, as folksentia
reported), `stale` (the file changed on the server since the export),
`unknown` (no such song), `unassigned`, `duplicate` and `invalid` (with the
line number on stderr). A running server sees the new values on its next read.

## Tested clients

Anything that speaks Subsonic API 1.16.x and prefers tag-based browsing should
work, including Substreamer, play:sub, Symfonium, Tempo, and Sonixd.

## How auth works

Every `/rest/*` request is authenticated in this order. A paired app (a
**device**, #101) is the exception in method 0: it sends no secret, only a proof
signed with a key that never leaves its Keystore.

0. **Device proof** (`DPoP: <proof>`, RFC 9449): the app signs each request with
   its device key. The proof must name the request's method and URL (without
   its query), carry a fresh `iat` (within 60 seconds) and a `jti` not seen
   before. Its key must belong to an active device. Devices may use `/rest/*`
   and `/api/*`, but not manage the account: confirming pairings, listing and
   removing devices, and managing keys, passkeys and identities need a session.
   A device's proof is refused with `pairing_pending` until the pairing is
   confirmed on both sides, `pairing_rejected` once rejected, `device_unknown`
   once removed, and `proof_replayed` on reuse.

1. **API key**: sent in the `X-Api-Key: <key>` header (preferred, for API clients
   such as DanceSonic), or as the `apiKey=<key>` parameter (Subsonic clients and
   the Android app). The header keeps the key out of URLs and access logs. Keys
   are created with `POST /api/keys`, or by a legacy pairing for apps without device keys. A key has the same
   rights as its user and can be revoked. A key may call the Subsonic API, the
   analysis reads, but not manage the account: creating,
   listing or revoking keys, pairing devices, passkeys, identities and library
   jobs need a signed-in session (`session_required`). Every key expires after
   `api_key_ttl_days` (365 by default), is renewed when it is used in its last 30
   days, and is revoked after `api_key_idle_days` (180) without use.
2. **Session token** (`Authorization: Bearer <token>`): issued by a passkey or
   password sign-in on the web page at `/login`.
3. **Signed stream URL** (`tinysonicStreamUrl`): `GET /api/stream-url?id=so-…` with a
   session returns a link to one song that works for 2 hours, carrying `uid`,
   `exp` and `sig` (an HMAC over the song, user and expiry). It is accepted only on
   `/rest/stream` and `/rest/download`, for that song only. A changed or expired
   link gets HTTP 403 with `stream_url_invalid` or `stream_url_expired`. API keys
   cannot mint links. Subsonic clients keep method 1 and do not use this.
4. **Subsonic username and password**: `u` with `p`, plain or `p=enc:<hex>`.
   Clients that offer a password option use this. Token auth (`t` with `s`) is
   refused with Subsonic error 42. Set `legacy_password_auth = false` to turn
   this method off: such requests then get error 42, and API keys are the only
   way in. Password sign-in on `/login` and `/api/login` is not affected.

Credentials may be sent in the query string or as a form-encoded POST body.
Only active accounts authenticate; pending and disabled accounts are refused.

Passwords are stored as argon2id hashes, with a per-password salt, so a copy of
the database does not reveal them. Each password check costs about 70 ms on a
small host, which is why passkeys or API keys are better for long-lived clients.
Passkeys remain the recommended sign-in for the web UI.

Existing databases are migrated at the next start: stored plaintext passwords
are hashed and the plaintext is removed. The step is idempotent.

Passkey-only accounts have no password. They cannot use the password parameter
and an empty password never matches.

### Hardening the Subsonic API

Subsonic clients send their credentials with every request, including each
stream and cover-art URL. A username and password, or an API key in the URL, is
therefore the weakest part of the server. The protocol offers nothing better:
a Subsonic client cannot ask for a short-lived, per-track link. Our own apps can
(see item 3 under [How auth works](#how-auth-works)), but Subsonic clients stay
on the long-lived key. For a server reachable from the internet:

1. **Serve HTTPS only.** `require_https` is on by default, so plain HTTP gets 426
   on `/rest` and `/api`. A LAN install gets HTTPS too, with `tls_cert` and
   `tls_key` (see [Running over HTTPS](#running-over-https)), so no key or
   password crosses the network in the clear.
2. **Turn off password sign-in for Subsonic clients:** `legacy_password_auth = false`.
   A request with `u` and `p` then gets Subsonic error 42, and only API keys (or a
   session) work. A password would otherwise be sent on every call.
3. **Send the key in the `X-Api-Key` header when the client allows it.** A key in
   the query string (`apiKey=`) is written to every proxy access log that records
   the full URL, to the browser history for a link opened in a browser, and to any
   link that is shared. The header keeps it out of all three. Clients that cannot
   set headers have to use the parameter.
4. **Keep query strings out of proxy logs.** The Apache template logs only the
   path (`%U`). nginx's default access log records `$request`, which includes the
   query string; use a `log_format` without it, or turn off the access log for
   the Subsonic paths. The Caddy template sets no access log, so add one with the
   query string removed if you enable it.
5. **Use short key lifetimes.** `api_key_ttl_days` (default 365) and
   `api_key_idle_days` (default 180) bound a leaked key. Shorter values suit a
   public server, at the cost of pairing devices again more often. A key can be
   revoked on the account page, and every key shows when it was last used.
6. **Expect the limits to be enforced in code, not left to the client.** Token
   auth (`t` with `s`) is refused with error 42. Repeated wrong passwords are
   slowed and then refused (see [Sign-in limits](#sign-in-limits)). Each refusal
   is logged with a reason code, and the key itself never appears in the log.

A recommended public configuration:

```toml
base_url             = "https://music.example.com"
legacy_password_auth = false
api_key_ttl_days     = 90
api_key_idle_days    = 30
```

**What a leaked Subsonic key can still do.** It can call every Subsonic endpoint
the account may use, until it expires or is revoked: play and read the library,
star, make playlists and scrobble. It cannot create, list or revoke keys, pair
devices, manage passkeys or identities, run library jobs, or mint stream links
(`session_required`). Revoking
the key from the account page ends its access at the next request.

**What is left to do.** A Subsonic client's key is valid for its whole lifetime,
and it reaches the server on every stream and cover-art request. Only our own
apps avoid that, through signed stream links that last 2 hours and cover one
song. Subsonic clients need a shorter key lifetime or a user who revokes the
key after a device is lost.

### Accounts and pairing

**Pairing a device.** The account page shows a QR code for 15 minutes. The app
scans it and creates a key in its Keystore; it sends only the public key with
the token. Like Bluetooth pairing, the phone and the account page (under
"Waiting for confirmation") then show the same six-digit **verification code**,
and the pairing completes only when it is confirmed on **both** sides: with
"Code matches" on the account page, and on the phone, which sends its answer
signed with the new key (`POST /api/pair/confirm`). The order does not matter.

Each side derives the code on its own; it is never sent. The code is the first
four bytes of `SHA-256(token_hash ":" jkt)`, read big-endian, modulo 1,000,000
and zero-padded to six digits. `token_hash` is the lower-case hex SHA-256 of the
token from the QR code and `jkt` the RFC 7638 thumbprint of the device key. Test
vector: token `tsp_example-pairing-token` and jkt
`NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs` give `446284`.

Someone who photographs the QR code and scans it too gets a different code,
because their key differs. A QR code can collect up to three waiting pairings.
The first one confirmed on both sides **uses it up**: every other pairing on it
is rejected (`pairing_rejected`), and further scans get `pairing_token_used`.
Rejecting a pairing on the account page also retires its QR code, so a rejected
device cannot simply scan again; pairings already waiting on it stay open.
Cancelling on the phone leaves the QR code usable. Nothing is granted before
both confirmations, and the wait ends with the QR code's 15 minutes. A paired
device appears under "Devices", where it can be removed at any time. A device
holds no bearer secret, so a copied request or log line is not a key.
`tinysonic device list` and `tinysonic device remove` do the same from the
command line.

The pairing without a device key (an API key for older apps) is refused for a
QR code that a device has already scanned, so it cannot be used to skip the
confirmation. It still works for a fresh QR code.

The JSON endpoints below are described in [`docs/openapi.yaml`](docs/openapi.yaml).

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| POST | `/api/register` | none | Create an account. With `invite` and `password`: active and signed in (returns a session `token`). Without an invite: pending, needs `allow_registration`, and needs a password |
| POST | `/api/keys` | session | Create an API key (shown once). `expires_days` sets its lifetime; without it the key lives `api_key_ttl_days` |
| GET | `/api/keys` | session | List own keys |
| GET | `/api/stream-url?id=so-…` | session | Signed stream URL for one song, valid 2 hours. Play it at `/rest/stream.view` or `/rest/download.view` with no other credentials |
| DELETE | `/api/keys/{id}` | session | Revoke an own key |
| POST | `/api/pair/start` | session | Create a pairing token and QR payload |
| POST | `/api/pair/complete` | token | Redeem a pairing token. With `jwk` (the app's public P-256 key) the pairing waits for both confirmations and no secret is returned (202, `"confirm": true`). Without `jwk` the legacy form returns an API key, for apps that predate device keys |
| POST | `/api/pair/confirm` | proof of the pending key | The device's answer to the verification code: `{"result": "match"}` or `"cancel"`. 200 with the device once both sides confirmed, 202 while the owner has not |
| GET | `/api/device/me` | device proof | The device this proof belongs to. The app polls it while the pairing waits |
| GET | `/api/pair/pending` | session | Pairings waiting for confirmation, each with its `code`, `clientAddress` and both confirmation flags |
| POST | `/api/pair/pending/{id}/confirm` | session | The owner's answer: `{"result": "match"}` (201 with the device, or 202 while the phone has not confirmed) or `"reject"` (also retires the QR code). 410 once the 15 minutes have run out |
| GET | `/api/devices` | session | The account's paired devices |
| DELETE | `/api/devices/{id}` | session | Remove a device; its next request fails with `device_unknown` |
| POST | `/api/passkey/register/begin` · `/finish` | session | Enrol a passkey |
| POST | `/api/passkey/login/begin` · `/finish` | none | Sign in with a passkey |
| POST | `/api/register/passkey/begin` · `/finish` | none | Sign up with an invite and a passkey, no password. The account is created only after the passkey is verified |
| POST | `/api/login` | none | Sign in with username and password |
| POST | `/api/logout` | Bearer | End a session |
| GET | `/account` | page | Account page: API keys, pairing QR, passkeys and linked identities. Redirects to sign-in when there is no session |
| POST | `/api/pair/qr` | session | Create a pairing token and return its QR code, SVG by default; PNG with `Accept: image/png` or `?format=png` |
| GET | `/api/passkeys` | session | List own passkeys (`id`, `label`, `createdAt`) |
| DELETE | `/api/passkeys/{id}` | session | Remove an own passkey. Refused (409) when it is the account's last sign-in method |
| GET | `/api/oidc/status` | none | `{"enabled": bool}`, so the page knows whether to offer single sign-on |
| POST | `/api/oidc/begin` | none (`link` needs user) | Start single sign-on. Body: `mode` (`signin`, `link` or `invite`), `client` (`web` or `app`), and `invite` and `username` for `invite`. Returns the provider's `url` |
| GET | `/api/oidc/callback` | state | Provider redirect target. Ends in the sign-in page with a one-time code, or with an error |
| POST | `/api/oidc/exchange` | one-time code | Trade the code for a session `token` (web) or a device `apiKey` (app) |
| GET | `/library` | page | Library page for librarians: statistics, rescan, BPM calculation |
| GET | `/api/library/stats` | session | Totals, BPM coverage (tag / calculated / missing), last scan, running scan or job, ffmpeg availability, analysis assignments and tool availability |
| POST | `/api/library/scan` | session | Rescan the music folders (409 while a scan or job runs) |
| POST | `/api/library/bpm` | session | Start BPM calculation with the tool assigned to `bpm`, body `{"mode": "missing"}` or `{"mode": "all"}` (all also recalculates; 503 when the tool is not available) |
| POST | `/api/library/analysis` | session | Recalculate analysis, body `{"ids": ["so-…"]}` or `{"all": true}`, optionally `"features": ["key"]` (409 while a scan or job runs; 503 when a tool is not available) |
| GET | `/api/library/analysis/failed` | session | Tracks whose last analysis failed, with tool and error; `?limit=` up to 1000 (default 200). Retry with the POST above |
| POST | `/api/library/jobs/cancel` | session | Stop the running job after the current track |
| GET | `/api/analysis?id=` | user | Stored analysis of one track; see [Track analysis](#track-analysis) |
| GET | `/api/duplicates` | user | Groups of songs with matching audio fingerprints; see [Track analysis](#track-analysis) |
| GET | `/api/oidc/identities` | session | List linked identities |
| DELETE | `/api/oidc/identities/{id}` | session | Unlink an identity. Refused (409) when it is the account's last sign-in method |

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

## Sign-in limits

Failed password attempts are limited per client address. The first five
attempts from an address are answered at once. After that, one more is allowed
each second, and the rest get HTTP 429 with `Retry-After` before any password
is checked. Each failure also waits 10 ms, doubling for each consecutive
failure up to 100 ms, so guessing gets no faster. The wait is the same whether
the username exists or not. Successful sign-ins cost nothing, so a client that
sends its password with every request is not throttled. Keys and sessions are
not limited.

Every failed attempt writes one line to the log:

```
WARN auth failed ip=198.51.100.7 reason=password path=/rest/ping
```

`reason` is `password`, `unknown-user`, `not-active` or `rate-limit`. The line
never holds the username, the password or the query string.

Behind a reverse proxy, the address is the proxy's unless you say otherwise.
List the proxy in `trusted_proxies`, and tinysonic takes the client address
from the rightmost `X-Forwarded-For` entry that is not itself a trusted proxy.
The nginx examples in `deploy/` append that header. Only list proxies you
control: a header from anywhere else is ignored.

`deploy/fail2ban/` has a filter for that line and a jail: 50 failures in a
minute bans the address for an hour. Apply the ban where clients connect. Behind
a reverse proxy that is the proxy's host, not the tinysonic host.

```toml
trusted_proxies = ["127.0.0.1", "::1"]
```

## Security headers and pages

Every response carries `X-Frame-Options: DENY` and `X-Content-Type-Options:
nosniff`, so no other site can frame tinysonic, including the sign-in page.
The web pages have no inline script or style. Their scripts and stylesheets
are served from `/static/` and the policy is `'self'` only. A reverse proxy
should not add `'unsafe-inline'` to that policy.

Every response also carries `Referrer-Policy: no-referrer`, so the URL of a
page (which can hold a key) is not sent to other sites, and a
`Permissions-Policy` that turns off camera, microphone, geolocation, payment
and USB. Passkeys are not affected by that policy.

The Subsonic and API responses (`/rest/*` and `/api/*`) carry
`Cache-Control: no-store`, except `stream` and `download`
(`private, max-age=3600`) and `getCoverArt` (`private, max-age=86400`). Those
are private, so the client may keep them, but a shared cache may not.

## Public pages

Four pages are public, and they describe the server only. The library is never
listed.

- `/` shows an information page to browsers: the name, description, version,
  contact, a link to the source repository, the OpenAPI description and the
  endpoint list (both on GitHub), and links to the Impressum and sign-in. Other
  clients, such as `curl`, still get the plain-text banner.
- `/impressum` renders `impressum_file`, a Markdown file. goldmark converts it,
  and raw HTML in the file is not passed through. Without the setting the page
  is not found.
- `/robots.txt` disallows `/rest/`, `/api/`, `/login`, `/account` and `/library`.
- `/sitemap.xml` lists `/` and `/impressum` with the absolute addresses from
  `base_url`. It is not found when `base_url` is not set.

The sign-in, account and library pages send `X-Robots-Tag: noindex`.

```toml
base_url = "https://music.example.com"

[site]
name          = "Folk Cloud"
description   = "Private library of folk recordings for the dance group."
contact       = "admin@example.com"
impressum_file = "impressum.md"   # relative paths resolve from this file's directory
```

Under `base_path`, all four pages are served under that prefix. Crawlers only
read `robots.txt` from the host root, so a subpath deployment needs its own
`robots.txt` at the proxy.

## Serving under a subpath

Set `base_path` to serve tinysonic under a prefix of an existing site, for
example `https://example.com/folksonic`:

```toml
base_url  = "https://example.com/folksonic"
base_path = "/folksonic"
```

tinysonic then answers at `/folksonic/rest/…`, `/folksonic/api/…` and
`/folksonic/login`. The reverse proxy must pass the prefix through unchanged
(see `deploy/nginx-subpath.conf`, `deploy/apache-subpath.conf` and
`deploy/Caddyfile`); it does not need to
strip it.
Subsonic clients take the full address as the server URL. Passkeys are not
affected, since the RP ID is the hostname only.

## Running over HTTPS

HTTPS can come from a reverse proxy (nginx, Caddy or Apache; templates in
`deploy/`) or from tinysonic itself. Passkeys require HTTPS.

**On a LAN, tinysonic serves HTTPS itself.** Run `tinysonic cert`. It makes a
self-signed certificate for the host in `base_url` with its own key, which is
not shared with other installs, and prints the lines to add to the config:

```toml
base_url = "https://192.168.1.10:4533"
tls_cert = "/etc/tinysonic/tls/cert.pem"
tls_key  = "/etc/tinysonic/tls/key.pem"
```

Browsers and other clients do not trust a self-signed certificate. The pairing
QR code carries the certificate's pin (`pin=sha256/…`), so the app trusts this
server and no other certificate. A device set up by typing an address and
password has no pin: run `tinysonic cert fingerprint` and confirm the value in
the app before trusting the server. The certificate lasts 10 years; renewing it
changes the pin, so devices pair again. Use `--force` only for that.

With `tls_cert` set, the server accepts only HTTPS. A plain HTTP request gets
the TLS listener's own refusal, not a 426.

**Behind a proxy.** Bind tinysonic to `127.0.0.1` so only the proxy can reach
it, set `base_url` to the proxy's public address, and list the proxy in
`trusted_proxies`. Leave `tls_cert` unset.

**Enforcement.** `require_https` refuses plain HTTP on every endpoint. It is on
by default. A request counts as HTTPS if it arrived over TLS, or if a trusted
proxy says `X-Forwarded-Proto: https`. Refused requests get:

- `/api/*`: HTTP 426 with `{"error": "https_required", …}`;
- `/rest/*`: HTTP 426 with the Subsonic envelope, error 0;
- browser pages (GET): HTTP 301 to the same path on the `base_url` address.

A request from the server itself (loopback, with no `X-Forwarded-For` header,
as the deploy health check sends) is allowed. A request a proxy passed on
carries `X-Forwarded-For`, so it is never treated as local.

**Rollout on an existing public server.** Before turning enforcement on, the
proxy must send `X-Forwarded-Proto` (the Apache snippet does, with
`RequestHeader set`) and `trusted_proxies` must list it. Otherwise every
request through the proxy is refused, and the server logs a warning at start.
Every proxy template sends `Strict-Transport-Security`, so browsers keep to
HTTPS once they have used it.

Request logs record the path only; credentials in the query string or form
body are never written to the log.

## Deployment

Example files are in [`deploy/`](deploy/):

- `tinysonic.service`: systemd unit with a dedicated `tinysonic` user, a state
  directory under `/var/lib/tinysonic`, and sandboxing. Verify with
  `systemd-analyze verify deploy/tinysonic.service`. It sets
  `OOMPolicy=continue`, so when the kernel kills an analysis tool for memory,
  the kill is recorded as a failed run and the server keeps running. Without
  it, systemd stops the whole service. `MemoryHigh=` and `MemoryMax=` are
  commented out; they cap the service and its tools together, so set them
  only with room for the other services on the host.
- `tinysonic-watch.sh`, `tinysonic-watch.service`, `tinysonic-watch.timer`:
  a timer that runs every 10 minutes and logs a `tinysonic-watch` line at
  priority crit when `tinysonic.service` restarts or its last result changes
  (for example to `oom-kill`). Set `NOTIFY_CMD` in a drop-in to send the same
  message elsewhere, such as by mail or ntfy. Check with
  `journalctl -t tinysonic-watch`.
- `folksonic.toml`: config for serving under `/folksonic` of an existing site
  (`base_path`, `host = "127.0.0.1"`, `port = 4534`, state under
  `/var/lib/tinysonic`).
- `nginx-subpath.conf`: nginx locations to `include` in an existing site's
  `server` block, for `/folksonic` next to other services, with gzip for
  JSON responses.
- `apache-subpath.conf`: the same for Apache, including gzip for JSON (a
  `<Location>` type list replaces the global one, so it lists the text types
  too).
- `nginx.conf`: TLS reverse proxy for a domain of its own, with streaming
  enabled and the same 1 MiB body limit as tinysonic.
- `Caddyfile`: the same proxy with automatic certificates, plus the subpath
  block as a commented snippet.

The deploy examples use port 4534 so tinysonic can share a host with
Navidrome, whose default is 4533 (also tinysonic's built-in default). Keep
`host` at `127.0.0.1` so only the proxy can reach tinysonic, and set
`base_url` to the proxy's public address.

### Logging

The server logs to stderr at four levels, and `log_level` (or `--log-level`)
sets the lowest one written:

| Level   | What |
|---------|------|
| `debug` | Scan progress (`scan: walking <dir>`), song cache preload |
| `info`  | One line per request, startup (`listening on`), `scan complete`, migrations, files the watcher adds, updates or removes |
| `warn`  | Unreadable files or directories, a missing music directory, disabled passkeys, a config with a secret readable by others, BPM and analysis failures for single songs, 5xx answers |
| `error` | Database errors, files that cannot be streamed, failed scans or watcher, startup failures |

Under systemd (detected through `JOURNAL_STREAM`) each line starts with its
syslog priority and has no timestamp of its own, so journald stores the level
and the time:

```bash
journalctl -u tinysonic -o short-iso        # with times
journalctl -u tinysonic -p warning          # warnings and errors only
```

Elsewhere each line starts with the local time, and every level but `info` is
named (`2026/11/04 21:29:14 WARN …`). The startup banner is printed only when
stdout is a terminal. Query strings are never logged, because they can carry
credentials. A request that has an `id` parameter also shows it (`id=so-…`), so
stream lines name their track; only a plain token is shown.

A refused credential is logged as `auth failed ip=… reason=… path=…`, which the
Fail2ban filter in `deploy/fail2ban` reads. The reason is one of the refusal
codes below. A revoked key is logged at warn, the others at info; a password
failure keeps its existing reasons (`password`, `unknown-user`, `not-active`,
`rate-limit`) at warn.

For a revoked, idle or expired key the line also names the key's id and label
(`key=7 label="old phone"`), so an administrator can find it with
`tinysonic user keys`. The key itself is never logged.

### Refusal codes

A refused request says why, with a code clients can branch on. On the JSON
API the body is `{"error": "<code>", "message": "<text>"}`. On Subsonic
endpoints the answer keeps the envelope with error 40 and the message. The
message text is for people; apps should not parse it.

| Code | Meaning |
|---|---|
| `credentials_required` | No credentials were sent |
| `key_unknown` | The API key does not match any key |
| `key_revoked` | The key was revoked |
| `key_idle` | The key was revoked for going unused (`api_key_idle_days`) |
| `key_expired` | The key is past its lifetime |
| `session_unknown`, `session_expired` | The web session token is unknown or expired |
| `user_disabled` | The account is disabled |
| `scope_forbidden` | The key may not be used for this request (a key with a scope, such as the removed analysis worker keys) |
| `session_required` | The request needs a signed-in session, not an API key (account and library management) |
| `stream_url_expired` | The signed stream link is past its expiry; ask `/api/stream-url` for a new one (403) |
| `stream_url_invalid` | The signed stream link was changed, is for another song, or its key was rotated (403) |
| `proof_invalid` | A device proof is malformed, signed by another key, or for another method or URL (401) |
| `proof_replayed` | A device proof's `jti` was used before (401) |
| `pairing_pending` | The pairing waits for its confirmation on the phone, the account page, or both (401) |
| `pairing_rejected` | The pairing was rejected on the account page or on the phone, or another device used the QR code first (401, 409) |
| `pairing_token_used` | The QR code was used up by a confirmed device or retired by a rejection; create a new one (409) |
| `pairing_busy` | Three devices already wait on this QR code (429) |
| `device_unknown` | The device was removed, or its key was never paired (401) |
| `password_invalid`, `rate_limited` | Subsonic `u`/`p` sign-in failed, or too many failures |
| `pairing_expired`, `pairing_invalid` | A pairing code is past its 15 minutes, or unknown or already used |
| `invite_expired`, `invite_invalid` | An invitation code is past its 7 days, or unknown, used or revoked |
| `https_required` | Plain HTTP while `require_https` is on |

Access logs written by Apache leave out query strings too. See the comments in
`deploy/apache-subpath.conf`, which also say which host settings are needed.

A request line ends with the client name (`c`), the signed-in user and, for a
failed Subsonic answer (which is still HTTP 200), its error code, each only
when known:

```
GET /folksonic/rest/getAlbum.view 200 1801B 3.1ms minimal client=DanceSonic user=ademin
GET /folksonic/rest/ping.view 200 162B 210ms client=DSub err=40
```

Client names are cut to 64 characters, and characters other than letters,
digits and `._-+/:@` become `_`.

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
  passwords/                   argon2id password hashing
  db/                          schema + migrations + FTS5 backfill
  models/                      Artist / Album / Song / Playlist row types
  audioinfo/                   per-format duration + bitrate probers
  scanner/                     filepath.WalkDir + tag extraction + cover art
  bpm/                         built-in tempo estimator, ffmpeg decoding
  analysis/                    per-track analysis: tool adapters, job selection, storage
  server/
    server.go                  net/http server, routing, CORS
    ratelimit.go               sign-in failure limit, stream slots, client address
    site.go                    public pages: info, Impressum, robots.txt, sitemap
    auth.go                    Subsonic password auth (hash check)
    response.go                JSON envelope helpers
    repo.go                    database/sql queries
    handlers.go                Subsonic endpoint handlers
```

## License

MIT.
