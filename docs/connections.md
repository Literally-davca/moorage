# Connections & External Integrations

**Settings → Connections** centralizes credentials for external services paired with the Moorage hub. All secrets are stored write-only (`********` placeholder once saved), included in encrypted `.mbk` backups, and overridable via `MOORAGE_*` environment variables.

## 1. OIDC / Single Sign-On

Supports Authentik, Keycloak, Pocket ID, Zitadel, Dex, and any OpenID Connect provider with Discovery (`/.well-known/openid-configuration`).

- Copy the **Callback URI** displayed at the top of the OIDC card into your identity provider's allowed redirect URIs.
- Map IdP group names to **Admin groups** and **Viewer groups** (comma-separated).
- Set **Default role (outside groups)** to **Deny sign-in** for strict allowlisting.
- Click **Clear** next to **Client Secret** and save to remove a stored secret.

## 2. Coolify API

Powers the `coolify-integration` add-on. See [docs/coolify.md](./coolify.md).

## 3. Docker Registries

Add private registry credentials (`ghcr.io`, `docker.io`, Gitea, Harbor, Forgejo, etc.). Credentials are synced securely over mTLS to connected agents for:

- `docker compose pull` during deployments and image updates
- Registry v2 tag/digest checks and Compose Linter 1-click **Pin tag** fixes

Click **Test** on a row before saving to verify registry login.

## 4. Sister Panels (Grafana, Beszel & shortcuts)

Powers the `prometheus-grafana` add-on and Command Palette shortcuts:

- **Grafana URL + Service Account Token (`glsa_...`)**: enables 1-click **Push / Update in Grafana** from **Settings → Add-ons → Prometheus / Grafana → Generators**. Click **Clear token** and save to remove it.
- **Beszel URL**: adds quick links from host cards and `Ctrl+K`.
- **Custom links**: add arbitrary homelab tools (e.g. VictoriaLogs, Dozzle, Portainer) to the Command Palette (`Ctrl+K`).
