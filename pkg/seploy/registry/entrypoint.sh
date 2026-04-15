#!/bin/sh
# Heartbeat watchdog. seploy bind-mounts $BEAT_FILE from the host and
# rewrites its content on every tick with a value that always differs
# from the previous tick. The watchdog polls the file and kills PID 1 as
# soon as two consecutive reads see identical content — i.e. seploy
# stopped writing — so the registry never leaks when seploy dies without
# cleanup.
set -u

INTERVAL="${SEPLOY_HEARTBEAT_TIMEOUT:-1}"
BEAT_FILE="${SEPLOY_HEARTBEAT_FILE:-/tmp/seploy-heartbeat}"

(prev=""
first=1
while true; do
    sleep "$INTERVAL"
    curr=$(cat "$BEAT_FILE" 2>/dev/null || true)
    if [ "$first" = "0" ] && [ "$curr" = "$prev" ]; then
        echo "seploy-registry: heartbeat unchanged, exiting" >&2
        kill 1
        exit 0
    fi
    prev="$curr"
    first=0
done) &

exec /entrypoint.sh "$@"
