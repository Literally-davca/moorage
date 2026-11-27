#!/usr/bin/env bash
# The installers run as `curl … | sh`, so scripts/lib/runtime.sh is copied into each of
# them (M19F, M19H). Without arguments this checks that every copy matches the library;
# --write puts the current library into them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LIB="$ROOT/scripts/lib/runtime.sh"
WRITE=0
[ "${1:-}" = "--write" ] && WRITE=1

status=0
for f in "$ROOT/scripts/install.sh" "$ROOT/scripts/install-agent.sh"; do
	tmp="$(mktemp)"
	# everything before the block (or the @@RUNTIME@@ marker), the library, everything after
	awk -v lib="$LIB" '
		BEGIN { skip = 0 }
		/^# @@RUNTIME@@$/ || /^# --- BEGIN lib\/runtime.sh/ {
			while ((getline line < lib) > 0) print line
			close(lib)
			if ($0 ~ /BEGIN/) skip = 1
			next
		}
		skip && /^# --- END lib\/runtime.sh/ { skip = 0; next }
		!skip { print }
	' "$f" | tr -d '\r' >"$tmp"
	if ! diff -q <(tr -d '\r' <"$f") "$tmp" >/dev/null; then
		if [ "$WRITE" -eq 1 ]; then
			cp "$tmp" "$f"
			echo "updated $(basename "$f")"
		else
			echo "$(basename "$f"): the copy of lib/runtime.sh is out of date – run scripts/dev/check-installers.sh --write" >&2
			status=1
		fi
	fi
	rm -f "$tmp"
	if command -v sh >/dev/null; then
		sh -n "$f" || status=1
	fi
done
exit "$status"
