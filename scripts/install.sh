#!/bin/sh
# Install the Moorage hub on this host (M19H):
#
#   curl -fsSL https://raw.githubusercontent.com/Literally-davca/moorage/<version>/scripts/install.sh | sh
#
# It checks Docker or Podman (installs it only if you say yes, or with
# --install-runtime), asks for the stacks folder, the port and the public URL, writes
# compose.yaml (the same file as in the repository, from the release tag) and starts
# Moorage. An existing installation is never overwritten: --upgrade only pulls the image
# and recreates the container with the compose file as it is.
set -eu

REPO="${MOORAGE_REPO:-Literally-davca/moorage}"
VERSION="${MOORAGE_VERSION:-main}"
DIR="/opt/stacks/moorage"
STACKS=""
LISTEN=""
PUBLIC_URL=""
RUNTIME=""
UPGRADE=0

usage() {
	cat <<'EOF'
Install the Moorage hub.

Usage: sh install.sh [options]

  --stacks DIR             Stacks folder (default: /opt/stacks)
  --listen ADDR            127.0.0.1:5080 (behind a reverse proxy, default) or 0.0.0.0:5080
  --public-url URL         The address people open, e.g. https://moorage.example.com
  --runtime R              docker | podman | podman-rootless (default: detected)
  --dir DIR                Where compose.yaml goes (default: /opt/stacks/moorage)
  --version TAG            Release tag of compose.yaml (default: the script's own)
  --install-runtime R      Install docker or podman without asking (no terminal)
  --yes                    Use defaults for every question about Moorage (never installs a runtime)
  --upgrade                Pull the new image and recreate Moorage; compose.yaml stays as it is
  -h, --help               Show this help
EOF
}

# --- BEGIN lib/runtime.sh ----------------------------------------------------------
# Shared by install.sh (hub) and install-agent.sh (M19F, M19H). Both run as
# `curl … | sh` without other files, so this block is copied into each of them; the
# check `scripts/dev/check-installers.sh` (run by check.sh) fails when a copy differs.
#
# It checks the container runtime (Docker or Podman, the compose plugin, a running
# daemon, the socket) and installs what is missing only with consent: a question on the
# terminal (/dev/tty) or --install-runtime docker|podman. Nothing else is installed.

RT_RUNTIME=""      # docker | podman | podman-rootless
RT_SOCKET=""       # the engine socket on this host
RT_COMPOSE=""      # the compose command
RT_SUDO=""         # "sudo" when needed and available
RT_INSTALL=""      # --install-runtime value
RT_YES=0           # --yes: install Moorage without questions (never the runtime)

rt_say() { printf '%s\n' "$*" >&2; }
rt_die() {
	printf 'Error: %s\n' "$*" >&2
	exit 1
}
rt_have() { command -v "$1" >/dev/null 2>&1; }

# rt_ask QUESTION – yes/no on the terminal; no terminal = no.
rt_ask() {
	if [ -r /dev/tty ] && [ -w /dev/tty ]; then
		printf '%s [y/N] ' "$1" >/dev/tty
		read -r rt_answer </dev/tty || rt_answer=""
		case "$rt_answer" in y | Y | yes | YES) return 0 ;; esac
	fi
	return 1
}

# rt_prompt QUESTION DEFAULT – a value from the terminal, or the default.
rt_prompt() {
	if [ "$RT_YES" -eq 0 ] && [ -r /dev/tty ] && [ -w /dev/tty ]; then
		printf '%s [%s] ' "$1" "$2" >/dev/tty
		read -r rt_value </dev/tty || rt_value=""
		[ -n "$rt_value" ] && {
			printf '%s' "$rt_value"
			return
		}
	fi
	printf '%s' "$2"
}

