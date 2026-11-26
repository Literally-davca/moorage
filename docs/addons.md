# Add-ons & Integrations

Moorage separates lightweight built-in integrations from optional sidecar containers so deployments stay minimal until you turn features on.

## Built-in Add-ons

Manage in **Settings → Add-ons** (each card has an in-app **Guide** button):

| ID | Name | Purpose |
| --- | --- | --- |
| `coolify-integration` | Coolify integration | Decode Coolify stack UUIDs to names & operate stacks via Coolify REST API v1 ([coolify.md](./coolify.md)). |
| `proxy-labels` | Reverse proxy labels | Parse Traefik, Caddy & `moorage.url` labels into quick-open links. |
| `official-templates` | Official templates | Enable Moorage's curated Compose template catalog alongside local/custom sources. |
| `prometheus-grafana` | Prometheus / Grafana | Expose `/metrics`, scrape config generators, and 1-click Grafana dashboard push. |
| `stack-scheduler` | Stack Scheduler | Run cron tasks (`restart`, `pull-up`, `start`, `stop`, `exec`) on stacks across hosts. |
| `socket-proxy-check` | Socket Proxy Auditor | Scan Compose files for raw `/var/run/docker.sock` or `:2375` mounts and generate scoped proxy snippets. |
| `registry-webhooks` | Registry Webhooks | Trigger image update checks or applies from GHCR, Docker Hub, Gitea, or Harbor push webhooks. |
| `mcp-server` | MCP Server | Expose an ACL-scoped Streamable HTTP MCP endpoint (`/mcp`) with human approval drafts for AI assistants. |
| `status-snapshot` | Static status page snapshot | Publish public status pages as static HTML + JSON to S3 / R2, WebDAV or GitHub Pages so they survive an outage of this server. |

### Status snapshot

The `status-snapshot` add-on keeps a static copy of your published status pages outside this server:

- **Targets:** any S3-compatible bucket (AWS S3, Cloudflare R2, MinIO, Garage…), a WebDAV folder, or a GitHub Pages branch. Moorage never creates the bucket, folder or repository — create it yourself, then use **Test write access**.
- **When it publishes:** a few seconds after any check changes state, every *N* minutes (default 5; at least 15 for GitHub Pages because of its build limits), and on **Publish now**. Failures are retried with backoff.
- **What it writes:** `index.html` and `status.json` per page (`<slug>/`). The page shows when it was last updated and switches to *“data may be out of date”* by itself after the stale time (default 15 min), even when nothing can update it any more.
- **Secrets** (S3 secret key, WebDAV password, GitHub token) are stored like other integration secrets, never returned by the API and never logged. Leaving a secret field empty keeps the saved one.
- Changes, tests and manual publishes are in the audit log. The add-on is off in demo mode.

## Sidecar Add-ons & Custom Sources

- **Sidecars** (such as `restic-backup`, Dozzle, Beszel agent, etc.) deploy per host into the dedicated `add-ons` Compose stack.
- Click **Sources** to subscribe to custom HTTPS `index.json` add-on catalogs (with optional Private/LAN IP allow).
- Click **Custom add-on** to author a bespoke sidecar Compose + `.env` or convert a `docker run` command directly.

## Feature Gates & Manual Override

Gated screens prompt you to **Enable** (or **Install & enable** on a host) the matching add-on in 1 click, or choose **Manual override** (saved per user in Account preferences) if you manage equivalent tooling manually.
