#!/bin/sh
# Install the `moorage` command on a Docker host, outside the Moorage container.
#
#   curl -fsSL https://raw.githubusercontent.com/Literally-davca/moorage/main/scripts/install-cli.sh | sh
#   sh install-cli.sh --native            # the binary from GitHub Releases instead
#   sh install-cli.sh --help
#
# Two modes:
#   docker (default)  /usr/local/bin/moorage runs `docker exec` into the Moorage container,
#                     so commands use that container's local socket as admin "cli:local".
#                     Works at once: no socket bind mount, no API token. Interactive (-it)
#                     when run from a terminal, plain stdin otherwise (pipes, cron).
#   native            the static binary from GitHub Releases (checked against SHA256SUMS).
#                     It talks to the hub over the socket in Moorage's data folder
#                     (bind-mount /data, then MOORAGE_SOCKET=<host data dir>/moorage.sock)
#                     or remotely with MOORAGE_URL + MOORAGE_TOKEN.
set -eu

REPO="${MOORAGE_REPO:-Literally-davca/moorage}"
MODE="docker"
PREFIX="/usr/local/bin"
CONTAINER="${MOORAGE_CONTAINER:-}"
VERSION="latest"

usage() {
	cat <<'EOF'
Install the moorage CLI on this host.

Usage: install-cli.sh [--docker | --native] [options]

  --docker              Wrapper that runs `docker exec` into the Moorage container (default)
  --native              Download the moorage binary from GitHub Releases
  --container NAME      Moorage container for --docker (default: detected, else "moorage")
  --version vX.Y.Z      Release for --native (default: latest)
  --prefix DIR          Where to install (default: /usr/local/bin)
  -h, --help            Show this help

Environment: MOORAGE_REPO (owner/name of the GitHub repository), MOORAGE_CONTAINER.
EOF
}

die() {
	echo "install-cli: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--docker) MODE="docker" ;;
	--native) MODE="native" ;;
	--container)
		[ $# -ge 2 ] || die "--container needs a name"
		CONTAINER="$2"
		shift
		;;
	--version)
		[ $# -ge 2 ] || die "--version needs a value, e.g. v1.4.0"
		VERSION="$2"
		shift
		;;
	--prefix)
		[ $# -ge 2 ] || die "--prefix needs a folder"
		PREFIX="$2"
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown option $1 (see --help)" ;;
	esac
	shift
done

# install_file SRC DEST: copy with mode 0755, through sudo when the folder isn't writable.
install_file() {
	src="$1"
	dest="$2"
	dir=$(dirname "$dest")
	if [ -w "$dir" ] || { [ ! -e "$dir" ] && mkdir -p "$dir" 2>/dev/null; }; then
		cp "$src" "$dest.tmp.$$" && chmod 0755 "$dest.tmp.$$" && mv -f "$dest.tmp.$$" "$dest"
	elif command -v sudo >/dev/null 2>&1; then
		echo "Writing $dest needs root, using sudo."
		sudo mkdir -p "$dir"
		sudo cp "$src" "$dest.tmp.$$" && sudo chmod 0755 "$dest.tmp.$$" && sudo mv -f "$dest.tmp.$$" "$dest"
	else
		die "$dir isn't writable; run as root or pick another folder with --prefix \$HOME/.local/bin"
	fi
}

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t moorage-cli)
trap 'rm -rf "$TMP"' EXIT INT TERM

# A previous native binary must not be silently replaced by a wrapper (or the other way
# round) without saying so.
if [ -e "$PREFIX/moorage" ]; then
	if grep -q "Moorage CLI wrapper" "$PREFIX/moorage" 2>/dev/null; then
		echo "Replacing the existing Moorage CLI wrapper in $PREFIX/moorage."
	else
		echo "Replacing the existing $PREFIX/moorage."
	fi
fi

