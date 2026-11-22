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
#   sh remote.sh apache [vhost]   # add the relay proxy lines to the HTTPS vhost (#161)
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

# apache adds the WebSocket relay lines of apache-subpath.conf to the HTTPS vhost,
# above its single "ProxyPass /folksonic" line (the first match wins). It is
# idempotent, keeps a backup, and restores that backup if configtest fails.
apache() {
	vhost=${1:-/etc/apache2/sites-enabled/000-default-le-ssl.conf}
	snippet=apache-subpath.conf
	if ! sudo test -f "$vhost"; then
		echo "$vhost not found; pass the HTTPS vhost with APACHE_VHOST=" >&2
		exit 1
	fi
	if [ ! -f "$snippet" ]; then
		echo "$snippet is missing from the upload" >&2
		exit 1
	fi
	relay=$(mktemp)
	grep -E '^ProxyPass(Reverse)?[[:space:]]+/folksonic/api/relay/connect[[:space:]]' "$snippet" > "$relay"
	if [ "$(wc -l < "$relay")" -ne 2 ]; then
		echo "$snippet does not carry the two relay lines" >&2
		rm -f "$relay"
		exit 1
	fi

	# Modules first: if one is missing, nothing in the vhost has changed yet.
	# mod_proxy_wstunnel carries the upgrade; the others are the proxy itself.
	for m in proxy proxy_http proxy_wstunnel headers; do
		sudo a2enmod "$m" >/dev/null
	done

	backup=
	if sudo grep -q '^[[:space:]]*ProxyPass[[:space:]][[:space:]]*/folksonic/api/relay/connect[[:space:]]' "$vhost"; then
		echo "the relay proxy line is already in $vhost"
	else
		n=$(sudo grep -c '^[[:space:]]*ProxyPass[[:space:]][[:space:]]*/folksonic[[:space:]]' "$vhost" || true)
		if [ "$n" -ne 1 ]; then
			echo "$vhost has $n 'ProxyPass /folksonic' lines; add the relay lines by hand" >&2
			rm -f "$relay"
			exit 1
		fi
		backup="$vhost.bak-$(date -u +%Y%m%d%H%M%S)"
		sudo cp -p "$vhost" "$backup"
		tmp=$(mktemp)
		sudo awk -v relayfile="$relay" '
			BEGIN { while ((getline l < relayfile) > 0) block = block l "\n" }
			/^[ \t]*ProxyPass[ \t]+\/folksonic[ \t]/ && !done { printf "%s", block; done = 1 }
			{ print }' "$vhost" > "$tmp"
		sudo install -m 0644 -o root -g root "$tmp" "$vhost"
		rm -f "$tmp"
		echo "added the relay lines to $vhost (backup: $backup)"
	fi
	rm -f "$relay"

	if ! sudo apache2ctl configtest; then
		if [ -n "$backup" ]; then
			echo "configtest failed; restoring $backup" >&2
			sudo cp -p "$backup" "$vhost"
		fi
		exit 1
	fi
	sudo systemctl reload apache2
	echo "apache reloaded with the relay proxy line"
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
apache) apache "${2:-}" ;;
*)
	echo "usage: $0 setup|deploy|rollback|apache [vhost]" >&2
	exit 2
	;;
esac
