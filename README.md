# Moorage

Docker compose stacks, live logs and uptime checks in one small self-hosted app.
Plan and decisions: [PLAN.md](PLAN.md) · ideas: [ROADMAP.md](ROADMAP.md) · UI system: [.interface-design/system.md](.interface-design/system.md)

**Status: milestone M9 + roadmap features R1–R7** – one hub, any number of Docker hosts with agents, version history of stacks, uptime checks (also from several agents at once), log alerts, a coordinated hub watchdog, public status pages with RSS, image update checking with auto-update and rollback, an opt-in web terminal, an audit log of every change, planned maintenance windows, a per-stack timeline, a dependency graph with grouped cascade alerts, stack templates and Prometheus metrics. Podman support is in progress and not yet verified.

## What works

- First-run setup protected by a one-time **setup token** printed in the log, admin account with **argon2id** password and **mandatory TOTP**.
- Stack list from the stacks folder, plus compose projects running elsewhere shown **read-only**.
- Status per stack: Up, Degraded, Down (with exit code), Stopped, Not deployed.
- Compose and `.env` editor with live validation (compose-spec loader), atomic saves.
- Deploy, Stop, Restart, Update images, Pull, Down, Delete – run through the official **Docker Compose v5 Go SDK**, output streamed live.
- Live logs of all containers of a stack, regex search with highlighting, stderr marked, follow / jump to latest.
- CPU and memory per container.
- Convert `docker run …` into a compose service.
- **Version history** of every stack: each save, rollback, delete and each edit made outside Moorage becomes a version with author and diff. Roll back to any version in one click, optionally deploying it right away.
- **Several hosts:** run `moorage agent` on each Docker host; it connects out to the hub over mutual TLS, so hosts behind NAT work. Switch hosts in the sidebar, add and remove them on the Hosts page. An agent can run **read-only**.
- **Uptime checks:** HTTP(S) with status codes, keyword or JSON value; TCP port; ping; DNS; and stack checks that go down when a container exits, crash-loops or turns unhealthy. Checks run on the hub, so they keep working when an agent is offline.
- **Log alerts:** watch a stack (or one container) for a substring or regular expression and alert when it matches N times within a sliding window, then recover automatically after a quiet window. Agents filter logs locally and send only matching lines through the tunnel.
- **Alerts with context** to ntfy, Telegram, Discord, e-mail or a webhook: the failing check, matched log lines, exit codes and the stack's last 20 log lines, plus a link. Checks linked to a stack stay quiet for 3 minutes after a deploy, and alerts of one stack within 10 seconds arrive as one message.
- **Hub watchdog:** watching agents monitor the hub and coordinate over peer-to-peer mutual TLS so that when the hub goes down, **one** agent sends a single emergency notice (and a single recovery notice when the hub is back) directly through a configured notification channel.
- **Public status pages:** share selected checks and stacks without sign-in under `/status/<slug>` or on a custom domain, with 30-day daily bars, optional incident history, and an RSS 2.0 feed (`application/rss+xml`). Internal check names, URLs, hostnames, probe messages and logs never reach the public page.
- **Image updates:** check compose services for rebuilt image digests (same tag, new digest) and newer version tags. Per stack policy (`Off`, `Notify`, `Auto`), Moorage either notifies once per new digest/tag or automatically runs `pull + up` inside a configured apply window, silences checks for 3 minutes, records the old and new digests, and offers one-click **Roll back update** and one-click **Change to `<tag>`** (saved as a git version).
- **Web terminal:** open an interactive shell (`sh`, `bash`, or `ash` via xterm.js) inside any running stack container when enabled on that host (`MOORAGE_TERMINAL=true`), restricted to admins and recorded in a 1-year audit log.
- **Audit log:** every change made through Moorage (and every sign-in, failed sign-in and terminal session) with who, when, from which IP and whether it worked. Request contents (passwords, compose files, tokens) are never stored.
- **Maintenance windows:** plan one-off or weekly windows for everything, some stacks or some checks. Failures inside a window send no alert and show as Maintenance on status pages; a check still down when the window ends alerts once, right away.
- **Stack timeline:** the History tab merges versions, deploys and other events, incidents, image updates and (for admins) audit and terminal entries. Each incident has **Show what changed before** – the changes in the 2 hours before it.
- **Dependencies:** Moorage finds which stacks depend on which (shared external networks, host names in links and env) and you can add your own. When a database stack goes down, the apps that depend on it arrive in **one** alert naming the probable cause.
- **Checks from several places:** run an HTTP, TCP, ping or DNS check from the hub and chosen agents and count it as down only when most of them agree, so a blip in one network doesn't page you.
- **Users and single sign-on:** invite local users by a one-time link (you never see their password), roles `admin` / `viewer`, sign in with **OIDC** (Authentik, Keycloak, Zitadel…) or a trusted reverse-proxy header, and manage your own password, two-factor and sessions.
- **Update Moorage from the browser:** see which version the hub and every agent run and update them in one click; each node checks the release signature itself and rolls back on its own if the new version doesn't come up.
- **New stack from an image:** type an image name and Moorage offers a matching template, the compose file from the image's source repository, or one generated from the image's ports and volumes.
- **`.env` editor** with a Variables view (a key/value table that keeps comments and formatting, masks secrets and offers the variables the compose file still needs) and a Raw view.
- **Moorage's own stack is protected:** when Moorage runs from a compose stack, that stack can't be stopped, taken down or deleted from the UI; changes go through **Apply & restart**, which rolls back if Moorage doesn't come back.
- **Stack templates** and **Prometheus metrics**, see below.
- The check strip shows every result with deploys as anchors, so you see what changed before something broke.
- **CLI & interactive TUI (`moorage cli`):** manage stacks, compose files, rollbacks, deleted stack restores, templates, `docker run` conversion, uptime checks, maintenance windows, updates, dependencies, hosts and encrypted backups from any script (`--json`) or the `lazydocker`-style TUI — locally over a `0600` Unix socket (even in headless `MOORAGE_WEB_UI=false` mode without any open HTTP port) or remotely via `--url` and scoped API bearer tokens (`Account → API tokens`).
- Dark and light theme, works at phone width.

