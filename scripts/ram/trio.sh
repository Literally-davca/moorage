#!/bin/bash
# Run Dockge, Dozzle and Uptime Kuma side by side for a RAM comparison.
set -e
DEV=$HOME/moorage-dev
mkdir -p "$DEV/kuma" "$DEV/dockge"
docker rm -f cmp-dockge cmp-dozzle cmp-kuma >/dev/null 2>&1 || true
docker run -d --name cmp-dockge -p 127.0.0.1:5001:5001 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$DEV/dockge":/app/data -v "$DEV/stacks":"$DEV/stacks" \
  -e DOCKGE_STACKS_DIR="$DEV/stacks" louislam/dockge:1
docker run -d --name cmp-dozzle -p 127.0.0.1:8088:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock amir20/dozzle:latest
docker run -d --name cmp-kuma -p 127.0.0.1:3001:3001 \
  -v "$DEV/kuma":/app/data louislam/uptime-kuma:2
docker images --format '{{.Repository}}:{{.Tag}} {{.Size}}' | grep -E 'dockge|dozzle|uptime-kuma'
