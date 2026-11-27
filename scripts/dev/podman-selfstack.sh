#!/bin/bash
# Moorage in a container under Podman (M19M): the stack it runs from is detected, Save
# shows "saved, not applied" (M19A), Apply & restart goes through the helper container
# and comes back, and the CLI works inside the container.
#
# Usage (rootless):
#   IMAGE=localhost/moorage:m19 DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock scripts/dev/podman-selfstack.sh
# The image must contain this repo's binary (see the summary in PLAN.md M19M). Needs
# curl, python3 and `podman compose` (podman-compose) to start the first container.
set -u
SOCK=${DOCKER_HOST:?set DOCKER_HOST to the Podman socket}
SOCK=${SOCK#unix://}
IMAGE=${IMAGE:-localhost/moorage:m19}
PORT=${SELF_PORT:-5097}
BASE=http://127.0.0.1:$PORT
ROOT=$HOME/moorage-dev/selfstack
STACKS=$ROOT/stacks
DIR=$STACKS/moorage
JAR=$ROOT/cookies
FAILS=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s%s\n' "$1" "${2:+ — $2}"; FAILS=$((FAILS + 1)); }
api() {
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
    d = d[int(k)] if isinstance(d, list) else (d or {}).get(k, "")
print(d if not isinstance(d, (dict, list)) else json.dumps(d))' "$1"; }
compose_text() { # $1 = extra env line
  cat <<EOF
services:
  moorage:
    image: $IMAGE
    restart: unless-stopped
    ports:
      - "127.0.0.1:$PORT:5080"
    volumes:
      - $SOCK:/var/run/docker.sock
      - selfdata:/data
      - $STACKS:$STACKS
    environment:
      MOORAGE_LISTEN: ":5080"
      MOORAGE_AGENT_LISTEN: "off"
      MOORAGE_STACKS_DIR: $STACKS
$1
volumes:
  selfdata:
EOF
}
wait_up() {
  for _ in $(seq 1 "${1:-90}"); do curl -fsS "$BASE/api/session" >/dev/null 2>&1 && return 0; sleep 1; done
  return 1
}
cleanup() {
  (cd "$DIR" 2>/dev/null && podman compose down -v >/dev/null 2>&1)
  podman rm -f podsnap-web-1 >/dev/null 2>&1
  [ "${KEEP:-}" = 1 ] || rm -rf "$ROOT"
}
trap cleanup EXIT

rm -rf "$ROOT"
mkdir -p "$DIR"
compose_text "      MOORAGE_LOG_LEVEL: info" >"$DIR/compose.yaml"
(cd "$DIR" && podman compose up -d >/dev/null 2>&1)
if wait_up 60; then pass "Moorage runs in a container"; else fail "start" "$(podman logs moorage-moorage-1 2>&1 | tail -5)"; exit 1; fi

# setup (first admin with TOTP)
TOKEN=$(podman logs moorage-moorage-1 2>&1 | grep -o 'setup_token=[^ ]*' | head -n1 | cut -d= -f2 | tr -d '"')
r=$(api POST /api/setup/start "{\"token\":\"$TOKEN\",\"username\":\"self\",\"password\":\"self-$(date +%s)-pw\"}")
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
r=$(api POST /api/setup/finish "{\"token\":\"$TOKEN\",\"username\":\"self\",\"code\":\"$TOTP\"}")
[ "$(code "$r")" = 200 ] && pass "setup admin" || { fail "setup" "$(body "$r")"; exit 1; }

r=$(api GET /api/hosts/local/stacks/moorage)
if [ "$(body "$r" | jget self)" = "True" ]; then pass "own stack detected (service $(body "$r" | jget selfService))"; else fail "self stack" "$(body "$r" | head -c 300)"; fi
sleep 3
r=$(api GET /api/hosts/local/stacks/moorage)
[ "$(body "$r" | jget selfPending)" != "True" ] && pass "files match the running container (not pending)" || fail "selfPending on a fresh start"
[ "$(body "$r" | jget selfRollback)" = "True" ] && pass "rollback target known" || fail "selfRollback" "$(body "$r" | jget selfRollback)"

# Save without Apply → saved, not applied
NEW=$(compose_text "      MOORAGE_LOG_LEVEL: debug")
r=$(api PUT /api/hosts/local/stacks/moorage "$(python3 -c 'import json,sys; print(json.dumps({"compose": sys.argv[1], "env": ""}))' "$NEW")")
[ "$(code "$r")" -lt 300 ] && pass "save" || fail "save" "$(body "$r")"
r=$(api GET /api/hosts/local/stacks/moorage)
[ "$(body "$r" | jget selfPending)" = "True" ] && pass "saved, not applied is shown" || fail "selfPending after save"

# Apply & restart through the helper container
r=$(api POST /api/hosts/local/stacks/moorage/self-apply "$(python3 -c 'import json,sys; print(json.dumps({"compose": sys.argv[1], "env": ""}))' "$NEW")")
if [ "$(code "$r")" = 202 ]; then pass "apply & restart accepted"; else fail "apply" "$(body "$r")"; fi
sleep 8
if wait_up 180; then
  sleep 25
  rm -f "$JAR"; r=$(api GET /api/hosts/local/stacks/moorage)
  pass "Moorage came back after Apply & restart"
else
  fail "Moorage didn't come back" "$(podman ps -a --format '{{.Names}} {{.Status}}' | head)"
fi
RES=$(podman run --rm -v moorage_selfdata:/d:ro docker.io/library/busybox:latest cat /d/self-stack-result.json 2>/dev/null)
if [ "$(jget ok <<<"$RES")" = "True" ]; then pass "helper result: $(jget message <<<"$RES")"; else fail "helper result" "$RES"; fi

# the CLI inside the container (docker exec wrapper of install-cli.sh)
OUT=$(podman exec moorage-moorage-1 moorage stacks ls 2>/dev/null | head -3)
[ -n "$OUT" ] && pass "CLI inside the container works ($(echo "$OUT" | head -1 | cut -c1-40))" || fail "CLI" "$OUT"

echo
if [ "$FAILS" = 0 ]; then echo "ALL PASS (self stack under $(podman --version))"; else echo "$FAILS step(s) failed"; exit 1; fi