## Run with Docker

Use the example [compose.yaml](compose.yaml) (image `ghcr.io/literally-davca/moorage`, multi-arch: amd64, arm64, arm/v7):

```sh
docker compose up -d
docker compose logs moorage | grep setup_token   # first run only
```

To build from source instead: `docker build -t moorage:dev .` and set `image: moorage:dev`.

The CLI is in the image too: `docker compose exec moorage moorage stacks ls`. To run `moorage …` straight from the host's terminal, install the wrapper once (or `… | sh -s -- --native` for the release binary), see [docs/cli.md](docs/cli.md#running-moorage-on-the-docker-host-moorage-in-a-container):

```sh
curl -fsSL https://raw.githubusercontent.com/Literally-davca/moorage/main/scripts/install-cli.sh | sh
```

Open http://localhost:5080, enter the setup token, create the admin and scan the QR code with an authenticator app.

## Modes

| Command | Runs | Use for |
|---|---|---|
| `moorage` (`all-in-one`) | hub + local Docker (+ Web UI if `MOORAGE_LISTEN` set) | single server, or headless local socket daemon |
| `moorage hub` | hub only; every Docker host runs an agent | central hub outside managed hosts |
| `moorage agent` | this host's Docker, no UI | every other Docker host |
| `moorage cli` / `stacks` / … | interactive TUI & scriptable CLI commands | manage local (`moorage.sock`) or remote (`--url`) hub |

### Add a host

1. On the hub, open **Hosts → Add a host**, give it a name and create a join token.
2. Copy the generated `compose.yaml` to the new host and start it. The agent pairs once (token is single-use, valid 24 h) and keeps its own key in its data volume.
3. The hub's agent port (`5081`) must be reachable from the host directly, not through an HTTPS proxy.

### Hub watchdog

On **Hosts → Hub watchdog**, pick a notification channel and the agents that should watch the hub:
- **Quorum and single reporter:** the hub counts as down when a majority (`> N/2`) of watching agents lose their connection to it for longer than the grace period. With 1 watching agent it decides alone; with 2 both must lose the hub. Among the agents that see the hub down, only the alive agent with the lowest ID sends the alert (and the recovery notice when the hub is back). When only one agent loses the connection while the hub is healthy, the hub sends a normal host-offline alert instead.
- **Peer port:** with 1 watching agent no extra port is needed. With 2 or more watching agents, set `MOORAGE_WATCHDOG_LISTEN` (e.g. `:5082`) and `MOORAGE_WATCHDOG_ADDR` (`<agent-ip>:5082`) on each participating agent so they can poll each other's `/watchdog/status` over mutual TLS (TLS 1.3, verified against the hub CA and the current peer list).
- **Channel credentials:** the selected notification channel's secret is copied to the participating agents (stored in `<data>/agent/watchdog.json` with mode `0600`) so they can deliver the alert while the hub is unreachable. Non-participating agents do not receive it, and other channels never leave the hub.