rt_os() {
	case "$(uname -s)" in
	Linux) ;;
	Darwin) rt_die "macOS: install Docker Desktop, then use the compose.yaml from the Moorage UI or README." ;;
	*) rt_die "This script runs on Linux. On Windows use Docker Desktop (or WSL) and the compose.yaml from the README." ;;
	esac
	if [ -r /etc/os-release ]; then
		# shellcheck disable=SC1091
		. /etc/os-release
		RT_DISTRO="${ID:-linux}"
		RT_DISTRO_LIKE="${ID_LIKE:-}"
	else
		RT_DISTRO="linux"
		RT_DISTRO_LIKE=""
	fi
}

rt_root() {
	if [ "$(id -u)" -eq 0 ]; then
		RT_SUDO=""
	elif rt_have sudo; then
		RT_SUDO="sudo"
	else
		RT_SUDO="none"
	fi
}

rt_as_root() {
	[ "$RT_SUDO" = "none" ] && rt_die "run this as root (or install sudo): it needs to $*"
	$RT_SUDO "$@"
}

rt_pkg_install() {
	if rt_have apt-get; then
		rt_as_root apt-get update -qq && rt_as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
	elif rt_have dnf; then
		rt_as_root dnf install -y "$@"
	elif rt_have apk; then
		rt_as_root apk add --no-cache "$@"
	elif rt_have pacman; then
		rt_as_root pacman -Sy --noconfirm "$@"
	elif rt_have zypper; then
		rt_as_root zypper --non-interactive install "$@"
	else
		rt_die "no known package manager: install $* by hand"
	fi
}

rt_install_docker() {
	case "$RT_DISTRO" in
	alpine) rt_pkg_install docker docker-cli-compose && rt_as_root rc-update add docker default && rt_as_root service docker start ;;
	arch | manjaro) rt_pkg_install docker docker-compose && rt_as_root systemctl enable --now docker ;;
	*)
		rt_have curl || rt_pkg_install curl
		rt_say "Installing Docker with the official script from get.docker.com…"
		curl -fsSL https://get.docker.com -o /tmp/get-docker.sh || rt_die "can't download get.docker.com"
		rt_as_root sh /tmp/get-docker.sh
		rm -f /tmp/get-docker.sh
		;;
	esac
}

rt_install_podman() {
	rt_pkg_install podman
	rt_have podman-compose || rt_have docker-compose || rt_pkg_install podman-compose || true
}

