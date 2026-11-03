#!/bin/sh
# Post-deploy check of cloud.ademant.de: public endpoints through Apache,
# recent service log, accounts, and that Navidrome still answers.
set -u

for path in /folksonic/ /folksonic/login /folksonic/account /folksonic/rest/ping /navidrome/app/; do
	curl -s -o /dev/null -w "%{http_code} $path\n" "https://cloud.ademant.de$path"
done

echo "--- service"
systemctl is-active tinysonic
ps -o rss=,etime= -C tinysonic | awk '{printf "rss %d MB, up %s\n", $1/1024, $2}'

echo "--- log since last start"
since=$(systemctl show tinysonic -p ActiveEnterTimestamp --value)
sudo journalctl -u tinysonic --since "$since" --no-pager -o cat |
	grep -E "listening on|scan complete|error|fail|panic|warning" |
	grep -v "readable by other users" | tail -n 10

echo "--- accounts"
(cd / && sudo -u tinysonic /usr/local/bin/tinysonic user list -c /etc/tinysonic/tinysonic.toml 2>/dev/null)