### Status pages

Create one or more public status pages under **Status pages**:
- **What is public (and what never is):** visitors see only the page title, description, each service's public display name, status (`Operational`, `Down`, `Maintenance`, `Paused`, `No data`), 24 h / 30 d uptime %, 30 daily bars, and (when enabled) incident start/end times and duration. Internal check names, check types, URLs, IPs, ports, hostnames, stack names, container names, probe error messages, incident causes, log lines and deploy anchors are never exposed in the HTML, RSS feed or headers.
- **Paths and RSS feed:** every published page is served at `/status/<slug>` with its RSS 2.0 feed at `/status/<slug>/feed.xml` (`Content-Type: application/rss+xml; charset=utf-8`). If neither a custom domain nor `MOORAGE_PUBLIC_URL` is configured, RSS item links fall back to `http://<Host>/status/<slug>`.
- **Separate listener (`MOORAGE_STATUS_LISTEN`):** set `MOORAGE_STATUS_LISTEN=:5085` to expose a port that serves **only** status pages and their feeds, never the admin UI or `/api/*`, so you can keep `:5080` private on your LAN or VPN.
- **Custom domain:** when a page has a custom domain (e.g. `status.example.com`), requests with `Host: status.example.com` serve that page at `/` and its feed at `/feed.xml`; every other path on that host (including `/api/*` and the admin UI) returns 404. Point your reverse proxy at Moorage (either `:5080` or `MOORAGE_STATUS_LISTEN`):

  **Caddy (`Caddyfile`):**
  ```caddyfile
  status.example.com {
      reverse_proxy moorage:5080
  }
  ```

  **Traefik (labels on the `moorage` service):**
  ```yaml
  labels:
    - traefik.enable=true
    - traefik.http.routers.moorage-status.rule=Host(`status.example.com`)
    - traefik.http.routers.moorage-status.entrypoints=websecure
    - traefik.http.routers.moorage-status.tls.certresolver=letsencrypt
    - traefik.http.services.moorage-status.loadbalancer.server.port=5080
  ```

### Image updates

