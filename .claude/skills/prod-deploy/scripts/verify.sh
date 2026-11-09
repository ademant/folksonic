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

# NRestarts counts automatic restarts. RESTARTS_BEFORE, when set (the value
# read before the deploy), makes any restart since then a failure. Result must
# be "success" unless the unit was stopped on purpose.
restarts=$(systemctl show tinysonic -p NRestarts --value)
result=$(systemctl show tinysonic -p Result --value)
echo "NRestarts=$restarts Result=$result"
if [ "$result" != success ]; then
	echo "WARN: tinysonic's last result is $result"
fi
failed=0
if [ -n "${RESTARTS_BEFORE:-}" ] && [ "$restarts" -gt "$RESTARTS_BEFORE" ]; then
	echo "FAIL: tinysonic restarted $((restarts - RESTARTS_BEFORE)) time(s) since the deploy"
	failed=1
fi

echo "--- log since last start"
since=$(systemctl show tinysonic -p ActiveEnterTimestamp --value)
sudo journalctl -u tinysonic --since "$since" --no-pager -o cat |
	grep -iE "listening on|scan complete|error|fail|panic|warn" |
	grep -v "readable by other users" | tail -n 10

echo "--- accounts"
(cd / && sudo -u tinysonic /usr/local/bin/tinysonic user list -c /etc/tinysonic/tinysonic.toml 2>/dev/null)

exit "$failed"
