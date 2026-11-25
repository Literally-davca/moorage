# Moorage CLI, Headless Mode & Interactive TUI

The `moorage` binary provides both the server (`all-in-one`, `hub`, `agent`) and a full CLI & interactive terminal UI (`moorage cli`) backed by the same hub REST/WebSocket API as the web interface.

---

## 1. Headless CLI-Only Mode (`MOORAGE_WEB_UI=false`)

By default in bare binary deployments:

- If `MOORAGE_LISTEN` is **unset**, empty, or `"off"` (and `MOORAGE_WEB_UI` is not set to `true`), Moorage automatically starts with **`MOORAGE_WEB_UI=false`**: no HTTP port is opened, OIDC is disabled, and the hub listens only on a private local Unix domain socket.
- Setting `MOORAGE_WEB_UI=false` explicitly disables the web UI listener even if `MOORAGE_LISTEN` is configured.
- Setting `MOORAGE_LISTEN=:5080` (for example in the official `Dockerfile` image environment) keeps `MOORAGE_WEB_UI=true`, so existing Docker/Compose installs preserve `:5080` across upgrades.

### Local Unix Domain Socket

- Controlled by `MOORAGE_SOCKET` (default: `<MOORAGE_DATA_DIR>/moorage.sock`, permissions `0600`).
- Local CLI invocations (`moorage stacks ls`, `moorage cli`, etc.) automatically auto-discover `<MOORAGE_DATA_DIR>/moorage.sock` (default `/data/moorage.sock`) and authenticate locally as `cli:local` with `admin` role—no password or token required for local host root/service users with socket access.
- On **Windows**, Unix domain sockets are not used; connect over HTTP(S) using `--url` + `--token` (`MOORAGE_URL` + `MOORAGE_TOKEN`).

---

## 2. Remote Access & Bearer API Tokens

To run CLI/TUI commands against a remote Moorage hub (or from Windows), create an API token:

1. In the Web UI, open **Account → API tokens (CLI & remote access)** (admins only) or via local CLI run:
   ```sh
   moorage token create laptop-cli --role admin --scope admin --expires 90
   ```
2. Token secrets start with `mrg_` and are stored only as SHA-256 hashes in the `api_tokens` SQLite table. The plaintext secret is displayed **once** upon creation.
3. Scopes:
   - `read`: read-only GET endpoints and status/logs streams
   - `deploy`: stack lifecycle operations (`up`, `down`, `restart`, `stop`, `start`, `pull`, `update`, `rollback`, compose edits, check tests)
   - `admin`: full administrative access (templates, hosts, backups, maintenance, system updates, tokens)
   - `viewer` role tokens are automatically restricted to `read` scope.

### Connecting from a Remote Machine

```sh
export MOORAGE_URL="https://moorage.example.com"
export MOORAGE_TOKEN="mrg_..."
moorage stacks ls
moorage cli
```

Global flags override environment variables:

- `--url <url>` (`MOORAGE_URL`) — remote hub HTTP(S) base URL
- `--token <token>` (`MOORAGE_TOKEN`) — `Bearer` API token (`mrg_...`)
- `--socket <path>` (`MOORAGE_SOCKET`) — explicit Unix socket path
- `--data-dir <dir>` (`MOORAGE_DATA_DIR`) — data directory used to locate `moorage.sock`
- `-H, --host <host-id>` — target host ID (defaults to local/first connected host)
- `--json` — machine-readable JSON output
- `-q, --quiet` — minimal ID/name output
- `-y, --yes` — skip interactive confirmation prompts

---

## 3. Core Command Reference

### Stacks & Live Operations
- `moorage stacks list|ls` — list stacks with status and Coolify resolved display names
- `moorage stacks status <name>` — inspect stack containers, health, ports, Coolify metadata & URLs
- `moorage stacks up <name>` / `down` / `restart` / `stop` / `start` / `pull` / `update` — live-streamed Compose operations
- `moorage stacks delete <name>` (`rm`) — delete a managed stack
- `moorage stacks logs [stack]` — live or historical logs (`-f` follow, container/regex filters)
- `moorage stacks stats <stack>` — container CPU/memory/network stats

