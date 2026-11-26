#!/usr/bin/env bash
# PSS memory gate: Moorage must stay under 100 MiB PSS with a realistic load.
#
#   ./scripts/dev/pss-gate.sh          # Linux with Docker; build the UI first (cd web && npm run build)
#
# It builds Moorage, starts it as a real process against the local Docker daemon, runs the
# load in internal/pssgate (go test -tags pssgate: 15 agents over the tunnel, 100 HTTP
# checks, 2 clients following a log stream), lets it settle and reads PSS from
# /proc/<pid>/smaps_rollup of the Moorage process only (several samples, the highest
# counts).
#
# Exit codes: 0 passed (a warning above PSS_WARN_MB), 1 over PSS_LIMIT_MB, 2 the
# measurement itself failed (no Linux /proc, no Docker, Moorage or the load didn't come
# up). A failed measurement is never a pass.
#
# Settings: PSS_LIMIT_MB (100), PSS_WARN_MB (80), PSS_SETTLE (seconds after the load is up,
# 60), PSS_SAMPLES (5), PSS_RESULT_FILE (markdown line for release notes, default
# pss-result.md), PSS_AGENTS / PSS_CHECKS / PSS_LOG_CLIENTS. MB here means MiB.
set -euo pipefail

LIMIT_MB="${PSS_LIMIT_MB:-100}"
WARN_MB="${PSS_WARN_MB:-80}"
SETTLE="${PSS_SETTLE:-60}"
SAMPLES="${PSS_SAMPLES:-5}"
RESULT_FILE="${PSS_RESULT_FILE:-pss-result.md}"
LOGGER="moorage-pss-logger-$$"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

summary() {
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		echo "$*" >>"$GITHUB_STEP_SUMMARY"
	fi
}

WORK=""
MOORAGE_PID=""
LOAD_PID=""
cleanup() {
	set +e
	[ -n "$WORK" ] && touch "$WORK/stop" 2>/dev/null
	[ -n "$LOAD_PID" ] && kill "$LOAD_PID" 2>/dev/null
	[ -n "$MOORAGE_PID" ] && kill "$MOORAGE_PID" 2>/dev/null && wait "$MOORAGE_PID" 2>/dev/null
	docker rm -f "$LOGGER" >/dev/null 2>&1
	[ -n "$WORK" ] && rm -rf "$WORK"
}
trap cleanup EXIT

# fail_tech: the measurement could not be made. Never reported as a pass.
fail_tech() {
	echo "::error title=PSS gate::could not measure Moorage's memory: $*" >&2
	summary "### PSS memory gate"
	summary "Could not measure: $*"
	if [ -n "$WORK" ]; then
		for f in moorage.log load.log; do
			if [ -f "$WORK/$f" ]; then
				echo "--- last lines of $f ---" >&2
				tail -n 40 "$WORK/$f" >&2
			fi
		done
	fi
	exit 2
}

[ "$(uname -s)" = "Linux" ] || fail_tech "it needs Linux (/proc/<pid>/smaps_rollup); this is $(uname -s)"
[ -r /proc/self/smaps_rollup ] || fail_tech "/proc/self/smaps_rollup is missing (kernel older than 4.14?)"
command -v docker >/dev/null 2>&1 || fail_tech "docker is not installed"
docker info >/dev/null 2>&1 || fail_tech "the Docker daemon is not reachable"
command -v go >/dev/null 2>&1 || fail_tech "go is not installed"
[ -f internal/web/dist/index.html ] || fail_tech "the UI is not built (cd web && npm ci && npm run build)"

WORK="$(mktemp -d)"
mkdir -p "$WORK/data" "$WORK/stacks"

free_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()' 2>/dev/null ||
		echo $((20000 + RANDOM % 20000))
}
UI_PORT="$(free_port)"
AGENT_PORT="$(free_port)"

echo "==> Building Moorage and the load"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=pssgate" -o "$WORK/moorage" ./cmd/moorage ||
	fail_tech "go build of Moorage failed"
go test -c -tags pssgate -o "$WORK/pssload" ./internal/pssgate || fail_tech "building the load (go test -c -tags pssgate) failed"

echo "==> Starting a container that keeps logging"
docker run -d --name "$LOGGER" --label moorage.pssgate=1 alpine:3.20 \
	sh -c 'i=0; while true; do i=$((i+1)); echo "pss gate log line $i"; sleep 0.05; done' >/dev/null ||
	fail_tech "could not start the log container (docker run alpine:3.20)"

