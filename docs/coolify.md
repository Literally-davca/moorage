# Coolify Integration

Moorage integrates with [Coolify](https://coolify.io) in two layers so Coolify-managed stacks have human-readable names everywhere and can be operated cleanly from Moorage.

## Layer 1 — Container labels (automatic)

When the `coolify-integration` add-on is enabled (on by default), Moorage inspects Docker container labels on every host:

- `coolify.name` / `coolify.applicationId` / `coolify.serviceId` / `coolify.projectName` / `coolify.environmentName`
- Helper build containers during Coolify deployments

Even without an API token, Moorage decodes Coolify UUID project names (e.g. `ok8cw0k...`) into their human-readable names across:

- Sidebar, Home overview, Stack detail headers & titles
- Command Palette (`Ctrl+K`)
- Dependencies graph & stack dependency pickers
- Image updates list & history
- Logs & Live terminal selector
- Uptime checks, Maintenance windows, Audit log, Scheduler tasks & MCP drafts

## Layer 2 — Coolify REST API v1 (optional)

Connecting Moorage to your Coolify API unlocks inside Stack Details:

- **Start / Stop / Restart / Redeploy** and **Force rebuild** buttons routed directly through Coolify (so Coolify remains the source of truth)
- **Variables & Compose** tab to inspect and update Coolify `.env` variables with optional immediate redeploy
- **Coolify Deployments** history and build logs
- **Direct "Open in Coolify"** header links and enriched project/environment names even for stopped stacks

### Setup steps

1. Open **Settings → Add-ons** and make sure **Coolify integration** is **Enabled**.
2. In your Coolify dashboard, open **Keys & Tokens → API tokens** and create a token with **`read`** and **`deploy`** permissions.
3. In Moorage open **Settings → Connections → Coolify API**.
4. Enter your Coolify URL (e.g. `https://coolify.example.com`) and paste the API token.
   - If Coolify runs on a LAN or Tailscale/NetBird IP (`100.x.y.z`, `10.x.x.x`, `192.168.x.x`), enable **Allow private / Tailscale IP addresses**.
   - If different hosts report to different Coolify instances, use **Per-host override**.
5. Click **Test connection**, then **Save Coolify**.

### Removing or replacing tokens

Because API tokens are write-only (`********`), click **Clear token** next to the default Bearer API Token or per-host override, then click **Save Coolify** to remove an existing token cleanly.

### Environment overrides

```env
MOORAGE_COOLIFY_URL=https://coolify.example.com
MOORAGE_COOLIFY_TOKEN=your-api-token
MOORAGE_COOLIFY_ALLOW_PRIVATE=true
```

When set via environment variables, the Connections UI marks those fields **Set via environment**.