# rt_detect [preferred] – sets RT_RUNTIME, RT_SOCKET, RT_COMPOSE; installs with consent.
rt_detect() {
	want="${1:-}"
	rt_os
	rt_root
	if [ -z "$want" ]; then
		if rt_have docker && ! docker --version 2>/dev/null | grep -qi podman; then
			want="docker"
		elif rt_have podman; then
			want="podman"
		else
			want="docker"
		fi
	fi
	case "$want" in
	docker)
		if ! rt_have docker; then
			if [ "$RT_INSTALL" = "docker" ] || rt_ask "Docker isn't installed. Install Docker now?"; then
				rt_install_docker
			else
				rt_die "Docker isn't installed. Install it (https://docs.docker.com/engine/install/) or run again with --install-runtime docker."
			fi
		fi
		if ! docker info >/dev/null 2>&1 && ! $RT_SUDO docker info >/dev/null 2>&1; then
			if rt_have systemctl && rt_ask "The Docker daemon isn't running. Start it now (systemctl enable --now docker)?"; then
				rt_as_root systemctl enable --now docker
			else
				rt_die "the Docker daemon isn't running: systemctl enable --now docker"
			fi
		fi
		if docker compose version >/dev/null 2>&1 || $RT_SUDO docker compose version >/dev/null 2>&1; then
			RT_COMPOSE="docker compose"
		elif rt_have docker-compose; then
			RT_COMPOSE="docker-compose"
		else
			if rt_ask "The Docker compose plugin is missing. Install it now?"; then
				rt_pkg_install docker-compose-plugin || rt_pkg_install docker-compose
				RT_COMPOSE="docker compose"
			else
				rt_die "install the Docker compose plugin (docker-compose-plugin)"
			fi
		fi
		docker info >/dev/null 2>&1 || RT_COMPOSE="$RT_SUDO $RT_COMPOSE"
		RT_RUNTIME="docker"
		RT_SOCKET="/var/run/docker.sock"
		;;
	podman | podman-rootless)
		if ! rt_have podman; then
			if [ "$RT_INSTALL" = "podman" ] || rt_ask "Podman isn't installed. Install Podman now?"; then
				rt_install_podman
			else
				rt_die "Podman isn't installed. Install it or run again with --install-runtime podman."
			fi
		fi
		if [ "$want" = "podman-rootless" ] || [ "$(id -u)" -ne 0 ]; then
			RT_RUNTIME="podman-rootless"
			RT_SOCKET="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock"
			rt_have systemctl && systemctl --user enable --now podman.socket >/dev/null 2>&1 || true
			if rt_have loginctl && [ "$(loginctl show-user "$(id -un)" -p Linger 2>/dev/null)" != "Linger=yes" ]; then
				if rt_ask "Keep the agent running after you log out (loginctl enable-linger)?"; then
					loginctl enable-linger "$(id -un)" || rt_say "enable-linger failed: run it as root"
				fi
			fi
		else
			RT_RUNTIME="podman"
			RT_SOCKET="/run/podman/podman.sock"
			rt_have systemctl && rt_as_root systemctl enable --now podman.socket >/dev/null 2>&1 || true
		fi
		if [ ! -S "$RT_SOCKET" ]; then
			if [ "$RT_RUNTIME" = "podman-rootless" ]; then
				rt_die "the Podman socket $RT_SOCKET does not exist: systemctl --user enable --now podman.socket"
			fi
			rt_die "the Podman socket $RT_SOCKET does not exist: systemctl enable --now podman.socket"
		fi
		if podman compose version >/dev/null 2>&1; then
			RT_COMPOSE="podman compose"
		elif rt_have podman-compose; then
			RT_COMPOSE="podman-compose"
		elif rt_have docker-compose; then
			RT_COMPOSE="env DOCKER_HOST=unix://$RT_SOCKET docker-compose"
		else
			rt_die "no compose for Podman: install podman-compose or docker-compose"
		fi
		[ "$RT_RUNTIME" = "podman" ] && [ "$(id -u)" -ne 0 ] && RT_COMPOSE="$RT_SUDO $RT_COMPOSE"
		;;
	*) rt_die "unknown runtime $want (docker, podman or podman-rootless)" ;;
	esac
	rt_say "Runtime: $RT_RUNTIME, socket $RT_SOCKET, compose: $RT_COMPOSE"
}
# --- END lib/runtime.sh ------------------------------------------------------------

while [ $# -gt 0 ]; do
	case "$1" in
	--stacks) STACKS="${2:?--stacks needs a folder}"; shift ;;
	--listen) LISTEN="${2:?--listen needs an address}"; shift ;;
	--public-url) PUBLIC_URL="${2:?--public-url needs a URL}"; shift ;;
	--runtime) RUNTIME="${2:?--runtime needs a value}"; shift ;;
	--dir) DIR="${2:?--dir needs a folder}"; shift ;;
	--version) VERSION="${2:?--version needs a tag}"; shift ;;
	--install-runtime) RT_INSTALL="${2:?--install-runtime needs docker or podman}"; shift ;;
	--yes) RT_YES=1 ;;
	--upgrade) UPGRADE=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; rt_die "unknown option $1" ;;
	esac
	shift
done

rt_detect "$RUNTIME"

running=0
if (docker ps --format '{{.Names}}' 2>/dev/null || podman ps --format '{{.Names}}' 2>/dev/null) | grep -q '^moorage'; then
	running=1