Configure image update checks under **Updates** and per-stack on each stack's overview tab:
- **Digest updates vs. version tags:** Moorage checks each service's current tag against the registry for a newer digest (e.g. `nginx:1.27` or `:latest` rebuilt) and also looks for newer numeric version tags with the same format and suffix (e.g. `v1.10` → `v1.11`, `16.4-alpine` → `16.5-alpine`). Pinned digests (`@sha256:…`), locally built services (`build:`) and local-only images are skipped.
- **Policies (`Off`, `Notify`, `Auto`):** new stacks default to **Notify**. When set to **Auto**, Moorage applies same-tag digest updates (`pull + up`) only inside the configured apply window (evaluated in the hub's time zone, `TZ`), silences the stack's checks for 3 minutes, and records the old and new image IDs and digests. It never touches a stack that is busy, stopped on purpose, outside the stacks folder or on a read-only agent, and **Check now** never applies anything. An update whose new image fails to start is not retried in the next window; it waits for you (Update now, Roll back, or a newer digest). Version tag changes are always notify-only; clicking **Change to `<tag>`** rewrites that service's `image:` line in `compose.yaml`, saves a git version, and updates the stack.
- **Rollback:** clicking **Roll back update** re-tags each service back to its previous local image ID (or pulls the old image by its recorded `@sha256:…` digest if it was pruned), recreates the containers without pulling, and switches the stack's policy back to **Notify** so the next window does not re-apply the broken digest.
- **Registry load:** one check run asks the registry once per image reference, even when 20 stacks use `postgres:16`; at most 4 requests run at a time per host, 20 s each. The default interval is 6 hours (1 hour – 7 days, ±10 % jitter). Anonymous Docker Hub checks count against its pull rate limit; a login in `auths` raises it.
- **Registry credentials:** agents read only the `auths` entries of `$DOCKER_CONFIG/config.json` (or `~/.docker/config.json`). Credential helpers (`credsStore`, `credHelpers`) are never executed, because Moorage runs no external programs; such registries show `credential helper not supported`. For a private registry, mount a config file with an `auths` entry into the agent (or all-in-one) container and point `DOCKER_CONFIG` at its folder:
  ```json
  { "auths": { "ghcr.io": { "auth": "<base64 of user:token>" } } }
  ```
  ```yaml
  environment:
    DOCKER_CONFIG: /docker-config
  volumes:
    - ./docker-config/config.json:/docker-config/config.json:ro
  ```
  Registries without an entry are checked anonymously, also when the file sets a global `credsStore`.

### Web terminal

Open an interactive shell in a running stack container directly from the stack's **Containers** tab:
- **Off by default (`MOORAGE_TERMINAL=true`):** the web terminal can only be enabled by setting `MOORAGE_TERMINAL=true` in the environment of the agent (or the `all-in-one` process for the local host). It can never be switched on from the web UI, and it is always disabled on read-only hosts (`MOORAGE_READ_ONLY=true` wins even if `MOORAGE_TERMINAL=true` is set). The agent itself enforces this check.
- **Who can use it:** only signed-in **admins**, and only on running containers that belong to a stack on a host where the terminal is enabled. Fixed shell choices (`sh`, `bash`, `ash`) run as the container's default user; arbitrary command strings are rejected. Access to a container terminal equals root-level control inside that container.
- **What is logged (and what never is):** every session is recorded at start and end in the 1-year audit log (visible on **Hosts → Terminal sessions** and in the stack's **History** Logbook) and in the server log, with the username, client IP, host, stack, container, shell, start time, duration, end reason (`exit`, `closed`, `idle`, `limit`, `revoked`, `error`, `hub-stopped`), and exit code. **Nothing typed or printed in the terminal is ever recorded**, because commands and output often contain passwords and secrets.
- **Limits:** at most 4 concurrent terminal sessions per hub, 15-minute idle timeout (measured on keyboard input, not output), 8-hour maximum session duration, and automatic termination within 30 seconds if the admin signs out, loses the admin role or the session expires.
- **One-time tickets and Origin check:** the browser first asks `POST /api/terminal/ticket` for a ticket (30 s, one use, bound to the user, the session and the container or host) and opens the WebSocket with it; the cookie alone never opens a terminal. The upgrade's `Origin` must match the request `Host`, the host of `MOORAGE_PUBLIC_URL`, or `X-Forwarded-Host` from a proxy in `MOORAGE_TRUSTED_PROXIES`. Behind a reverse proxy that rewrites `Host`, set `MOORAGE_PUBLIC_URL` to the address you open Moorage at; the panel names the reason when a terminal can't open.
- **Revoked sign-ins close live connections at once:** signing out, a password change, removing or disabling a user, a role change to viewer and revoking a session or an API token close the affected terminals and log streams immediately (WebSocket close code `4401`, "Session ended, sign in again"); the terminal output stays on screen.
- **Ending a session ends the shell:** closing the tab, **Disconnect**, a timeout, a stopped hub or agent all end the shell and everything started from it (HUP, then KILL a second later). Docker can't do this by itself: closing a terminal's connection leaves the shell running. Moorage finds the processes by a variable it sets in the shell (`MOORAGE_TERMINAL_SESSION`), so a process started with a cleared environment (`env -i …`), or any shell when the agent is killed with `SIGKILL`, can outlive the session.

### Users and sign-in

- **Users** (sidebar, admins): **Create invite link** gives a one-time link valid 48 h. The new user picks a password and adds an authenticator app; every local user needs two-factor sign-in. **Reset link** replaces the password and two-factor when the user finishes it; their sessions end then. Role changes, disabling and deleting end the user's sessions right away.
- There is always at least one active **local admin** with two-factor, so you can get in when the identity provider is down. Moorage refuses anything that would remove the last one, and admins can't demote, disable or delete themselves from the list.
- **Account** (click your name): change your password, add a new authenticator, see where you're signed in and sign out other sessions.
- **OIDC:** set `MOORAGE_PUBLIC_URL` and the `MOORAGE_OIDC_*` variables below. Register the redirect URI `<public URL>/api/auth/oidc/callback` at the identity provider; the Users page shows it with a copy button, plus the status of the connection. Roles come from groups at every sign-in (admin groups, then viewer groups, then `MOORAGE_OIDC_DEFAULT_ROLE`); a user in none of them is refused. An OIDC sign-in lasts 12 hours and signing out at the provider doesn't end it; disable the user in Moorage to cut access at once. Accounts are matched by issuer and subject, never by name or e-mail, so an SSO user can't take over a local account.
  - *Authentik:* OAuth2/OpenID provider with an RS256 signing key, issuer `https://<authentik>/application/o/<slug>/`, `MOORAGE_OIDC_SCOPES="openid profile email"` (groups come with `profile`).
  - *Keycloak:* client with standard flow and PKCE S256, issuer `https://<keycloak>/realms/<realm>`, a *Group Membership* mapper named `groups` (or `MOORAGE_OIDC_GROUPS_CLAIM=realm_access.roles`), `MOORAGE_OIDC_SCOPES="openid profile email"`.
- **Forward-auth** (Authelia, oauth2-proxy, Authentik outpost): set `MOORAGE_TRUSTED_PROXIES` to the proxy's address and `MOORAGE_PROXY_USER_HEADER` (e.g. `Remote-User`). The header counts only when the TCP connection comes from a trusted address; `X-Forwarded-For` is never used. Make sure Moorage's port can't be reached around the proxy.

### New stacks and `.env`

- **New stack → Start from an image:** enter e.g. `vaultwarden/server` or `ghcr.io/owner/app:1.2`. The hub reads the image's metadata from its registry (anonymously; private images aren't found, paste their compose instead) and offers:
  - a matching **template**;
  - the **compose file of the source repository** named in the image's `org.opencontainers.image.source` label (GitHub, GitLab.com and Codeberg only; a service that builds from source gets the image instead, and missing files are pointed out);
  - a **generated** compose file with the image's ports, volumes and settings.

  Nothing runs before you review it and press Create.
- **`.env` editor:** **Variables** shows a table; pasting several `KEY=value` lines into a name field adds them all. Values of keys that look secret (`PASSWORD`, `TOKEN`, `KEY`…) are hidden until you show them. Variables the compose file needs without a default are offered with one click, and unused ones are marked. **Raw** is the plain file. Comments and formatting of lines you don't touch stay as they are.

### Moorage's own stack

When Moorage itself runs from a compose stack it manages (for example `/opt/stacks/moorage`), that stack page has **Restart** and **Apply & restart** only:
- Stop, Down, Delete, Deploy and image auto-update are refused by the node itself, not just hidden.
- **Apply & restart** saves the files and starts a short-lived helper container that runs `docker compose up -d` for the stack from outside Moorage. If Moorage isn't back within 2 minutes, the helper puts the previous compose and `.env` back and runs `up` again. The stack page shows the outcome.
- Saving is refused when the change would cut Moorage off: its service, the Docker socket mount and the data mount must stay.
- To update Moorage itself, use **Moorage updates**.

### Moorage updates

**Hosts → Moorage updates** lists the hub and every agent with their version and how they run (container or binary). **Update all** updates the agents one by one and the hub last, stopping at the first failure.
- Releases are signed with an Ed25519 key. Every node checks the signature with the key built into its own binary before installing, so even a compromised hub can't install anything else.
- Container mode: a short-lived helper container from the new image recreates the Moorage container with the same settings. Binary mode: the binary is swapped in place and restarted.
- A node that doesn't come back healthy within 2 minutes rolls back by itself. The previous version is kept for 24 hours for **Roll back**.
- `MOORAGE_SELF_UPDATE=false` on a node refuses updates there. Only the hub asks GitHub for new releases (once a day or on **Check now**), sending nothing but its version in the User-Agent.
- If you run Moorage with Compose, set the new image tag in your compose file too; otherwise the next `docker compose up` goes back to the old tag.

### Audit log, maintenance and dependencies

- **Audit log** (sidebar, admins only): filter by who, stack, host and action group. The stack's History shows its own entries. Kept for 1 year. Refused requests (validation errors, a busy stack) are not recorded because nothing changed; server errors are recorded as Failed.
- **Maintenance** (sidebar): a window has a title, an optional public message, a scope (everything, stacks, checks) and a schedule (once, or weekly on chosen days in a time zone). Only a window over *everything* also pauses host-offline alerts and the hub watchdog. Uptime leaves maintenance time out.
- **Dependencies** (sidebar, per host): detected edges are dashed, your own are solid; ignore a wrong detected edge on the stack page. Detection reads compose files of managed stacks only; agents older than this version send none (your own edges still work). Grouped alerts keep one incident per check; a dependent that stays down after the root recovers alerts on its own.

### Stack templates

**Templates** (sidebar) creates a stack from a template: pick one, fill in a few values (passwords are generated), review the exact `compose.yaml` and `.env` (secrets masked until you reveal them), then create and optionally deploy. The stack's first version says which template it came from. Templates come from:
- 7 bundled examples (whoami, static site, PostgreSQL, Valkey, Adminer, Gitea, Caddy);
- a local folder, `MOORAGE_TEMPLATES_DIR` (default `<data dir>/templates`), one subfolder per template. You can `git clone` a template repository there yourself; Moorage runs no git;
- template indexes added by HTTPS URL under **Template sources** (admin). The hub fetches them with limits (HTTPS only, 1 MiB per file, no private addresses unless you allow the local network for that index) and keeps the last good copy when a refresh fails.

A template folder holds `template.yaml`, `compose.yaml` and an optional `.env`:

```yaml
id: myapp                # must match the folder name
name: My app
description: One line about it.
category: Web
variables:
  - name: DB_PASSWORD
    label: Database password
    secret: true
    generate: password   # password | hex32 | uuid
  - name: PORT
    default: "8080"
    pattern: "[0-9]{2,5}"
notes: Open http://this-host:${PORT}
```

Only declared `${NAME}` placeholders are replaced; anything else (`$$`, `${OTHER:-x}`) is left for compose. Put secrets in `.env` (`DB_PASSWORD=${DB_PASSWORD}`) and use them from compose. A template that asks for `privileged`, host networking, `pid: host` or the Docker socket gets a visible warning. A remote index is `{"format": 1, "templates": [{"id": "x", "files": {"template": "x/template.yaml", "compose": "x/compose.yaml", "env": "x/.env"}}]}` with paths relative to the index URL on the same host.

### Prometheus metrics

Set `MOORAGE_METRICS_TOKEN` (at least 16 characters, use a random one) on the hub or all-in-one to serve `GET /metrics` in the Prometheus text format. Requests need `Authorization: Bearer <token>`. Without the variable the endpoint answers 404, and it is never served on the status-page listener or status domains. Data from agents is refreshed in the background every 30 s while someone scrapes, so a scrape never waits for Docker.

```yaml
scrape_configs:
  - job_name: moorage
    scrape_interval: 30s
    scheme: https
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/moorage-token
    static_configs:
      - targets: ["moorage.example.com"]   # the main listener, not MOORAGE_STATUS_LISTEN
```

Alert on `moorage_check_state{state="down"} == 1` (not `moorage_check_up == 0`, which is also 0 for paused, pending and maintenance) and `moorage_host_up == 0`. A Grafana dashboard is in [`deploy/grafana/moorage-dashboard.json`](deploy/grafana/moorage-dashboard.json) (Grafana 10+, pick the data source when importing).

### Podman (in progress, unverified)

Moorage detects Podman's Docker-compatible API and adapts stats, logs, health and the terminal to it, but this has not been tested against a real Podman yet. To try it, point `DOCKER_HOST` at the Podman socket:
- rootful: `systemctl enable --now podman.socket`, `DOCKER_HOST=unix:///run/podman/podman.sock`;
- rootless: `systemctl --user enable --now podman.socket`, `loginctl enable-linger $USER`, `DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock`; leave `MOORAGE_PUID`/`MOORAGE_PGID` unset; ports below 1024 need `net.ipv4.ip_unprivileged_port_start`.

`scripts/dev/podman-smoke.sh` runs a throwaway instance against `DOCKER_HOST` and prints PASS/FAIL per feature. Please report what fails.

## Documentation

Detailed guides for built-in integrations and features live in `docs/`:

- [Coolify integration](docs/coolify.md) — Layer 1 container labels, Layer 2 REST API v1 setup, UUID decoding & token clearing
- [Connections & SSO](docs/connections.md) — OIDC/SSO groups, private container registries, Grafana & Beszel links
- [Backups & Restore](docs/backups.md) — encrypted `.mbk` exports, scheduled S3/WebDAV uploads, Restic volumes
- [Add-ons & Integrations](docs/addons.md) — built-in add-ons, sidecar catalogs, custom add-on authoring & overrides
- [Compose Editor & Experience Modes](docs/compose-editor.md) — Beginner/Advanced/Custom modes, lossless Form/Comments/Clean tabs, auto-indent & linter 1-click fixes
- [CLI, Headless Mode & Interactive TUI](docs/cli.md) — `MOORAGE_WEB_UI=false`, Unix socket (`moorage.sock`), API bearer tokens, full CLI command reference & `moorage cli` TUI

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `MOORAGE_LISTEN` | `:5080` in Docker; unset on bare binary | HTTP listen address (`off` / unset without `MOORAGE_WEB_UI=true` runs headless CLI-only mode) |
| `MOORAGE_WEB_UI` | `true` when `MOORAGE_LISTEN` set; else `false` | set `false` to disable HTTP/Web UI and serve local CLI via `MOORAGE_SOCKET` only |
| `MOORAGE_SOCKET` | `<MOORAGE_DATA_DIR>/moorage.sock` | Unix domain socket (`0600`) for local CLI & TUI on Linux/macOS (`off` disables) |
| `MOORAGE_URL` / `MOORAGE_TOKEN` | – | CLI defaults for remote hub HTTP(S) URL and Bearer API token (`mrg_...`) |
| `MOORAGE_DATA_DIR` | `/data` | SQLite database (accounts, sessions) and `history.git` (stack versions) |
| `MOORAGE_STACKS_DIR` | `/opt/stacks` | one folder per stack with `compose.yaml` |
| `MOORAGE_SECURE_COOKIES` | `false` | set `true` behind HTTPS |
| `MOORAGE_PUID` / `MOORAGE_PGID` | unset | owner of files written to the stacks folder |
| `DOCKER_HOST` | local socket | Docker daemon to manage |
| `MOORAGE_PUBLIC_URL` | – | e.g. `https://moorage.example.com`, used for links in alerts and status page feeds |
| `MOORAGE_STATUS_LISTEN` | – | hub: optional separate HTTP listen address serving only public status pages (e.g. `:5085`) |
| `MOORAGE_AGENT_LISTEN` | `:5081` | hub: TLS port agents connect to; `off` disables pairing |
| `MOORAGE_HUB` | – | agent: `host:port` of the hub's agent port |
| `MOORAGE_JOIN_TOKEN` | – | agent: one-time token from the hub, first start only |
| `MOORAGE_READ_ONLY` | `false` | refuse every change on this host (enforced by the agent itself) |
| `MOORAGE_METRICS_TOKEN` | unset | hub / all-in-one: bearer token that enables `GET /metrics` (Prometheus format); at least 16 characters |
| `MOORAGE_PROBES` | `true` | agent: run HTTP/TCP/ping/DNS checks for the hub from this agent's network (checks from multiple locations); also on read-only agents |
| `MOORAGE_TEMPLATES_DIR` | `<data dir>/templates` | hub / all-in-one: local folder with stack templates (one subfolder each); not created by Moorage |
| `MOORAGE_TERMINAL` | `false` | agent / all-in-one: allow admins to open an interactive container shell (`sh`, `bash`, `ash`) on this host; ignored when `MOORAGE_READ_ONLY=true` |
| `MOORAGE_WATCHDOG_LISTEN` | – | agent: peer mTLS listen address for the hub watchdog (e.g. `:5082`), needed when 2+ agents watch the hub |
| `MOORAGE_OIDC_ISSUER` | – | OIDC issuer URL (https; http only for localhost) |
| `MOORAGE_OIDC_CLIENT_ID` | – | OIDC client id |
| `MOORAGE_OIDC_CLIENT_SECRET` / `_FILE` | – | client secret, or a file holding it; empty = public client with PKCE |
| `MOORAGE_OIDC_SCOPES` | `openid profile email groups` | space-separated |
| `MOORAGE_OIDC_GROUPS_CLAIM` | `groups` | claim name or dotted path (`realm_access.roles`) |
| `MOORAGE_OIDC_ADMIN_GROUPS` / `_VIEWER_GROUPS` | – | comma lists |
| `MOORAGE_OIDC_DEFAULT_ROLE` | `none` | role for users in no listed group: `none` (refuse) or `viewer` |
| `MOORAGE_OIDC_LABEL` | `Sign in with SSO` | button text |
| `MOORAGE_OIDC_LOCAL_LOGIN` | `true` | `false` hides the password form (still at `/#/login?local=1`, and shown while SSO is broken) |
| `MOORAGE_TRUSTED_PROXIES` | – | forward-auth: proxy IPs/CIDRs (TCP peer); empty = off |
| `MOORAGE_PROXY_USER_HEADER` / `_GROUPS_HEADER` | – | forward-auth headers, e.g. `Remote-User`, `Remote-Groups` (comma-separated) |
| `MOORAGE_PROXY_ADMIN_GROUPS` / `_VIEWER_GROUPS` / `_DEFAULT_ROLE` | – / – / `none` | forward-auth role mapping |
| `MOORAGE_SELF_UPDATE` | `true` | every node: allow updates of Moorage itself from the UI |
| `MOORAGE_UPDATE_URL` | GitHub releases API of this repo | hub: where to look for releases (forks, mirrors) |
| `MOORAGE_UPDATE_CHANNEL` | `stable` | hub: `stable` or `pre` (include pre-releases) |
| `MOORAGE_WATCHDOG_ADDR` | – | agent: `host:port` other watching agents dial to reach this agent's watchdog listener |
| `GOMEMLIMIT` | `48MiB` | soft memory limit of the Go runtime; the default keeps the garbage from start-up from piling up (more only means more frequent collections) |

## Security notes

- **Checks from agents (`MOORAGE_PROBES`, on by default):** the hub can ask an agent to run an HTTP, TCP, ping or DNS check from the agent's network (max. 8 at once, 30 s each, the same private-address guards as on the hub). This gives the hub no new reach: it can already run any compose stack there. Read-only agents probe too, since a probe changes nothing on the host. Set `MOORAGE_PROBES=false` to refuse.

- Access to Moorage equals root on the host (it controls Docker). Keep it behind HTTPS and a VPN or SSO proxy.
- Every write needs the `X-Moorage` header (CSRF guard) and a `SameSite=Strict` session cookie; WebSockets reject cross-origin requests.
- Only the SHA-256 of session tokens is stored. Sign-in is rate limited per IP and per username; TOTP codes can't be reused.
- Stack history (`<data>/history.git`) contains `.env` files, so treat the data volume like a secret. It is never pushed anywhere.
- Agents and hub use mutual TLS with the hub's own CA (`<data>/ca`). The join token carries the CA fingerprint, so an agent never sends its token to the wrong hub. An agent's private key never leaves its host.
- An agent exposes only stack operations, never the Docker API. Removing a host on the hub disconnects it and its certificate stops working.
- The container web terminal is off by default (`MOORAGE_TERMINAL=false`), can only be switched on via the agent's or all-in-one environment (never from the UI), is always refused by read-only agents, runs only `/bin/sh`, `/bin/bash`, or `/bin/ash` inside a verified stack container, and records who opened it and when without ever logging typed commands or terminal output.
- Checks and notifications never connect to link-local or cloud metadata addresses (169.254.169.254 and friends), checked on the actual IP, so DNS rebinding doesn't get around it. A check can opt in. Notifications run no external programs and use fixed wording, no user templates. Channel secrets are never shown again after saving.
- Sign-in with OIDC uses the authorization code flow with PKCE, state and nonce; ID tokens are checked for signature (RS256/ES256/PS256 only), issuer, audience, expiry and nonce. Client secrets stay in the environment, never in the database or UI.
- Self-update installs only releases signed with the project's Ed25519 key, checked on the node that installs them; there is no downgrade except rollback to the kept previous version.
- No telemetry. Fonts and icons are bundled, the UI loads nothing from the internet.

## Develop

Requirements: Go 1.27+, Node 24+, a Docker daemon.

```sh
cd web && npm install && npm run build && cd ..   # builds the UI into internal/web/dist
go test ./...
MOORAGE_DATA_DIR=./.dev/data MOORAGE_STACKS_DIR=$PWD/.dev/stacks go run ./cmd/moorage
# UI with hot reload: (cd web && npm run dev) → http://localhost:5173, API proxied to :5080
```

Run the same full check as GitHub Actions CI (`svelte-check`, `vite build`, native `<select>` audit, dual-OS `go vet ./...`, all-package test compilation, `go test -count=1`, and static cross-platform builds for Linux & Windows; pass `-Quick` / `--quick` to skip cross-builds):

```sh
./scripts/dev/check.sh       # Linux / macOS
```

```bat
.\scripts\dev\check.cmd      # Windows (pass -AllTests to force Linux-centric suites too)
```

## Layout

```
cmd/moorage        entry point (modes: all-in-one, hub, agent)
internal/api       HTTP + WebSocket API, security headers, embedded UI
internal/host      Host interface: local engine, remote agent, agent API, registry
internal/tunnel    hub CA, join tokens, mTLS WebSocket + yamux between hub and agents
internal/monitor   checks, scheduler, incidents, deploy silence, alert batching and context
internal/updater   image update scheduler, apply-window decisions, notifications and rollback
internal/status    server-rendered public status pages, 60 s cache and RSS 2.0 feeds
internal/notify    webhook, ntfy, Telegram, Discord, SMTP
internal/netguard  blocks link-local and cloud metadata addresses for outgoing connections
internal/auth      accounts, argon2id, TOTP, sessions, rate limit
internal/engine    Docker + Compose SDK: stacks, ops, logs, stats
internal/convert   docker run → compose
internal/store     SQLite (pure Go)
web/               Svelte 5 UI
```
