---
name: prod-accounts
description: Manage accounts and invitations on the production tinysonic/folksonic server (cloud.ademant.de/folksonic). Use whenever the user asks to create, list or revoke an invite or invitation code/token/link, add or list users, give someone access, make someone a librarian, set or reset a password, or pair an app/device by QR, on the live server. Not for changing the invite feature's code.
---

# Accounts and invitations on cloud.ademant.de

The CLI works directly on the production database, so run it as the service user with the production config. Run it from `/` (the service user cannot read the repo directory) and drop stderr, which only carries the known false "holds a password" warning:

```bash
cd / && sudo -u tinysonic /usr/local/bin/tinysonic <command> -c /etc/tinysonic/tinysonic.toml 2>/dev/null
```

Check `… <command> --help` when unsure; the CLI has changed over time (e.g. `--admin` became `--librarian`).

## Roles

- **user** — listens, manages own playlists, stars, API keys, passkeys.
- **librarian** — additionally manages the library (rescan, and the library page once it exists). Account administration is CLI-only; there is no admin role in the web UI.

## Invitations

```bash
invite create [--expires 7d|12h] [--librarian] [--note "for Anna"]
invite list
invite revoke <id>
```

- Codes are single use and stored hashed. `create` prints the code (`inv-XXXX-XXXX-XXXX-XXXX`) and a link `https://cloud.ademant.de/folksonic/login?invite=…` exactly once; `list` never shows codes. A lost code cannot be recovered — revoke it and create a new one.
- Default expiry is 7 days. Give a librarian invite a short expiry (e.g. `1d`) and a `--note`.
- Present the result as: code, link, role, expiry (UTC), single use. Mention other still-open invites from `invite list`, especially open librarian invites, since those are the sensitive ones.
- Registration through the link can use a passkey only. Such accounts have no password, which matters for Subsonic apps (see below).

## Users

```bash
user list
user add <name> [--librarian]        # prompts for a password
user set-password <name>             # prompts
user disable|enable|remove <name>
pair --user <name>                   # QR code for the Android app, valid 5 min
```

Commands that prompt for a password need the user's own terminal: suggest they run it themselves with `!`, e.g. `! sudo -u tinysonic tinysonic user set-password ademin -c /etc/tinysonic/tinysonic.toml`. Don't invent or choose passwords for them.

## Connecting apps

- Server URL for any Subsonic app: `https://cloud.ademant.de/folksonic`.
- Stock Subsonic apps sign in with username + password. A passkey-only account needs a password first (`set-password`), or an app that supports OpenSubsonic API keys.
- QR pairing (account page or `pair --user`) gives the app its own API key. If pairing "fails", diagnose with the prod-diagnose skill before changing anything.

## Don't test on real accounts

Creating keys, pairing tokens or sessions on a real account to "check it works" changes that account and has been refused before. Reproduce on a copy of the database instead: `.claude/skills/prod-deploy/scripts/copy-instance.sh /usr/local/bin/tinysonic`, then run the CLI with `-c <dir>/c.toml` against the copy.
