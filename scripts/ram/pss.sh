#!/bin/bash
# RSS, PSS and anonymous memory (MiB) per tool, from /proc/<pid>/smaps_rollup. Run as root.
sum() { # pids...
  local rss=0 pss=0 anon=0
  for p in "$@"; do
    f=/proc/$p/smaps_rollup; [ -r "$f" ] || continue
    read r s a < <(awk '/^Rss:/{r=$2} /^Pss:/{s=$2} /^Anonymous:/{a=$2} END{print r, s, a}' "$f")
    rss=$((rss+r)); pss=$((pss+s)); anon=$((anon+a))
  done
  awk -v r=$rss -v s=$pss -v a=$anon 'BEGIN{printf "%8.1f %8.1f %8.1f\n", r/1024, s/1024, a/1024}'
}
printf "%-10s %8s %8s %8s\n" tool rss pss anon
for c in cmp-dockge cmp-dozzle cmp-kuma; do
  printf "%-10s " "${c#cmp-}"; sum $(docker top "$c" -eo pid | tail -n +2)
done
printf "%-10s " moorage; sum $(pgrep -x moorage)
