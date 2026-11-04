# tinysonic — Claude Code project instructions

## Workflow

Before implementing any non-trivial change:
1. **Discuss** the approach — explain the plan and tradeoffs, wait for confirmation
2. **Create a GitHub issue** with `gh issue create --repo ademant/folksonic` describing the problem and solution, once the user gives the ok
3. **Implement** the fix
4. **Commit** with `Closes #<issue>` in the message so the issue is closed automatically on push

```bash
gh issue create --repo ademant/folksonic --title "short description" --body "problem, solution, impact"
# then implement, then:
git commit -m "fix: description

Closes #NNN"
```

**Multiple issues in one commit need one `Closes #NNN` line each** — `Closes #1040, #1041` on a single line only closes the first issue on GitHub. Always write:
```
Closes #1040
Closes #1041
```
When in doubt after pushing a multi-issue commit, spot-check the non-first issues with `gh issue view <N> --json state`.

Discussion and issue creation (steps 1–2) do not have to be immediately followed by implementation (steps 3–4). Only create an issue after the user has explicitly said ok to that specific proposal — don't create it as part of a general discussion.

Skip the discussion step only for obvious typos or single-line fixes, which may be implemented directly without an issue. Always create the issue before writing code for anything else.

**Verification on every change**: run `go build ./...`, `go vet ./...`, `gofmt -l <touched files>` (should print nothing) and `go test ./...` before committing. Fix `gofmt` findings with `gofmt -w`.

**gofmt and doc comments**: `gofmt -w` converts straight quotes to curly quotes inside doc comments (comments immediately preceding a top-level declaration). This is canonical Go 1.19+ formatting, not a mistake to undo.

**Commits and pushes**: commit only when asked. Don't push unless the user asks. If on `main`, branch first.

## Go version

This project declares **Go 1.27.3** in `go.mod`. Check it rather than trusting this line:

```bash
go version  # must be at least the version in go.mod
```

Do not downgrade `go.mod`. If a newer language or stdlib feature fits the problem, prefer it over a manual workaround.

## Build & run

```bash
go build -o tinysonic ./cmd/tinysonic      # dynamic build
CGO_ENABLED=0 go build -o tinysonic ./cmd/tinysonic   # static build (no cgo needed)
./tinysonic --config smolsonic.toml        # --no-scan / --no-watch to disable startup scan / fsnotify watcher
```

Copy `smolsonic.example.toml` to `smolsonic.toml` for local testing. Don't commit a real config.

## Project layout

| Path | Purpose |
|---|---|
| `cmd/tinysonic/main.go` | Entry point: config, DB init, scan, watcher, HTTP server |
| `internal/cli/` | Flag parsing, startup banner |
| `internal/config/` | TOML config loader |
| `internal/db/` | Schema, migrations, FTS5 setup, SQLite pragmas (driver: `ncruces/go-sqlite3`) |
| `internal/models/` | Artist / Album / Song / Playlist row types |
| `internal/audioinfo/` | Per-format duration and bitrate probing |
| `internal/scanner/` | `WalkDir` scan, tag extraction, cover art, fsnotify watcher, ID generation |
| `internal/server/server.go` | Routing, CORS, request logging, song cache |
| `internal/server/auth.go` | Caller resolution: API key, session, Subsonic credentials |
| `internal/server/api_users.go` | JSON endpoints for accounts, API keys and pairing |
| `internal/server/webauthn.go` | Passkey (WebAuthn) registration and sign-in |
| `internal/server/library.go` | Librarian library page (`/library`, `static/library.html`), `/api/library/*`, BPM job runner |
| `internal/server/paging.go`, `minimal.go` | `tinysonicPaging` / `tinysonicLibraryInfo` and `tinysonicMinimal` vendor extensions |
| `internal/bpm/` | Tempo estimation (pure Go) and ffmpeg decoding for BPM calculation |
| `internal/server/login.go` | Password sign-in and the `/login` page (`static/login.html`) |
| `internal/users/` | Accounts, API keys, invitation codes, pairing tokens, sessions, passkey storage |
| `cmd/tinysonic/admin.go` | `tinysonic user …`, `tinysonic invite …` and `tinysonic pair` commands |
| `internal/server/handlers.go` | Subsonic endpoint handlers |
| `internal/server/repo.go` | `database/sql` queries |
| `internal/server/response.go` | Subsonic JSON envelope helpers |

## Key facts

- **Pure Go, no external services.** Accounts, API keys, pairing tokens and sessions live in the SQLite database (`internal/users`). The optional `username`/`password` in the config only bootstraps the first admin. The one exception is **ffmpeg, an optional runtime dependency used only for BPM calculation** (`internal/bpm`, `ffmpeg_path`); everything must keep working without it, and nothing else may shell out.
- **DB**: SQLite at `database_path` (default `smolsonic.db`), WAL mode, FTS5 for search.
- **IDs**: `ar-…` / `al-…` / `so-…`, derived from tag content. Keep them stable — clients store them as bookmarks. Changing the derivation breaks existing clients.
- **Schema changes**: `schema` in `internal/db/db.go` uses `CREATE TABLE IF NOT EXISTS`. For a change to an existing table, add an explicit, idempotent migration step in `migrate()` rather than editing the `CREATE` statement alone, since existing databases won't pick it up.
- **Streams**: `State.songCache` (`internal/server/server.go`) maps song IDs to file paths. Any code path that changes or deletes a song's path must also call `evictSong`. Verify the scanner/watcher paths do this before relying on it.
- **Subsonic compatibility**: every endpoint is registered at both `/rest/x` and `/rest/x.view`, for GET and POST. Keep responses in the Subsonic envelope. Clients are sensitive to field names and shapes.
- **Auth**: every `/rest/*` handler calls `requireAuth`, and every `/api/*` handler that needs a user calls `requireUser`. `authenticate` (`internal/server/auth.go`) is the only place that resolves a caller. A new endpoint without one of these is a bug unless it's deliberately public.
- **Watcher**: file events are debounced (`watchDebounce`, 750 ms) so partially copied files aren't read. Don't remove the debounce.
- **CORS** is intentionally permissive (any origin) for web clients. Don't tighten it without asking.

## Test coverage

There are currently no test files. When adding logic with non-trivial behaviour, add a `_test.go` next to it. Priority targets, in order: `authCheck` (`internal/server/auth.go`), `fts5Match` (`internal/server/repo.go`, FTS query escaping), ID generation (`internal/scanner/ids.go`), and the scanner against a fixture directory.

## Documentation

`README.md` documents the endpoints, configuration and layout. Update it in the same commit when endpoints, config keys or the dependency set change. Keep its driver and build notes in line with `go.mod`.