echo "==> Starting Moorage (UI 127.0.0.1:$UI_PORT, agents 127.0.0.1:$AGENT_PORT)"
MOORAGE_DATA_DIR="$WORK/data" MOORAGE_STACKS_DIR="$WORK/stacks" \
	MOORAGE_LISTEN="127.0.0.1:$UI_PORT" MOORAGE_AGENT_LISTEN="127.0.0.1:$AGENT_PORT" \
	MOORAGE_SOCKET="$WORK/data/moorage.sock" MOORAGE_SELF_UPDATE=false \
	"$WORK/moorage" >"$WORK/moorage.log" 2>&1 &
MOORAGE_PID=$!

alive() { kill -0 "$1" 2>/dev/null; }
for _ in $(seq 1 60); do
	[ -S "$WORK/data/moorage.sock" ] && break
	alive "$MOORAGE_PID" || fail_tech "Moorage exited during start"
	sleep 0.5
done
[ -S "$WORK/data/moorage.sock" ] || fail_tech "Moorage's socket did not appear within 30 s"

echo "==> Starting the load"
PSS_SOCKET="$WORK/data/moorage.sock" PSS_AGENT_ADDR="127.0.0.1:$AGENT_PORT" \
	PSS_READY_FILE="$WORK/ready" PSS_STOP_FILE="$WORK/stop" PSS_LOG_CONTAINER="$LOGGER" \
	"$WORK/pssload" -test.run '^TestPSSLoad$' -test.v -test.timeout 20m >"$WORK/load.log" 2>&1 &
LOAD_PID=$!

for _ in $(seq 1 360); do
	[ -f "$WORK/ready" ] && break
	alive "$MOORAGE_PID" || fail_tech "Moorage exited while the load started"
	alive "$LOAD_PID" || fail_tech "the load exited before it was up"
	sleep 0.5
done
[ -f "$WORK/ready" ] || fail_tech "the load was not up within 3 minutes"

echo "==> Load is up, settling for ${SETTLE}s"
sleep "$SETTLE"
alive "$MOORAGE_PID" || fail_tech "Moorage exited under load"

read_kb() { # read_kb FIELD: kB of FIELD in Moorage's smaps_rollup
	awk -v f="$1:" '$1 == f { print $2; found=1 } END { if (!found) exit 1 }' "/proc/$MOORAGE_PID/smaps_rollup"
}
MAX_PSS=0
MAX_RSS=0
for i in $(seq 1 "$SAMPLES"); do
	pss="$(read_kb Pss)" || fail_tech "could not read Pss from /proc/$MOORAGE_PID/smaps_rollup"
	rss="$(read_kb Rss)" || fail_tech "could not read Rss from /proc/$MOORAGE_PID/smaps_rollup"
	[[ "$pss" =~ ^[0-9]+$ ]] || fail_tech "unexpected Pss value '$pss'"
	echo "  sample $i: PSS ${pss} kB, RSS ${rss} kB"
	[ "$pss" -gt "$MAX_PSS" ] && MAX_PSS="$pss"
	[ "$rss" -gt "$MAX_RSS" ] && MAX_RSS="$rss"
	[ "$i" -lt "$SAMPLES" ] && sleep 3
done

touch "$WORK/stop"
if ! wait "$LOAD_PID"; then
	LOAD_PID=""
	fail_tech "the load reported a problem (see load.log), so the number would not mean anything"
fi
LOAD_PID=""
grep -E 'agents connected|checks created|log lines streamed' "$WORK/load.log" | sed 's/^ */  load: /' || true

PSS_MB="$(awk -v k="$MAX_PSS" 'BEGIN { printf "%.1f", k / 1024 }')"
RSS_MB="$(awk -v k="$MAX_RSS" 'BEGIN { printf "%.1f", k / 1024 }')"
LINE="Memory (PSS gate): **${PSS_MB} MiB PSS** (RSS ${RSS_MB} MiB) with 15 agents, 100 checks and 2 log streams; limit ${LIMIT_MB} MiB."
mkdir -p "$(dirname "$RESULT_FILE")"
echo "$LINE" >"$RESULT_FILE"
summary "### PSS memory gate"
summary "$LINE"
echo "$LINE"

if [ "$MAX_PSS" -gt $((LIMIT_MB * 1024)) ]; then
	echo "::error title=PSS gate::Moorage uses ${PSS_MB} MiB PSS, over the ${LIMIT_MB} MiB limit" >&2
	exit 1
fi
if [ "$MAX_PSS" -gt $((WARN_MB * 1024)) ]; then
	echo "::warning title=PSS gate::Moorage uses ${PSS_MB} MiB PSS, over the ${WARN_MB} MiB warning level" >&2
fi
echo "PSS gate passed."
