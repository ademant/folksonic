#!/bin/sh
# Server-side half of the Makefile's setup, deploy and rollback targets.
#
# The Makefile copies this script and its inputs into ~/tinysonic-deploy on
# the server and runs it there; the directory is removed when it exits. Root
# steps go through sudo.
#
#   sh remote.sh setup      # user, directories, unit, config if absent
#   sh remote.sh deploy     # install the uploaded binary, restart, health check
#   sh remote.sh rollback   # restore the previous binary
set -eu

NAME=tinysonic
BIN=/usr/local/bin/$NAME
CONF_DIR=/etc/$NAME
CONFIG=$CONF_DIR/$NAME.toml
UNIT=/etc/systemd/system/$NAME.service

# config_value KEY prints KEY's value from the installed config, unquoted.
config_value() {
	sudo sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\{0,1\}\([^\"#[:space:]]*\).*/\1/p" "$CONFIG" | head -n 1
}

# healthy waits for a Subsonic response from <base_path>/rest/ping. Without
# credentials it is an error envelope, which still proves the server answers.
healthy() {
	host=$(config_value host)
	case "$host" in "" | 0.0.0.0 | ::) host=127.0.0.1 ;; esac
	port=$(config_value port)
	base=$(config_value base_path)
	url="http://$host:${port:-4533}${base%/}/rest/ping"
	i=0
	while [ "$i" -lt 15 ]; do
		if curl -sS --max-time 2 "$url" 2>/dev/null | grep -q subsonic-response; then
			echo "$NAME answers at $url"
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "no Subsonic response from $url" >&2
	return 1
}

setup() {
	if ! id -u "$NAME" >/dev/null 2>&1; then
		sudo useradd --system --home-dir "/var/lib/$NAME" --shell /usr/sbin/nologin "$NAME"
	fi
	sudo install -d -m 0750 -o root -g "$NAME" "$CONF_DIR"
	if sudo test -e "$CONFIG"; then
		echo "keeping existing $CONFIG"
	else
		sudo install -m 0640 -o root -g "$NAME" folksonic.toml "$CONFIG"
		echo "installed $CONFIG; set music_dirs and base_url before the first deploy"
	fi
	sudo install -m 0644 "$NAME.service" "$UNIT"
	sudo systemctl daemon-reload
	sudo systemctl enable "$NAME"
}

deploy() {
	if ! sudo test -e "$CONFIG"; then
		echo "$CONFIG is missing; run make setup first" >&2
		exit 1
	fi
	if [ -e "$BIN" ]; then
		sudo cp -p "$BIN" "$BIN.prev"
	fi
	sudo install -m 0755 "$NAME" "$BIN"
	sudo systemctl restart "$NAME"
	if healthy; then
		return 0
	fi
	sudo journalctl -u "$NAME" -n 30 --no-pager >&2 || true
	if [ -e "$BIN.prev" ]; then
		echo "restoring the previous binary" >&2
		sudo install -m 0755 "$BIN.prev" "$BIN"
		sudo systemctl restart "$NAME"
	fi
	exit 1
}

rollback() {
	if [ ! -e "$BIN.prev" ]; then
		echo "no $BIN.prev to restore" >&2
		exit 1
	fi
	sudo install -m 0755 "$BIN.prev" "$BIN"
	sudo systemctl restart "$NAME"
	healthy
}

cd "$(dirname "$0")"
stage=$(pwd)
if [ "$(basename "$stage")" = "$NAME-deploy" ]; then
	trap 'rm -rf "$stage"' EXIT
fi

case "${1:-}" in
setup) setup ;;
deploy) deploy ;;
rollback) rollback ;;
*)
	echo "usage: $0 setup|deploy|rollback" >&2
	exit 2
	;;
esac
