---
name: prod-deploy
description: Deploy tinysonic/folksonic to production at https://cloud.ademant.de/folksonic, which is this machine. Use whenever the user says "deploy", "deploy to prod", "roll out", "update the server", "install the new version", "roll back", or asks to try a new build against the production database, even if they don't name the host. Covers pre-flight checks, testing migrations on a copy of the live database, backup, install, health check, rollback and verification.
---

# Deploying to cloud.ademant.de

Production runs on **this machine** (hostname `nextcloud-ubuntu-4gb-hel1-13`), next to Nextcloud and Navidrome. sudo works without a password. Deploy locally with the scripts here, not with `make deploy HOST=…`: its health check waits only 15 s and its rollback restores the binary without the database, which is how a slow migration once left the old binary running on a half-migrated schema.

## Production layout

| What | Where |
|---|---|
| Binary / previous binary | `/usr/local/bin/tinysonic`, `/usr/local/bin/tinysonic.prev` |
| Config | `/etc/tinysonic/tinysonic.toml` (root:tinysonic 0640) — `127.0.0.1:4534`, `base_path = "/folksonic"`, music in `/srv/music` |
| State, DB, backups | `/var/lib/tinysonic/` (`tinysonic.db`, `covers/`, `backup-*/`) |
| systemd | `tinysonic.service` + drop-in `tinysonic.service.d/music.conf` (`SupplementaryGroups=music`, needed to read `/srv/music`) |
| Proxy | Apache, `/etc/apache2/sites-enabled/000-default-le-ssl.conf`, `ProxyPass /folksonic http://127.0.0.1:4534/folksonic flushpackets=on`, plus a `<Location /folksonic>` block with `AddOutputFilterByType DEFLATE` for JSON and text types (gzip; it replaces the global type list for that path, so it must list text/html etc. too). The same vhost serves Nextcloud and `/navidrome` (port 4533) |

Never restart Apache for a tinysonic deploy; if the vhost ever needs editing, back it up, run `apache2ctl configtest` and `systemctl reload apache2`.

## Steps

1. **Decide what to deploy.** Usually `main`: `git checkout main && git pull`. List what is new since the running version (`git log --oneline --no-merges <deployed>..HEAD`; the running commit is in the name of the newest `/var/lib/tinysonic/backup-*-pre-<sha>` directory). If the user wants an unmerged branch, say that production will then be ahead of `main` and that the next deploy from `main` would drop the change until it is merged.

2. **Look for schema changes and new config keys**, because they decide how risky the deploy is:
   ```bash
   git diff <deployed>..HEAD -- internal/db/ | grep "^+" | grep -iE "ALTER|CREATE|DROP|TRIGGER|UPDATE |func migrate"
   git diff <deployed>..HEAD -- internal/config/config.go | grep "^+.*toml:"
   ```
   A new required config key must be added to `/etc/tinysonic/tinysonic.toml` before installing. Migrations that rewrite every row (e.g. `UPDATE songs …`) can take tens of seconds on this host because each song update also rewrites the FTS index through a trigger.

3. **Check and build.** The Makefile sets the low-memory Go flags itself (this host OOM-kills the compiler otherwise):
   ```bash
   make check && make build
   ```
   Don't deploy if anything fails.

4. **Try the binary on a copy of the live database** whenever the diff touches `internal/db/` or `internal/users/`. It costs a minute and catches failed or slow migrations before production sees them:
   ```bash
   TMPDIR=<scratchpad> .claude/skills/prod-deploy/scripts/copy-instance.sh dist/tinysonic
   ```
   Note the reported startup time. Check what the change is about (e.g. `curl` a new page, `dist/tinysonic user list -c <dir>/c.toml`), then `kill "$(cat <dir>/pid)" && rm -rf <dir>`.

5. **Deploy:**
   ```bash
   .claude/skills/prod-deploy/scripts/deploy-local.sh dist/tinysonic
   ```
   It stops the service, copies the DB to `/var/lib/tinysonic/backup-<UTC>-pre-<sha>/`, keeps the old binary as `.prev`, installs, starts and waits up to 180 s (`WAIT=…` to change) for `/folksonic/rest/ping`. On failure it restores binary and database together and exits 1. Downtime is normally ~2 s plus migration time.

6. **Verify:** `.claude/skills/prod-deploy/scripts/verify.sh` — all five URLs should be 200 (`/navidrome/app/` included, to show Navidrome is unaffected), the log should show `listening on` and `scan complete` without errors, and the accounts should be unchanged.

7. **Report** what was deployed (commit), downtime, the backup directory, and anything not verified. Old `backup-*` directories are kept; mention them occasionally so the user can prune.

## Rolling back by hand

Only binary and database together are a consistent rollback:
```bash
sudo systemctl stop tinysonic
sudo install -m 0755 /usr/local/bin/tinysonic.prev /usr/local/bin/tinysonic
sudo rm -f /var/lib/tinysonic/tinysonic.db /var/lib/tinysonic/tinysonic.db-wal /var/lib/tinysonic/tinysonic.db-shm
sudo cp -p /var/lib/tinysonic/backup-<…>/* /var/lib/tinysonic/
sudo systemctl start tinysonic
```
Restoring the database discards accounts, invites, playlists and stars created since the backup. Check first (`tinysonic user list`, `invite list`) and tell the user what would be lost before doing it.

## Known noise

Before #59, every start and CLI call warned that the config "holds a password" and is readable by others. It was a false positive (no password in the file; 0640 root:tinysonic is intended). Since #59 the warning appears only when the config holds `password` or `oidc.client_secret`.