if [ "$MODE" = "docker" ]; then
	command -v docker >/dev/null 2>&1 || die "docker isn't installed here; use --native with MOORAGE_URL and MOORAGE_TOKEN instead"
	if [ -z "$CONTAINER" ]; then
		# The Moorage server container: image named moorage, not one of its helpers
		# (updater, host shell), which carry the moorage.role label.
		CONTAINER=$(docker ps --filter "label=com.docker.compose.service=moorage" --format '{{.Names}}' 2>/dev/null | head -n 1 || true)
		if [ -z "$CONTAINER" ]; then
			CONTAINER=$(docker ps --format '{{.Names}} {{.Image}} {{.Label "moorage.role"}}' 2>/dev/null |
				awk '$2 ~ /moorage/ && $3 == "" { print $1; exit }' || true)
		fi
		[ -n "$CONTAINER" ] || CONTAINER="moorage"
	fi
	cat >"$TMP/moorage" <<EOF
#!/bin/sh
# Moorage CLI wrapper (scripts/install-cli.sh): runs moorage inside the Moorage container.
# Set MOORAGE_CONTAINER to use another container name.
C="\${MOORAGE_CONTAINER:-$CONTAINER}"
if [ -t 0 ] && [ -t 1 ]; then
	exec docker exec -it "\$C" moorage "\$@"
fi
exec docker exec -i "\$C" moorage "\$@"
EOF
	install_file "$TMP/moorage" "$PREFIX/moorage"
	echo "Installed $PREFIX/moorage (runs in container \"$CONTAINER\")."
	if ! docker exec "$CONTAINER" moorage version >/dev/null 2>&1; then
		echo "Note: \"docker exec $CONTAINER moorage version\" didn't answer. Is the container running, and is its image new enough to have /usr/local/bin/moorage?" >&2
		echo "      Otherwise run with --container NAME, or as a user that may use docker (or with sudo)." >&2
	fi
	echo "Try: moorage stacks ls"
	exit 0
fi

# --- native -----------------------------------------------------------------
[ "$(uname -s)" = "Linux" ] || die "release binaries exist for Linux only; on other systems use MOORAGE_URL + MOORAGE_TOKEN with a self-built binary"
case "$(uname -m)" in
x86_64 | amd64) ARCH="amd64" ;;
aarch64 | arm64) ARCH="arm64" ;;
armv7l | armv7 | armhf) ARCH="armv7" ;;
*) die "no release binary for $(uname -m)" ;;
esac
ASSET="moorage-linux-$ARCH"
if [ "$VERSION" = "latest" ]; then
	BASE="https://github.com/$REPO/releases/latest/download"
else
	BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is needed to download"
	fi
}

echo "Downloading $ASSET ($VERSION) from github.com/$REPO …"
fetch "$BASE/$ASSET" "$TMP/$ASSET" || die "download of $BASE/$ASSET failed"
fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" || die "download of SHA256SUMS failed"
WANT=$(awk -v f="$ASSET" '$2 == f || $2 == "*"f { print $1; exit }' "$TMP/SHA256SUMS")
[ -n "$WANT" ] || die "$ASSET is not listed in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
	GOT=$(sha256sum "$TMP/$ASSET" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	GOT=$(shasum -a 256 "$TMP/$ASSET" | awk '{ print $1 }')
else
	die "sha256sum or shasum is needed to check the download"
fi
[ "$GOT" = "$WANT" ] || die "checksum mismatch for $ASSET (want $WANT, got $GOT); nothing was installed"

install_file "$TMP/$ASSET" "$PREFIX/moorage"
echo "Installed $PREFIX/moorage ($("$PREFIX/moorage" version 2>/dev/null || echo "$VERSION"))."
cat <<'EOF'
Connect it to Moorage:
  - on this host, with Moorage's data folder bind-mounted (e.g. ./data:/data):
      export MOORAGE_SOCKET=/path/to/moorage/data/moorage.sock   # needs read/write on the socket (root)
  - or remotely, with an API token from Account → API tokens:
      export MOORAGE_URL=https://moorage.example.com MOORAGE_TOKEN=mrg_...
Then: moorage stacks ls
EOF
