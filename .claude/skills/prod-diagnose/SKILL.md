---
name: prod-diagnose
description: Diagnose problems on the production tinysonic/folksonic server at cloud.ademant.de from its logs and database. Use whenever the user reports that something on the live server failed or behaves oddly — login, registration or invite didn't work, app pairing/QR failed, an app can't connect, tracks or tags missing, a deploy misbehaved — or asks to "check the logs", "did X work", or for library numbers such as how many tracks there are.
---

# Diagnosing production

Work from evidence: find the requests in the logs, reconstruct the timeline, then confirm the cause by reproducing it on a copy of the database — not on the live data.

## Where to look

| Source | Command | Shows |
|---|---|---|
| Service log | `sudo journalctl -u tinysonic --since "-1h" --no-pager -o short-iso` | every request: method, path, status, bytes, duration; startup; scan summary. Lines carry no time of their own under systemd, so use `-o short-iso` (not `-o cat`) when times matter |
| Problems only | `sudo journalctl -u tinysonic --since "-1d" --no-pager -p warning` | warnings and errors (journal priority is set per line) |
| Apache access log | `sudo grep folksonic /var/log/apache2/access.log \| tail -n 50` | client IP, **user agent** (browser vs app, e.g. `okhttp` = Android), full query string |
| Accounts/invites | `cd / && sudo -u tinysonic /usr/local/bin/tinysonic user list\|invite list -c /etc/tinysonic/tinysonic.toml 2>/dev/null` | states, who used which invite |
| Database (read-only) | see below | anything else |

Scan progress (`scan: walking`) is at debug level and not logged at the default `log_level = "info"`; versions before #59 logged it, filter it with `grep -v "scan: walking"`.

Read-only database queries, as the service user so file permissions don't change:
```bash
sudo -u tinysonic python3 -c "
import sqlite3; c=sqlite3.connect('file:/var/lib/tinysonic/tinysonic.db?mode=ro', uri=True)
print(c.execute('select count(*) from songs').fetchone())"
```
Check column names with `pragma table_info(<table>)` first; the schema changes between versions. Never write to the production database by hand.

## Reading the logs

- **Subsonic endpoints (`/rest/*`) answer HTTP 200 even when authentication fails.** The error is inside the JSON body (`"status":"failed"`, code 40). Since #57 the request line ends with `err=<code>` for such answers, and with `client=<c>` and `user=<name>` when known, so the service log alone shows which app and account made a request. In logs from before #57, compare the body size to a known-good and a known-unauthenticated response, or match times against the Apache log for the user agent.
- **`/api/*` uses real status codes.** 401 = not signed in or token invalid/used; 403 = forbidden or invalid invite; 409 = name taken.
- **~200 ms on a 401/403** is the deliberate delay on failed sign-in or invalid invite/pairing codes, i.e. a rejected credential rather than a server error.
- **Pairing flow**: browser `POST /api/pair/qr` → app `POST /api/pair/complete` (200 = key issued) → app's first `/rest/ping?apiKey=…`. Tokens are single use, so a second `pair/complete` with the same token is a 401 by design (DanceSonic has sent duplicates).
- **Registration flow**: `register/passkey/begin` → `finish` (201 = account created) → `passkey/login/*`.

Build a small table of time / client / request / result for the user. It makes the story obvious and separates server bugs from client behaviour or user error.

## Confirming a cause

Reproduce on a snapshot of the live database with the same binary:
```bash
TMPDIR=<scratchpad> .claude/skills/prod-deploy/scripts/copy-instance.sh /usr/local/bin/tinysonic
```
Then drive the failing flow against `http://127.0.0.1:4598/folksonic` with curl and the CLI (`/usr/local/bin/tinysonic <cmd> -c <dir>/c.toml`), using the real account names from the copy. Clean up with `kill "$(cat <dir>/pid)" && rm -rf <dir>`.

Don't create keys, sessions or pairing tokens on real production accounts to test; do it on the copy.

## After diagnosis

State the cause with file and line, what it affects beyond this case, a workaround the user can apply now, and the fix. Code fixes follow the ship-change workflow (issue first); a fix the user wants live goes out with prod-deploy.