### Compose, Linter & Clean Mode
- `moorage compose get <stack>` — print current `compose.yaml` (and `.env` with `--env`)
- `moorage compose set <stack>` — upload updated `compose.yaml` / `.env` from file or stdin
- `moorage compose edit <stack>` — open `$EDITOR` on `compose.yaml` and upload changes cleanly
- `moorage compose validate <stack-or-file>` — validate via hub or offline (`--local`) using `internal/lint`
- `moorage compose lint <stack>` — inspect warnings/tips (`latest-tag`, `no-mem-limit`, `no-healthcheck`, missing vars)
- `moorage compose clean [file]` — strip blank lines/comments using Moorage Clean mode semantics (works offline)

### History, Diffs, Rollback & Deleted Stack Restore
- `moorage history list|ls <stack>` — list git versions of a stack
- `moorage history show <stack> <hash>` — inspect compose & env at a commit hash
- `moorage history diff <stack> <hashA> [hashB]` — unified diff between commits or current state
- `moorage rollback <stack> <hash>` — restore version and deploy
- `moorage restore list|ls` — list soft-deleted stacks still recoverable from git history
- `moorage restore stack <name> <hash>` — restore deleted stack files (and optionally deploy)

### Offline Docker Run Converter
- `moorage convert <docker run ...>` — converts `docker run` arguments into Compose YAML offline (no hub connection required)

### Templates, Monitoring, Updates, Dependencies, Hosts, Backups & System
- `moorage templates list|ls` · `inspect <source>/<id>` · `deploy <source>/<id>`
- `moorage checks list|ls` · `get <id>` · `test <id>` · `pause <id>` · `resume <id>` · `delete|rm <id>`
- `moorage maintenance list|ls` · `create` · `delete|rm <id>`
- `moorage updates list|ls` · `check [stack]` · `apply <stack>` · `rollback <stack>`
- `moorage deps list|ls` · `detect` · `add <from-stack> <to-stack>` · `remove <from-stack> <dep-id>`
- `moorage hosts list|ls` · `info` · `pair <name>` · `remove <host-id>` · `prune`
- `moorage backup export` · `preview <file.mbk>` · `restore <file.mbk>`
- `moorage system status` · `update` · `rollback <node>`
- `moorage token list|ls` · `create <name>` · `delete|rm|revoke <id>`
- `moorage audit`

---

## 4. Interactive TUI (`moorage cli` / `moorage tui`)

`moorage cli` launches a full-screen `lazydocker`-style terminal interface built directly on `github.com/moby/term` and `github.com/mattn/go-runewidth` with zero extra runtime dependencies:

- **Left pane:** stack list for the active host (showing Coolify decoded project/display names and live stack health badges)
- **Right pane tabs (`1`–`5`):**
  - `1`: **Containers** & resource summary
  - `2`: **Logs** (live streamed stack logs)
  - `3`: **Compose & Env**
  - `4`: **History** (with `Enter` confirmation prompt to roll back to the highlighted commit)
  - `5`: **Checks** (with `Space` toggle pause/resume)

### Keybindings

| Key | Action |
|---|---|
| `Tab` | Switch focus between stack list and main pane |
| `↑`/`↓`, `j`/`k` | Move cursor |
| `PgUp`/`PgDn` | Scroll right pane |
| `1`–`5` | Switch detail tab |
| `Enter` | Select stack / focus right pane (or trigger rollback on History tab) |
| `/` | Filter stacks (`Enter` confirm, `Esc` cancel) |
| `H` | Open host selector modal |
| `u` / `d` / `r` / `s` / `p` / `U` | Confirm & run `up` / `down` / `restart` / `stop` / `pull` / `update` |
| `Space` | Pause / resume selected check (on Checks tab) |
| `R` / `Ctrl+R` | Refresh stacks & details |
| `?` | Toggle help overlay |
| `q` | Quit |
