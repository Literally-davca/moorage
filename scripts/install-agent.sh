#!/bin/sh
# Install a Moorage agent on this host (M19F). The hub's Hosts → Add host builds the
# command; the join token goes in the environment, never as an argument (ps shows those):
#
#   curl -fsSL https://raw.githubusercontent.com/Literally-davca/moorage/<version>/scripts/install-agent.sh \
#     | MOORAGE_JOIN_TOKEN=… sh -s -- --hub hub.example.com:5081
#
# It checks Docker or Podman (and installs it only if you say yes, or with
# --install-runtime), writes compose.yaml into --dir and starts the agent.
set -eu

HUB=""
RUNTIME=""
STACKS="/opt/stacks"
NETWORK="bridge"
HOST_IP=""
READ_ONLY="false"
TS_SERVE="false"
TS_SOCKET="/var/run/tailscale/tailscaled.sock"
DIR="/opt/moorage-agent"
DATA=""
IMAGE="ghcr.io/literally-davca/moorage:latest"
REPLACE=0
DRY=0
TOKEN="${MOORAGE_JOIN_TOKEN:-}"

usage() {
	cat <<'EOF'
Install a Moorage agent and connect it to a hub.

Usage: MOORAGE_JOIN_TOKEN=<token> sh install-agent.sh --hub HOST:PORT [options]

  --hub HOST:PORT          The hub's agent address (MOORAGE_AGENT_LISTEN on the hub)
  --runtime R              docker | podman | podman-rootless (default: detected)
  --stacks DIR             Stacks folder (default: /opt/stacks)
  --network bridge|host    host lets the agent see the host's addresses and ports
  --host-ip ADDR           This host's address for service links (MOORAGE_HOST_IP)
  --read-only              Refuse every change on this host (MOORAGE_READ_ONLY)
  --tailscale-serve        Let hub admins publish services in the tailnet with Tailscale
                           Serve (mounts the host's tailscaled socket; never Funnel)
  --data DIR              Keep agent data in a folder instead of a named volume
  --image REF              Image (default: ghcr.io/literally-davca/moorage:latest)
  --dir DIR                Where compose.yaml goes (default: /opt/moorage-agent)
  --install-runtime R      Install docker or podman without asking (no terminal)
  --replace                Replace an existing agent (new token; its data stays)
  --dry-run                Print compose.yaml and the commands, change nothing
  -h, --help               Show this help

The token is valid for 24 hours and works once.
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
	--hub) HUB="${2:?--hub needs HOST:PORT}"; shift ;;
	--runtime) RUNTIME="${2:?--runtime needs a value}"; shift ;;
	--stacks) STACKS="${2:?--stacks needs a folder}"; shift ;;
	--network) NETWORK="${2:?--network needs bridge or host}"; shift ;;
	--host-ip) HOST_IP="${2:?--host-ip needs an address}"; shift ;;
	--read-only) READ_ONLY="true" ;;
	--tailscale-serve) TS_SERVE="true" ;;
	--data) DATA="${2:?--data needs a folder}"; shift ;;
	--image) IMAGE="${2:?--image needs a reference}"; shift ;;
	--dir) DIR="${2:?--dir needs a folder}"; shift ;;
	--install-runtime) RT_INSTALL="${2:?--install-runtime needs docker or podman}"; shift ;;
	--replace) REPLACE=1 ;;
	--dry-run) DRY=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; rt_die "unknown option $1" ;;
	esac
	shift
done

