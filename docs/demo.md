# Read-only demo mode

`MOORAGE_DEMO=true` turns Moorage into a public demo: visitors can look at every page without signing in, but nothing can be changed and no Docker host is touched.

## What the demo mode does

- **No Docker socket.** The local host is a built-in fake host (`demo-host`) with sample stacks (Nextcloud, Jellyfin, Vaultwarden, a degraded Immich and a stopped Home Assistant), their containers, compose files, logs, stats and history. Moorage never opens `/var/run/docker.sock`, even when it is mounted.
- **Every change is refused.** Any `POST`, `PUT`, `PATCH` or `DELETE` (API, local CLI socket, MCP, webhooks) answers `403` with *"This is a read-only demo, so changes are turned off."* before any handler runs. Sign-in, setup, terminals, imports and deploys are all writes, so none of them work.
- **No sign-in.** Every visitor is the `demo` user with the admin role, so all pages are visible (and all of them read-only). The account page and sign-out are hidden.
- **A banner** at the top of every page says that this is a read-only demo with sample data.
- **Sample data** in the database on the first start: a stack check per sample stack (they read the fake host, so the demo sends no probes anywhere), a paused HTTP check, a disabled ntfy channel pointing at `ntfy.invalid` and a published status page at `/status/demo`. The data is created only when the database has no checks, channels or status pages, so a restart or a real database is never changed.
- **Locked down by itself:** `MOORAGE_DEMO` also sets `MOORAGE_READ_ONLY`, turns off the agent port (`MOORAGE_AGENT_LISTEN`), terminals and self-update. It runs as hub / all-in-one only; `moorage agent` refuses to start with it.

## Running a demo instance

Use a separate data folder (or a throw-away volume) and don't mount the Docker socket:

```yaml
services:
  moorage-demo:
    image: ghcr.io/literally-davca/moorage:latest
    restart: unless-stopped
    ports:
      - "127.0.0.1:5090:5080" # put a reverse proxy with HTTPS in front
    environment:
      MOORAGE_DEMO: "true"
      MOORAGE_LISTEN: ":5080"
      MOORAGE_PUBLIC_URL: https://demo.example.com
    tmpfs:
      - /data              # fresh sample data on every restart
      - /opt/stacks
```

Or locally: `MOORAGE_DEMO=true MOORAGE_LISTEN=:5080 MOORAGE_DATA_DIR=/tmp/moorage-demo moorage`.

Notes for a public instance:

- The demo shows whatever the server's own configuration exposes to an admin: **Settings → General** lists `MOORAGE_*` variables (values of secret ones are never shown). Don't give the demo container any real credentials, registry logins or connections.
- Checks and notifications send nothing (the sample checks read the fake host, the sample channel is off). The hub's own background jobs, like the template catalog refresh, still run as on any instance.
- The demo is meant to show the UI. It isn't a sandbox to try deploys in; for that, run a normal instance.