fi
if [ -f "$DIR/compose.yaml" ] || [ "$running" -eq 1 ]; then
	if [ "$UPGRADE" -eq 1 ]; then
		[ -f "$DIR/compose.yaml" ] || rt_die "Moorage runs, but not from $DIR – pass --dir with its folder"
		cd "$DIR"
		# shellcheck disable=SC2086
		$RT_COMPOSE pull && $RT_COMPOSE up -d
		rt_say "Moorage was pulled and recreated with $DIR/compose.yaml as it is."
		exit 0
	fi
	rt_die "Moorage is already installed ($DIR or a running container). Nothing was changed. Use --upgrade to pull a new image."
fi
[ "$UPGRADE" -eq 0 ] || rt_die "Moorage isn't installed yet – run without --upgrade"

[ -n "$STACKS" ] || STACKS="$(rt_prompt 'Stacks folder' /opt/stacks)"
case "$STACKS" in /*) ;; *) rt_die "the stacks folder must be an absolute path" ;; esac
if [ -z "$LISTEN" ]; then
	if [ "$RT_YES" -eq 0 ] && rt_ask "Open Moorage on all addresses of this host (no reverse proxy)? Docker publishes past the host firewall."; then
		LISTEN="0.0.0.0:5080"
	else
		LISTEN="127.0.0.1:5080"
	fi
fi
case "$LISTEN" in *:*) ;; *) rt_die "--listen must be ADDRESS:PORT, e.g. 127.0.0.1:5080" ;; esac
[ -n "$PUBLIC_URL" ] || PUBLIC_URL="$(rt_prompt 'Public URL (empty = none yet)' '')"
case "$PUBLIC_URL" in "" | http://* | https://*) ;; *) rt_die "--public-url must start with http:// or https://" ;; esac

rt_have curl || rt_die "curl is needed to download compose.yaml"
url="https://raw.githubusercontent.com/$REPO/$VERSION/compose.yaml"
tmp="$(mktemp)"
curl -fsSL "$url" -o "$tmp" || rt_die "can't download $url"

port="${LISTEN##*:}"
# the published port, the stacks folder, the socket and the public URL
sed -i \
	-e "s#\"127.0.0.1:5080:5080\"#\"$LISTEN:$port\"#" \
	-e "s#^\(      - \)/opt/stacks:/opt/stacks#\1$STACKS:$STACKS#" \
	-e "s#^\(      MOORAGE_STACKS_DIR: \)/opt/stacks#\1$STACKS#" \
	-e "s#^\(      MOORAGE_LISTEN: \)\":5080\"#\1\":$port\"#" \
	-e "s#^\(      - \)/var/run/docker.sock:/var/run/docker.sock#\1$RT_SOCKET:/var/run/docker.sock#" \
	"$tmp"
if [ -n "$PUBLIC_URL" ]; then
	sed -i -e "s#^      \# MOORAGE_PUBLIC_URL: https://moorage.example.com#      MOORAGE_PUBLIC_URL: $PUBLIC_URL#" "$tmp"
fi

if [ -w "$(dirname "$DIR")" ] 2>/dev/null; then mkdir -p "$DIR" "$STACKS"; else rt_as_root mkdir -p "$DIR" "$STACKS"; fi
if [ -w "$DIR" ]; then mv "$tmp" "$DIR/compose.yaml"; else rt_as_root mv "$tmp" "$DIR/compose.yaml"; fi
cd "$DIR"
# shellcheck disable=SC2086
$RT_COMPOSE up -d

rt_say ""
rt_say "Moorage is starting. Open http://$( [ "${LISTEN%%:*}" = "0.0.0.0" ] && hostname -I 2>/dev/null | cut -d' ' -f1 || echo 127.0.0.1 ):$port${PUBLIC_URL:+ (or $PUBLIC_URL)}"
sleep 5
token="$($RT_COMPOSE logs 2>/dev/null | grep -o 'setup token[^ ]* [A-Za-z0-9_-]*' | tail -1 || true)"
[ -n "$token" ] && rt_say "First sign-in: $token" || rt_say "The setup token is in the log: cd $DIR && $RT_COMPOSE logs | grep -i setup"
