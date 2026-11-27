#!/bin/bash
# Smoke test of Moorage against a Podman (or Docker) API socket.
#
# Starts a throwaway all-in-one Moorage on 127.0.0.1:5095 (agent port 5096) against the
# engine in DOCKER_HOST, then exercises the HTTP API: setup, create stack, up, logs, stats,
# exec (web terminal), down, delete. Prints PASS/FAIL per step and cleans up after itself
# (the stack, its containers, the Moorage process and its data dir).
#
# Usage:
#   DOCKER_HOST=unix:///run/podman/podman.sock scripts/dev/podman-smoke.sh            # rootful
#   DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock scripts/dev/podman-smoke.sh  # rootless
# Env:
#   MOORAGE_BIN   binary to run (default ~/moorage-dev/podman-bin; built from this repo if missing)
#   KEEP=1        keep the data dir for inspection
#   SMOKE_PORT    host port the test stack publishes (default 18095)
# Needs: curl, python3 (TOTP and a tiny WebSocket client, standard library only).
set -u

DOCKER_HOST=${DOCKER_HOST:?set DOCKER_HOST to the engine socket, e.g. unix:///run/podman/podman.sock}
export DOCKER_HOST
REPO=$(cd "$(dirname "$0")/../.." && pwd)
BIN=${MOORAGE_BIN:-$HOME/moorage-dev/podman-bin}
SMOKE_PORT=${SMOKE_PORT:-18095}
BASE=http://127.0.0.1:5095
STACK=podsmoke
mkdir -p "$HOME/moorage-dev"
WORK=$(mktemp -d "$HOME/moorage-dev/podman-smoke-XXXXXX")
JAR=$WORK/cookies
LOG=$WORK/moorage.log
PID=""
FAILS=0

pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s%s\n' "$1" "${2:+ — $2}"; FAILS=$((FAILS + 1)); }
api() { # method path [json]
  local m=$1 p=$2 d=${3:-}
  if [ -n "$d" ]; then
    curl -sS -b "$JAR" -c "$JAR" -X "$m" -H 'X-Moorage: 1' -H 'Content-Type: application/json' --data "$d" -w '\n%{http_code}' "$BASE$p"
  else
    curl -sS -b "$JAR" -c "$JAR" -X "$m" -H 'X-Moorage: 1' -w '\n%{http_code}' "$BASE$p"
  fi
}
code() { tail -n1 <<<"$1"; }
body() { sed '$d' <<<"$1"; }
jget() { python3 -c 'import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d = d[int(k)] if isinstance(d, list) else d.get(k, "")
print(d if not isinstance(d, (dict, list)) else json.dumps(d))' "$1"; }

cleanup() {
  # Best effort: the stack may already be gone.
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    api POST "/api/hosts/local/stacks/$STACK/actions/down" >/dev/null 2>&1
    for _ in $(seq 1 60); do
      r=$(api GET "/api/hosts/local/stacks/$STACK" 2>/dev/null)
      [ "$(code "$r")" = 404 ] && break
      [ "$(body "$r" | jget busy 2>/dev/null)" = "False" ] && [ "$(body "$r" | jget status 2>/dev/null)" = "inactive" ] && break
      sleep 1
    done
    api DELETE "/api/hosts/local/stacks/$STACK" >/dev/null 2>&1
    kill "$PID" 2>/dev/null
    wait "$PID" 2>/dev/null
  fi
  if [ "${KEEP:-}" = 1 ]; then echo "kept $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

# ---- binary --------------------------------------------------------------------
if [ ! -x "$BIN" ]; then
  echo "building $BIN"
  (cd "$REPO" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/moorage) || { fail build; exit 1; }
fi
if [ "$BIN" = "$HOME/moorage-dev/moorage" ]; then
  echo "refusing to use the shared dev binary"; exit 1
fi

# ---- start ---------------------------------------------------------------------
mkdir -p "$WORK/data" "$WORK/stacks"
MOORAGE_LISTEN=127.0.0.1:5095 MOORAGE_AGENT_LISTEN=127.0.0.1:5096 \
MOORAGE_DATA_DIR=$WORK/data MOORAGE_STACKS_DIR=$WORK/stacks MOORAGE_TERMINAL=true \
  "$BIN" >"$LOG" 2>&1 &
PID=$!
for _ in $(seq 1 50); do curl -fsS "$BASE/api/session" >/dev/null 2>&1 && break; sleep 0.2; done
if curl -fsS "$BASE/api/session" >/dev/null 2>&1; then pass "start all-in-one on :5095"; else fail "start" "see $LOG"; KEEP=1; exit 1; fi

# ---- setup (first admin with TOTP) ------------------------------------------------
TOKEN=$(grep -o 'setup_token=[^ ]*' "$LOG" | head -n1 | cut -d= -f2 | tr -d '"')
PASSWORD="smoke-$(date +%s)-pw"
r=$(api POST /api/setup/start "{\"token\":\"$TOKEN\",\"username\":\"smoke\",\"password\":\"$PASSWORD\"}")
SECRET=$(body "$r" | jget secret 2>/dev/null)
TOTP=$(python3 - "$SECRET" <<'PY'
import base64, hmac, hashlib, struct, sys, time
s = sys.argv[1]; s += "=" * (-len(s) % 8)
c = struct.pack(">Q", int(time.time()) // 30)
h = hmac.new(base64.b32decode(s), c, hashlib.sha1).digest()
o = h[-1] & 15
print("%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7fffffff) % 1000000))
PY
)
r=$(api POST /api/setup/finish "{\"token\":\"$TOKEN\",\"username\":\"smoke\",\"code\":\"$TOTP\"}")
if [ "$(code "$r")" = 200 ]; then pass "setup admin"; else fail "setup admin" "$(body "$r")"; exit 1; fi

# ---- engine detection ------------------------------------------------------------
r=$(api GET /api/hosts/local/info)
INFO=$(body "$r")
ENGINE=$(jget engine <<<"$INFO"); EVER=$(jget engineVersion <<<"$INFO"); ROOTLESS=$(jget rootless <<<"$INFO")
if [ "$(jget reachable <<<"$INFO")" = "True" ] && [ -n "$ENGINE" ]; then
  pass "engine detected: $ENGINE $EVER rootless=${ROOTLESS:-False}"
else
  fail "engine detection" "$INFO"
fi

# ---- create stack --------------------------------------------------------------
# Fully qualified images: rootless Podman without short-name aliases refuses "busybox".
COMPOSE=$(cat <<YAML
services:
  web:
    image: docker.io/traefik/whoami:latest
    ports:
      - "127.0.0.1:${SMOKE_PORT}:80"
  ticker:
    image: docker.io/library/busybox:latest
    command: ["sh", "-c", "i=0; while true; do i=\$\$((i+1)); echo smoke-tick \$\$i; echo smoke-err \$\$i >&2; sleep 1; done"]
YAML
)
REQ=$(python3 -c 'import json,sys; print(json.dumps({"name": sys.argv[1], "compose": sys.argv[2], "env": ""}))' "$STACK" "$COMPOSE")
r=$(api POST /api/hosts/local/stacks "$REQ")
if [ "$(code "$r")" -lt 300 ]; then pass "create stack"; else fail "create stack" "$(body "$r")"; fi

# ---- up ------------------------------------------------------------------------
wait_idle() { # prints the final stack JSON
  local r
  for _ in $(seq 1 240); do
    r=$(api GET "/api/hosts/local/stacks/$STACK")
    if [ "$(body "$r" | jget busy)" != "True" ]; then body "$r"; return; fi
    sleep 1
  done
  body "$r"
}
r=$(api POST "/api/hosts/local/stacks/$STACK/actions/up")
if [ "$(code "$r")" = 202 ]; then
  ST=$(wait_idle)
  if [ "$(jget status <<<"$ST")" = up ]; then pass "up (status up, $(jget running <<<"$ST") running)"; else fail "up" "status $(jget status <<<"$ST"), see the op output in the UI"; fi
else
  fail "up" "$(body "$r")"
fi
if curl -fsS "http://127.0.0.1:$SMOKE_PORT/" | grep -q Hostname; then pass "published port answers"; else fail "published port $SMOKE_PORT"; fi

# ---- WebSocket helper (logs, terminal) ----------------------------------------------
cat >"$WORK/ws.py" <<'PY'
# Minimal WebSocket client (RFC 6455, no extensions) for the smoke test.
import base64, json, os, socket, struct, sys, time
mode, path, cookie_file = sys.argv[1], sys.argv[2], sys.argv[3]
cookie = ""
for line in open(cookie_file):
    f = line.rstrip("\n").split("\t")
    if len(f) == 7 and f[5] == "moorage_session":
        cookie = "%s=%s" % (f[5], f[6])
s = socket.create_connection(("127.0.0.1", 5095), timeout=20)
key = base64.b64encode(os.urandom(16)).decode()
s.sendall(("GET %s HTTP/1.1\r\nHost: 127.0.0.1:5095\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
           "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nCookie: %s\r\n\r\n" % (path, key, cookie)).encode())
buf = b""
while b"\r\n\r\n" not in buf:
    buf += s.recv(4096)
head, buf = buf.split(b"\r\n\r\n", 1)
if b" 101 " not in head.split(b"\r\n")[0]:
    print("handshake: " + head.split(b"\r\n")[0].decode()); sys.exit(2)
def recv_exact(n):
    global buf
    while len(buf) < n:
        d = s.recv(65536)
        if not d: raise EOFError
        buf += d
    out, buf = buf[:n], buf[n:]
    return out
def read_frame():
    b1, b2 = recv_exact(2)
    n = b2 & 127
    if n == 126: n = struct.unpack(">H", recv_exact(2))[0]
    elif n == 127: n = struct.unpack(">Q", recv_exact(8))[0]
    return b1 & 15, recv_exact(n)
def send(op, data):
    mask = os.urandom(4)
    hdr = bytes([0x80 | op])
    n = len(data)
    hdr += bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + struct.pack(">H", n)
    s.sendall(hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))
deadline = time.time() + 20
if mode == "logs":
    out, err = 0, 0
    while time.time() < deadline and (out == 0 or err == 0):
        op, data = read_frame()
        if op != 1: continue
        msg = json.loads(data)
        for l in msg.get("lines", []):
            if "smoke-tick" in l["m"] and l["s"] == "stdout" and l["t"]: out += 1
            if "smoke-err" in l["m"] and l["s"] == "stderr": err += 1
        if "end" in msg: break
    print("stdout=%d stderr=%d" % (out, err)); sys.exit(0 if out and err else 1)
else:
    send(2, b"echo moorage-$((40+2))\n")
    got = b""
    while time.time() < deadline and b"moorage-42" not in got:
        op, data = read_frame()
        if op == 2: got += data
        elif op == 1 and b'"error"' in data: print(data.decode()); sys.exit(1)
        elif op == 8: break
    send(2, b"exit\n")
    ok = b"moorage-42" in got
    print("exec output %s" % ("seen" if ok else "missing")); sys.exit(0 if ok else 1)
PY

# ---- logs ----------------------------------------------------------------------
sleep 2
out=$(python3 "$WORK/ws.py" logs "/api/hosts/local/ws/logs?stack=$STACK&container=ticker&tail=20" "$JAR" 2>&1)
if [ $? = 0 ]; then pass "logs over WebSocket ($out)"; else fail "logs" "$out"; fi

# ---- stats ---------------------------------------------------------------------
r=$(api GET "/api/hosts/local/stacks/$STACK/stats")
STATS=$(body "$r")
if [ "$(code "$r")" = 200 ] && python3 - "$STATS" <<'PY'
import json, math, sys
rows = json.loads(sys.argv[1])
assert rows, "no rows"
for s in rows:
    assert not math.isnan(s["cpu"]) and s["cpu"] >= 0, s
    # A value or a reason, never a silent 0.
    assert s.get("memNote") or s["memUsed"] > 0, s
    print("  %s cpu=%s mem=%s %s" % (s["id"][:12], s.get("cpuNote") and "–" or "%.1f%%" % s["cpu"],
          s.get("memNote") and "–" or s["memUsed"], (s.get("cpuNote") or s.get("memNote") or "")[:80]))
PY
then pass "stats"; else fail "stats" "$STATS"; fi

# ---- exec (web terminal) -----------------------------------------------------------
# Terminal upgrades need a one-time ticket since M17A (POST /api/terminal/ticket).
r=$(api POST /api/terminal/ticket "{\"kind\":\"stack\",\"host\":\"local\",\"stack\":\"$STACK\",\"container\":\"ticker\"}")
TICKET=$(body "$r" | jget ticket 2>/dev/null)
out=$(python3 "$WORK/ws.py" term "/api/hosts/local/stacks/$STACK/terminal?container=ticker&shell=sh&cols=80&rows=24&ticket=$TICKET" "$JAR" 2>&1)
if [ $? = 0 ]; then pass "exec in terminal ($out)"; else fail "exec" "$out"; fi

# ---- down ----------------------------------------------------------------------
r=$(api POST "/api/hosts/local/stacks/$STACK/actions/down")
if [ "$(code "$r")" = 202 ]; then
  ST=$(wait_idle)
  if [ "$(jget status <<<"$ST")" = inactive ]; then pass "down"; else fail "down" "status $(jget status <<<"$ST")"; fi
else
  fail "down" "$(body "$r")"
fi

# ---- delete --------------------------------------------------------------------
r=$(api DELETE "/api/hosts/local/stacks/$STACK")
if [ "$(code "$r")" = 204 ] && [ ! -e "$WORK/stacks/$STACK" ]; then pass "delete stack"; else fail "delete stack" "$(body "$r")"; fi

if [ "$FAILS" = 0 ]; then echo "ALL PASS ($ENGINE $EVER)"; else echo "$FAILS step(s) failed; Moorage log: $LOG (KEEP=1 keeps it)"; fi
[ "$FAILS" = 0 ]
