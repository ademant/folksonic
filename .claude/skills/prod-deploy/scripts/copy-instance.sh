#!/bin/sh
# Start a throwaway tinysonic on a snapshot of the production database, to try
# a new binary's migrations or reproduce a bug without touching production.
#
#   copy-instance.sh <binary> [port]      default port 4598
#
# Prints the work directory, the startup time and the base URL. The instance
# runs with --no-scan --no-watch and an empty music directory. Run CLI commands
# against it with: <binary> <command> -c <dir>/c.toml
# Stop it with:     kill "$(cat <dir>/pid)" && rm -rf <dir>
# Set TMPDIR to put the work directory somewhere other than /tmp.
set -eu

BIN=$(realpath "$1")
PORT=${2:-4598}
DB=/var/lib/tinysonic/tinysonic.db

DIR=$(mktemp -d "${TMPDIR:-/tmp}/tinysonic-copy.XXXXXX")
chmod 700 "$DIR"
mkdir "$DIR/music"

# SQLite's online backup gives a consistent copy while the service runs.
sudo python3 -c '
import sqlite3, sys
src = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
dst = sqlite3.connect(sys.argv[2])
src.backup(dst)
dst.close()
' "$DB" "$DIR/t.db"
sudo chown "$(id -u):$(id -g)" "$DIR/t.db"
chmod 600 "$DIR/t.db"

cat >"$DIR/c.toml" <<EOF
music_dirs    = ["$DIR/music"]
host          = "127.0.0.1"
port          = $PORT
base_url      = "http://127.0.0.1:$PORT/folksonic"
base_path     = "/folksonic"
database_path = "$DIR/t.db"
covers_dir    = "$DIR/covers"
EOF
chmod 600 "$DIR/c.toml"

(cd "$DIR" && "$BIN" -c "$DIR/c.toml" --no-scan --no-watch >"$DIR/log" 2>&1 & echo $! >"$DIR/pid")

i=0
until grep -q "listening on" "$DIR/log"; do
	if ! kill -0 "$(cat "$DIR/pid")" 2>/dev/null; then
		echo "instance exited; log:" >&2
		cat "$DIR/log" >&2
		exit 1
	fi
	if [ "$i" -ge 300 ]; then
		echo "not listening after 300s; log in $DIR/log" >&2
		exit 1
	fi
	i=$((i + 1))
	sleep 1
done

echo "dir:     $DIR"
echo "startup: ${i}s (migrations included)"
echo "url:     http://127.0.0.1:$PORT/folksonic"
