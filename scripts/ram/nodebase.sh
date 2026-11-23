#!/bin/bash
# PSS of an idle bare Node.js process (same Node as Uptime Kuma), and of an idle Node process with Express + socket.io loaded.
docker exec cmp-kuma node --version
docker exec -d cmp-kuma node -e "setInterval(()=>{},1000)"
docker exec -d -w /app cmp-kuma node -e "const e=require('express')();const s=require('http').createServer(e);require('socket.io')(s);s.listen(3999);setInterval(()=>{},1000)"
sleep 8
for p in $(pgrep -f 'setInterval'); do
  awk -v p=$p '/^Pss:/{s=$2} /^Anonymous:/{a=$2} END{printf "pid %s pss %.1f anon %.1f\n", p, s/1024, a/1024}' /proc/$p/smaps_rollup
  tr '\0' ' ' < /proc/$p/cmdline | cut -c1-60; echo
done
pkill -f 'setInterval'
