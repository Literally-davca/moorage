# Apply & restart of Moorage's own stack

When Moorage runs from a compose stack, **Apply & restart** on that stack hands `compose up` to a short-lived helper container from the same image. The helper waits for Moorage to come back and puts the previous `compose.yaml` / `.env` back if it doesn't.

## What the helper checks

On Linux Docker the helper runs in the host's network and checks Moorage the way users reach it, for 15 s in a row within 120 s:

- every published TCP port of the Moorage service (on `127.0.0.1`, or on the bound IP);
- with `network_mode: host`, the port Moorage listens on (`MOORAGE_LISTEN`, default 5080). `ports:` combined with `network_mode: host` is refused before saving, because Docker drops those ports;
- every network the old container shared with a reverse proxy (Traefik, Caddy, nginx-proxy, HAProxy…): the new container must be in it and answer on its IP there.

On Docker Desktop, rootless engines, or when host networking is refused, the helper keeps the older check (the container answers or writes a fresh health mark).

## After a rollback

- The restored files are written to the stack history as **Rolled back: <reason>**.
- Every admin sees a banner with the reason, **Show log**, **Open stack** and **Acknowledge**.
- The helper's output is kept in `<data>/self-stack-apply.log` (the last run, at most 256 KiB).

## Manual recovery

If even `compose up` with the previous files fails, Moorage may not come back. On the host, go to the stack folder (for example `/opt/stacks/moorage`) and run:

```sh
docker compose up -d
```

Check `<data>/self-stack-apply.log` and `<data>/self-stack-result.json` for what happened. The previous files of the last attempt are in `<data>/self-stack-prev/` until an apply succeeds.