[ -n "$HUB" ] || rt_die "--hub HOST:PORT is required (the hub shows it in Hosts → Add host)"
case "$HUB" in *:*) ;; *) rt_die "--hub must be HOST:PORT, e.g. hub.example.com:5081" ;; esac
case "$STACKS" in /*) ;; *) rt_die "--stacks must be an absolute path" ;; esac
case "$NETWORK" in bridge | host) ;; *) rt_die "--network is bridge or host" ;; esac
[ "$DRY" -eq 1 ] || [ -n "$TOKEN" ] || [ "$REPLACE" -eq 0 ] || rt_die "--replace needs a new MOORAGE_JOIN_TOKEN"

if [ "$DRY" -eq 1 ]; then
	RT_RUNTIME="${RUNTIME:-docker}"
	case "$RT_RUNTIME" in
	podman) RT_SOCKET="/run/podman/podman.sock"; RT_COMPOSE="podman compose" ;;
	podman-rootless) RT_SOCKET="/run/user/$(id -u)/podman/podman.sock"; RT_COMPOSE="podman compose" ;;
	*) RT_SOCKET="/var/run/docker.sock"; RT_COMPOSE="docker compose" ;;
	esac
else
	rt_detect "$RUNTIME"
	if [ -f "$DIR/compose.yaml" ] && [ "$REPLACE" -eq 0 ]; then
		rt_die "an agent is already installed in $DIR – run again with --replace and a new token to reconnect it (its data stays)"
	fi
	if (docker ps --format '{{.Names}}' 2>/dev/null || podman ps --format '{{.Names}}' 2>/dev/null) | grep -qx 'moorage-agent' && [ "$REPLACE" -eq 0 ]; then
		rt_die "a container named moorage-agent already runs – use --replace"
	fi
	[ -n "$TOKEN" ] || rt_die "set MOORAGE_JOIN_TOKEN (Hosts → Add host on the hub creates it)"
fi

volume_line="moorage-agent:/data"
volumes_block="
volumes:
  moorage-agent:"
if [ -n "$DATA" ]; then
	volume_line="$DATA:/data"
	volumes_block=""
fi
net_line=""
[ "$NETWORK" = "host" ] && net_line="
    network_mode: host"
ip_line=""
[ -n "$HOST_IP" ] && ip_line="
      MOORAGE_HOST_IP: \"$HOST_IP\""
ts_volume=""
ts_env=""
if [ "$TS_SERVE" = "true" ]; then
	if [ "$DRY" -eq 0 ] && [ ! -S "$TS_SOCKET" ]; then
		rt_say "warning: --tailscale-serve:$TS_SOCKET not found – install Tailscale on this host (tailscale up) before using Serve"
	fi
	ts_volume="
      - $TS_SOCKET:$TS_SOCKET"
	ts_env="
      MOORAGE_TAILSCALE_SERVE: \"true\""
fi

compose="# Moorage agent – written by install-agent.sh. Change it and run: $RT_COMPOSE up -d
services:
  moorage-agent:
    image: $IMAGE
    container_name: moorage-agent
    command: agent
    restart: unless-stopped$net_line
    volumes:
      - $RT_SOCKET:/var/run/docker.sock
      - $volume_line
      - $STACKS:$STACKS$ts_volume
    environment:
      MOORAGE_HUB: \"$HUB\"
      MOORAGE_JOIN_TOKEN: \${MOORAGE_JOIN_TOKEN:-}
      MOORAGE_STACKS_DIR: \"$STACKS\"
      MOORAGE_READ_ONLY: \"$READ_ONLY\"$ip_line$ts_env
$volumes_block
"

if [ "$DRY" -eq 1 ]; then
	printf '# %s/compose.yaml\n%s\n# then: mkdir -p %s && cd %s && MOORAGE_JOIN_TOKEN=… %s up -d\n' "$DIR" "$compose" "$STACKS" "$DIR" "$RT_COMPOSE"
	exit 0
fi

rt_as_mk() { if [ -w "$(dirname "$1")" ] || [ -z "$RT_SUDO" ]; then mkdir -p "$1"; else rt_as_root mkdir -p "$1"; fi; }
rt_as_mk "$DIR"
rt_as_mk "$STACKS"
tmp="$(mktemp)"
printf '%s' "$compose" >"$tmp"
if [ -w "$DIR" ]; then mv "$tmp" "$DIR/compose.yaml"; else rt_as_root mv "$tmp" "$DIR/compose.yaml"; fi
# the token only for the first start, in .env readable by root only
umask 077
envtmp="$(mktemp)"
printf 'MOORAGE_JOIN_TOKEN=%s\n' "$TOKEN" >"$envtmp"
if [ -w "$DIR" ]; then mv "$envtmp" "$DIR/.env"; else rt_as_root mv "$envtmp" "$DIR/.env"; fi

cd "$DIR"
# shellcheck disable=SC2086
$RT_COMPOSE up -d
rt_say ""
rt_say "The agent is starting. It shows up in the hub under Hosts within a minute."
rt_say "Files: $DIR/compose.yaml (the token in $DIR/.env is only used for the first start)."
