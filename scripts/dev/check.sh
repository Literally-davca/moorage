#!/usr/bin/env bash
set -euo pipefail

CI_MODE=0
QUICK=0
for arg in "$@"; do
  case "$arg" in
    --ci) CI_MODE=1 ;;
    --quick) QUICK=1 ;;
    *)
      echo "Usage: $0 [--ci] [--quick]" >&2
      exit 2
      ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

echo "==> [1/5] Checking for native <select> / <datalist> tags"
if grep -rnE --include='*.svelte' '<(select|datalist)\b' web/src; then
  echo "Native <select> or <datalist> tags found; use Select.svelte or MultiSelect.svelte." >&2
  exit 1
fi
echo "  OK (0 matches)"

echo "==> [1/5] Checking that {@html} is used only in web/src/lib/Icon.svelte"
# Text from containers, logs and labels is always rendered as text; Icon.svelte renders
# its own static SVGs. Any other {@html} could turn container output into markup (XSS).
HTML_HITS="$(grep -rnF --include='*.svelte' --include='*.ts' --include='*.js' '{@html' web/src | grep -v '^web/src/lib/Icon\.svelte:' || true)"
if [ -n "$HTML_HITS" ]; then
  echo "$HTML_HITS" >&2
  echo "{@html} found outside web/src/lib/Icon.svelte; render text as text instead." >&2
  exit 1
fi
echo "  OK (Icon.svelte only)"

echo "==> [1/5] Checking that the installers carry the current scripts/lib/runtime.sh"
bash scripts/dev/check-installers.sh
echo "  OK"

echo "==> [2/5] Checking and building web UI"
(
  cd web
  if [ "$CI_MODE" -eq 1 ]; then
    npm ci --ignore-scripts --no-audit --no-fund
  fi
  npm run check
  npm run build
)

echo "==> [3/5] Running go vet ./... (Linux & Windows build tags)"
go vet ./...
GOOS=windows GOARCH=amd64 go vet ./...

echo "==> [4/5] Running go test -count=1 ./..."
go test -count=1 ./...

if [ "$QUICK" -eq 1 ]; then
  echo "==> [5/5] Cross-platform static builds skipped (--quick)"
  exit 0
fi

echo "==> [5/5] Building static binaries (linux/amd64, linux/arm64, linux/arm/v7, windows/amd64)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

LDFLAGS="-s -w -X main.version=dev"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP_DIR/moorage-linux-amd64" ./cmd/moorage
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP_DIR/moorage-linux-arm64" ./cmd/moorage
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP_DIR/moorage-linux-armv7" ./cmd/moorage
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP_DIR/moorage-windows-amd64.exe" ./cmd/moorage

echo "All checks passed."
