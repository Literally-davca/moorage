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

## Sidecar Add-ons & Custom Sources

- **Sidecars** (such as `restic-backup`, Dozzle, Beszel agent, etc.) deploy per host into the dedicated `add-ons` Compose stack.
- Click **Sources** to subscribe to custom HTTPS `index.json` add-on catalogs (with optional Private/LAN IP allow).
- Click **Custom add-on** to author a bespoke sidecar Compose + `.env` or convert a `docker run` command directly.

## Feature Gates & Manual Override

Gated screens prompt you to **Enable** (or **Install & enable** on a host) the matching add-on in 1 click, or choose **Manual override** (saved per user in Account preferences) if you manage equivalent tooling manually.
