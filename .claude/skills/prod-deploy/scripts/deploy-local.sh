#!/bin/sh
# Deploy a built tinysonic binary on this host (cloud.ademant.de).
#
#   deploy-local.sh [binary]        default: dist/tinysonic
#
# Stops the service, backs up the database, keeps the old binary as
# tinysonic.prev, installs the new one and waits until it answers. Startup can
# take tens of seconds when a migration runs, so it waits as long as the
# service stays up (WAIT seconds, default 180) and fails fast if it exits.
# On failure it restores binary AND database together: the new binary may have
# migrated the schema, and the old binary must not run on a migrated database.
set -eu

SRC=${1:-dist/tinysonic}
NAME=tinysonic
BIN=/usr/local/bin/$NAME
STATE=/var/lib/$NAME
DB=$STATE/$NAME.db
CONFIG=/etc/$NAME/$NAME.toml
WAIT=${WAIT:-180}

if [ ! -x "$SRC" ]; then
	echo "no binary at $SRC; run make build first" >&2
	exit 1
fi
rev=$(git -C "$(dirname "$SRC")" rev-parse --short HEAD 2>/dev/null || echo unknown)

config_value() {
	sudo sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\{0,1\}\([^\"#[:space:]]*\).*/\1/p" "$CONFIG" | head -n 1
}
port=$(config_value port)
base=$(config_value base_path)
URL="http://127.0.0.1:${port:-4533}${base%/}/rest/ping"

# healthy waits for a Subsonic envelope from ping. Without credentials it is
# an error envelope, which still proves the server is up.
healthy() {
	i=0
	while [ "$i" -lt "$WAIT" ]; do
		if curl -s --max-time 2 "$URL" 2>/dev/null | grep -q subsonic-response; then
			echo "answering after ${i}s at $URL"
			return 0
		fi
		if ! systemctl is-active -q "$NAME"; then
			echo "service exited after ${i}s" >&2
			return 1
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "no answer within ${WAIT}s" >&2
	return 1
}

backup=$STATE/backup-$(date -u +%Y%m%d-%H%M%S)-pre-$rev

sudo systemctl stop "$NAME"
sudo install -d -m 0750 -o "$NAME" -g "$NAME" "$backup"
for f in "$DB" "$DB-wal" "$DB-shm"; do
	if sudo test -e "$f"; then
		sudo cp -p "$f" "$backup/"
	fi
done
echo "database backed up to $backup"
if [ -e "$BIN" ]; then
	sudo cp -p "$BIN" "$BIN.prev"
fi
sudo install -m 0755 "$SRC" "$BIN"
sudo systemctl start "$NAME"

if healthy; then
	echo "deployed $rev; previous binary: $BIN.prev"
	exit 0
fi

echo "deploy failed; restoring previous binary and database" >&2
sudo journalctl -u "$NAME" -n 30 --no-pager -o cat >&2 || true
sudo systemctl stop "$NAME"
if [ -e "$BIN.prev" ]; then
	sudo install -m 0755 "$BIN.prev" "$BIN"
fi
sudo rm -f "$DB" "$DB-wal" "$DB-shm"
for f in "$backup"/*; do
	sudo cp -p "$f" "$STATE/"
done
sudo systemctl start "$NAME"
healthy || echo "previous version does not answer either; check journalctl -u $NAME" >&2
exit 1
