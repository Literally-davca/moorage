#!/bin/bash
# Sample RSS (MiB) of Dockge, Dozzle, Uptime Kuma (all processes in each container) and Moorage.
# Usage: mem.sh <samples> <interval-seconds>
N=${1:-6}; I=${2:-10}
rss_container() { docker top "$1" -eo pid,rss 2>/dev/null | awk 'NR>1{s+=$2} END{printf "%.1f", s/1024}'; }
rss_moorage() { ps -C moorage -o rss= | awk '{s+=$1} END{printf "%.1f", s/1024}'; }
printf "%-6s %8s %8s %8s %8s\n" t dockge dozzle kuma moorage
for i in $(seq 1 "$N"); do
  printf "%-6s %8s %8s %8s %8s\n" "$i" "$(rss_container cmp-dockge)" "$(rss_container cmp-dozzle)" "$(rss_container cmp-kuma)" "$(rss_moorage)"
  [ "$i" -lt "$N" ] && sleep "$I"
done
echo "--- processes"
for c in cmp-dockge cmp-dozzle cmp-kuma; do echo "$c:"; docker top "$c" -eo pid,rss,args | awk 'NR>1{a=$0; sub(/^ *[0-9]+ +[0-9]+ +/, "", a); printf "  %6.1f MiB  %s\n", $2/1024, substr(a,1,80)}'; done
echo "--- moorage"; ps -C moorage -o rss=,args= | awk '{printf "  %6.1f MiB\n", $1/1024}'
echo "--- docker stats (cgroup, incl. cache)"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' cmp-dockge cmp-dozzle cmp-kuma
