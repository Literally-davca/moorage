#!/bin/bash
# Dev helper for M6: start watchdog test agent N (1..9) against the local hub.
# Usage: wd-agent.sh N [join-token]   – runs in the background, log in ~/moorage-dev/wdN/agent.log
# HUB=host:port overrides the hub address (e.g. to cut one agent off in tests).
# Stop:  kill $(cat ~/moorage-dev/wdN/pid)
set -e
N=$1
DEV=$HOME/moorage-dev
A=$DEV/wd$N
mkdir -p "$A/data" "$A/stacks"
PORT=$((5081 + N))
cd "$A"
MOORAGE_HUB=${HUB:-127.0.0.1:5081} MOORAGE_JOIN_TOKEN="${2:-}" \
MOORAGE_WATCHDOG_LISTEN=127.0.0.1:$PORT MOORAGE_WATCHDOG_ADDR=127.0.0.1:$PORT \
MOORAGE_DATA_DIR=$A/data MOORAGE_STACKS_DIR=$A/stacks \
nohup "$DEV/moorage" agent >>"$A/agent.log" 2>&1 &
echo $! > "$A/pid"
echo "agent $N pid $! watchdog port $PORT"
