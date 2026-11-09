#!/bin/sh
# Reports restarts and OOM kills of tinysonic.service. Run every few minutes by
# tinysonic-watch.timer. It keeps the last NRestarts and Result in STATE and
# reports only changes, so one event is reported once.
#
#   UNIT         unit to watch              (default tinysonic.service)
#   STATE        where the last values live (default /var/lib/tinysonic-watch/state)
#   NOTIFY_CMD   optional path to an executable, without arguments. It gets the
#                message as its only argument, e.g. a wrapper that sends mail
#                or calls ntfy.
#
# Every report also goes to the journal at priority crit, tag tinysonic-watch,
# so it shows up with `journalctl -t tinysonic-watch`.
set -eu

UNIT=${UNIT:-tinysonic.service}
STATE=${STATE:-/var/lib/tinysonic-watch/state}

restarts=$(systemctl show "$UNIT" -p NRestarts --value)
result=$(systemctl show "$UNIT" -p Result --value)
now="$restarts $result"

last=""
if [ -r "$STATE" ]; then
	last=$(cat "$STATE")
fi
mkdir -p "$(dirname "$STATE")"
printf '%s\n' "$now" > "$STATE"

# First run: nothing to compare with yet.
[ -n "$last" ] || exit 0
[ "$now" = "$last" ] && exit 0

last_restarts=${last%% *}
last_result=${last#* }
events=""
if [ "$restarts" -gt "$last_restarts" ]; then
	events="restarted $((restarts - last_restarts)) time(s), NRestarts=$restarts;"
fi
if [ "$result" != "$last_result" ]; then
	events="${events:+$events }Result changed from '$last_result' to '$result';"
fi
# A counter that went down (the unit was started by hand) is not an event.
[ -n "$events" ] || exit 0
msg="$UNIT: $events"

logger -p user.crit -t tinysonic-watch "$msg"
if [ -n "${NOTIFY_CMD:-}" ]; then
	$NOTIFY_CMD "$msg"
fi
