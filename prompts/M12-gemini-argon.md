# Task: implement milestone M12 "Update Moorage from the web UI" (hub and all agents)

You are a senior Go + Svelte engineer working directly in the repository `E:\Apps\moorage` (WSL path `/mnt/e/Apps/moorage`). You do the main implementation. Another engineer (Claude, the lead) will review your work afterwards, run it live against Docker, a hub and agents, and fix mistakes. Favour clear, conventional code over clever code. Report honestly what you did not finish or could not verify.

This milestone lets a browser **install new code on every machine Moorage runs on**. It is a remote-code-execution path by design. Every bug here either:
- installs code we did not sign;
- leaves a node down with no way back;
- updates a node whose owner switched it off;
- leaves no trace of who did it.

Design the verification and the rollback first.

## Project
Moorage is a self-hosted, MIT-licensed single Go binary that combines what Dockge (compose stacks), Dozzle (live logs) and Uptime Kuma (uptime checks) do. Milestones M1–M10 and roadmap features R1–R8 are done and reviewed. The project is now under git (public repo github.com/Literally-davca/moorage), but you still must not commit.

Read before you start:
- `PLAN.md`: "Rozhodnutí" (decisions, esp. "Bezpečnost"), "Stav milníků" M3–M9 incl. the review bug lists, "Náklady" (RAM).
- `ROADMAP.md`: your feature's row.
- `README.md`: modes, config table, security notes.
- `.interface-design/system.md`: UI direction (tokens in `web/src/app.css`).

Stack:
- Go 1.27, module `moorage`, modes `all-in-one` / `hub` / `agent` (`cmd/moorage/main.go`).
- moby client `github.com/moby/moby/client` v0.6.3 (check types with `go doc` before writing code).
- Hub ↔ agent: mTLS WebSocket + yamux. The hub calls the agent's HTTP API over tunnel streams (`internal/host/agentapi.go`: `AgentHandler` on the agent = built-in socket proxy with a fixed allowlist and a read-only `write()` wrapper; `Remote` = the hub's client; older agents answer 404 → `IsUnsupported`). `host.Host` interface in `internal/host/host.go` (Local and Remote implement it); wire error codes in `host.go`.
- SQLite via `modernc.org/sqlite`. Migrations are a list in `internal/store/store.go`: **append, never edit old ones**.
- Monitor: `internal/monitor/monitor.go` (checks, alerts, `StackEvent`, `DeployKinds` silence checks).
- API: `internal/api/api.go` (routes, `s.user` / `s.admin`, `hostOf`, `errStatus`), one file per area.
- UI: Svelte 5 (runes) + TypeScript in `web/`, embedded with `go:embed` from `internal/web/dist`.

## Commands (Windows host, Go runs in WSL)
- vet: `MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && gofmt -l ./internal ./cmd; go vet ./...'`
- tests: `MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && go test -count=1 ./...'` (no `-race`, CGO is off)
- If an inline `bash -lc '…'` breaks on quoting, write a small `.sh` file and run `wsl -d Ubuntu -- bash /mnt/…/file.sh`.
- UI (PowerShell): `cd E:\Apps\moorage\web; npm run check; npm run build`. svelte-check must report 0 errors.

## Working rules
- Do **not** commit, `git init` or push. The project is not under git.
- Do **not** stop, restart or rebuild the dev processes. The lead owns them:
  - the hub on 127.0.0.1:5080/5081/5085 (binary `~/moorage-dev/moorage`);
  - agents wd1–wd3 (`~/moorage-dev/wd1..3`);
  - a webhook receiver on 127.0.0.1:18099.

  Never run `pkill moorage` and never overwrite `~/moorage-dev/moorage`.
- Do not touch the dev stacks `whoami`, `crashy` or the `EmptyPockets` containers.
- For your own manual runs, use other ports (5090+) and separate data/stacks dirs under `~/moorage-dev/<feature>-*`. Build your own binary to `~/moorage-dev/<feature>-bin`. Remove your throwaway containers and stacks when done.
- Do **not** edit `PLAN.md`, `ROADMAP.md` or `README.md`; the lead writes the docs from your report.
- Code style:
  - English comments explaining *why*;
  - small functions;
  - errors wrapped with context;
  - LF line endings, `gofmt`.
- Don't refactor unrelated code. No new Go dependencies unless unavoidable; say why if you add one.
- RAM limit 100 MB PSS for all-in-one with the UI open (today ~18 MiB anon / ~48 MiB RSS). No unbounded in-memory growth.
- Security: the agent enforces its own rules (read-only, allowlist), not just the hub. Moorage runs no external programs. Viewers can read, only admins change things.

## Lessons from the M5–M9 reviews (each passed its own tests and still had 7–12 real bugs)
- State that ended in the UI but not on the server, or the reverse.
- Long work tied to the HTTP request instead of a background job.
- A check done only on the hub where the agent must enforce it.
- Duplicates after a reconnect or restart; in-memory state lost on restart.
- An event recorded although the action never started (busy, refused, failed validation).
- A setting with a surprising default.
- Older agents (404) must keep working.
- Svelte 5: an `$effect` reading props or polled state must use `$derived` primitive keys or `untrack`, or it loops (reconnects every 5 s poll). An `$effect` must not return a Promise.
- UI: max one primary button per view; status always has text, not only colour; works at 375 px without horizontal scroll.

Write a Go test for every case that can be tested without a browser. All existing tests must still pass.

## Final report (your last message)
1. Files changed or added, one line each.
2. Design decisions you made, with reasons.
3. Exact results of `go vet`, `go test ./...` and `npm run check` as you saw them. Do not claim a pass you did not see.
4. What you did not finish or could not verify, and anything you suspect is wrong.
5. How the lead can test it live (steps, URLs, env).

## What M10 (just finished) changed, relevant to you
- Users and roles:
  - `internal/auth` now has local, OIDC and proxy users;
  - `auth.User` has `source` and `displayName`;
  - `SessionUser` re-reads role/disabled on every request;
  - admin routes still use `s.admin(...)`.
- Audit: handlers can set the audit target themselves (see `internal/api/audit.go` and `internal/api/users.go` for examples). Every new mutating route must be in `auditActions`, or `TestAuditEveryMutatingRouteMapped` fails.
- `GET /api/session` also returns `login` info. The admin 403 message is "Only admins can do this."
- `internal/tunnel` has one flaky test (`TestWatchdogIntegrationThreeAgentsHubOutageAndRecovery`, TempDir cleanup race). Ignore it if it fails once and passes on rerun; say so.

## Goal
An admin sees on one page which version every Moorage part runs (hub/all-in-one + each agent) and which release is available. One click updates the hub, one agent, or "all agents". Today an update means SSH to every machine. Dockge, Dozzle and Kuma can't do this.

## Facts to start from (check them)
- Version comes from `-ldflags -X main.version` (`cmd/moorage/main.go`, `Dockerfile`). Agents report their version to the hub (`AgentInfo` / tunnel hello; check).
- The repo is public at `github.com/Literally-davca/moorage`. There are **no releases and no CI yet**.
- Deployments:
  - (a) **Docker container** from the scratch image (the main way; README); hub/all-in-one and agents alike;
  - (b) **plain binary** under systemd or by hand.

  Detect which one you are running as at runtime. In a container, the self-ID comes from `/proc/self/mountinfo` / cgroup / hostname, matched against the engine's container list. In binary mode, use `os.Executable()`.
- Hub ↔ agent: mTLS tunnel. The hub calls the agent's HTTP API (`internal/host/agentapi.go`). Older agents answer 404 → `IsUnsupported`.

## Required design
### 1. Releases (needed for anything to work)
- `.github/workflows/release.yml`, on a `v*` tag:
  - builds the UI, then static binaries for linux/amd64, linux/arm64 and linux/arm/v7 (`CGO_ENABLED=0`, version from the tag);
  - pushes a multi-arch image to `ghcr.io/literally-davca/moorage:<version>` and `:latest`;
  - creates a GitHub release with the binaries, `SHA256SUMS` and `SHA256SUMS.sig`.
- **Signature:** Ed25519 over `SHA256SUMS` (stdlib `crypto/ed25519`, no new deps).
  - The private key is a GitHub Actions secret `MOORAGE_RELEASE_KEY`; the public key is embedded in the binary (a Go constant file).
  - Write `scripts/release/genkey.go` (or a `moorage release-keygen` subcommand, your choice; justify) that prints a new keypair. **Do not generate the real key and do not touch GitHub secrets.** The lead/user does it. Use a test key in tests.
  - The image digest is published in the release notes and in a signed `images.json` (version → digest per arch), so container updates are verified too.
- No release exists yet. The code must handle "no releases / GitHub unreachable / rate limited" cleanly.
- Release source:
  - `MOORAGE_UPDATE_URL` (default the GitHub releases API of the repo), so forks and mirrors work;
  - `MOORAGE_UPDATE_CHANNEL` `stable` (default, no pre-releases) / `pre`.

### 2. Who checks and who downloads
- **Checking:** only the hub checks for new releases, daily plus on demand, with a cached result. Agents never phone home on their own. Respect the "no telemetry" decision: the request carries nothing but a User-Agent with the version; say so in the README notes.
- **Verification:** everything that installs something verifies it itself.
  - The hub fetches `SHA256SUMS` + `.sig` (+ `images.json`) and passes them to the agent together with the artifact (binary mode) or the image reference + digest (container mode).
  - The **agent verifies the signature with its own embedded key** and the checksum/digest before installing. The hub can't push an arbitrary binary or image: an attacker with the hub still can't install unsigned code on agents. Write this as an [A] decision.
- **Binary mode:**
  - the hub streams the verified binary to the agent over the tunnel, so agents without internet work. Size limit ~64 MiB; the agent writes to a temp file next to the executable and verifies before rename;
  - all-in-one/hub update themselves the same way from the download.
- **Container mode:**
  - the agent (or hub) pulls `image@digest` through its engine; registry access is from that host. If the host has no registry access, fail clearly; don't add an image push path;
  - the hub may offer "pull through hub" later (out of scope).
- **No downgrade** except to the previous version kept for rollback (see below).
- **Compatibility:** a release can declare `min_hub` / `min_agent` in `images.json` / release metadata. The UI refuses an agent update that would need a newer hub ("update the hub first") and shows when an agent is too old for the hub.

### 3. Applying the update and rollback
**Binary mode:**
- keep `moorage.prev` next to the executable;
- swap atomically (rename);
- re-exec: `syscall.Exec` of the new binary with the same args/env after a graceful shutdown of servers, the tunnel and terminals (use the existing shutdown paths). Under systemd with `Restart=always`, a clean exit is enough. Pick one and explain.

**Container mode** (the hard part):
- a running container can't replace itself, so start a **one-shot helper container** from the **new** image: `moorage self-update-helper --target <container id>`, with the engine socket mounted, label `moorage.role=updater`, auto-remove;
- the helper:
  1. inspects the target;
  2. stops it;
  3. renames it to `<name>-moorage-prev`;
  4. creates a new container with the **same config** (env, mounts, ports, networks with aliases, restart policy, labels, user, cap/security opts, healthcheck) and the new image;
  5. starts it;
  6. waits for health.

  List explicitly which container fields you copy and which you can't (e.g. anything set by compose that isn't in the inspect).
- **If the target is part of a compose project** (labels `com.docker.compose.*`), keep those labels, so compose and Moorage's own stack discovery still see it. Say what `docker compose up` does next time (it will recreate from the compose file's image tag; document that users should pin `:latest` or update their compose file, and show this hint in the UI).

**Health and rollback** (both modes):
- the new version must come back within 120 s: the agent reconnects to the hub with the new version, or the hub/all-in-one answers `/api/session` from the helper/re-exec watcher;
- otherwise roll back automatically (previous binary / previous container renamed back and started) and report "rolled back: <reason>";
- after success, delete `moorage.prev` / the `-moorage-prev` container only after 24 h or the next successful update, so a manual rollback stays possible. A "Roll back" button exists while it's kept.

**The hub updating itself:**
- the UI loses the connection;
- show a "Moorage is restarting, reconnecting…" overlay that polls `/api/session` and reloads when the version changes;
- an in-progress state survives the restart (store it in SQLite) so the result ("updated to X" / "rolled back") is shown after reconnect and audited.

### 4. Safety switches
- `MOORAGE_SELF_UPDATE` (default `true`) on every node; `false` refuses with a code, enforced by the agent/hub itself.
- A **read-only agent**: the agent updating itself doesn't touch user stacks, but it is code execution with socket access. Default: allowed (the binary is signed by us), but follow `MOORAGE_SELF_UPDATE`. Justify as [A].
- The helper container is the only new container Moorage creates outside user stacks. Mark it so stack discovery, R5 deps, stats and the log views ignore it.
- Admin only. R1 audit: `update.check`, `update.apply` (target, from → to), `update.rollback`, plus the result rows written by the background job (`update.done` / `update.failed` / `update.rolled_back`).
- One update at a time per node. "Update all agents" runs **sequentially** (or max 2 in parallel; decide), stops on the first failure, and reports per node.
- Mid-update agent disconnects are expected; don't record them as incidents. Silence the hub's own host-offline alerts for the updating node for up to 3 min (reuse the R2/deploy silence mechanics).

### 5. API + UI
**API:**
- `GET /api/system/updates`: per node — current version, mode (container/binary/unknown + reason), whether update is supported, latest available, compatibility, an in-progress job, the kept previous version;
- `POST /api/system/updates/check`;
- `POST /api/system/updates/apply` (targets: `hub` | agent ids | `all-agents`);
- `POST /api/system/updates/{node}/rollback`;
- progress over the existing op stream if it fits (op panel), otherwise polling.

**UI:**
- a "Moorage updates" section (Settings/Hosts; say where) with a table of nodes;
- a release-notes excerpt with a link;
- one primary button "Update all";
- per-row actions;
- state with text;
- a banner in the shell for admins when an update is available (dismissable per version);
- must work at 375 px.

Older agents (404 on the new endpoints) show "update manually: <command>" with the correct docker/binary command.

## Tests (at least)
- Signature/checksum verification:
  - good;
  - bad sig;
  - wrong key;
  - tampered binary;
  - an image digest not in the signed `images.json`.
- The agent refuses an unsigned/mismatched artifact even when the hub sends it (the hub-compromise case).
- Version compare and channel (pre-releases skipped on stable); no downgrade; `min_hub` gate.
- Binary mode, in a temp dir: swap, keep prev, rollback on health timeout (fake clock / injectable re-exec).
- Container mode with a fake engine: the helper copies the listed fields, keeps compose labels, rolls back on failed start/health, cleans the prev after success.
- `MOORAGE_SELF_UPDATE=false` refused; read-only agent allowed; viewer 403; audit rows; "update all" stops on first failure.
- The release check handles 404/no releases/rate limit/timeouts; the cached result is kept.

## Live verification you can do without a real release
Build two local versions (`-X main.version=0.0.1-test` and `0.0.2-test`), sign them with a **test** key (a build tag or env-only override of the embedded key, never active in release builds), and serve a fake release index from a local HTTP server on 5097. Exercise:
- binary mode with a throwaway agent on ports 5090+;
- container mode with a throwaway agent container built from the local Dockerfile (tag `moorage:update-test-*`).

Clean everything up. Don't touch the lead's dev hub/agents.

## Report
- [A] decisions.
- Env var table.
- Release/key setup steps for the user (genkey, `gh secret set`, first tag).
- The container-field copy list.
- What is unverified (e.g. arm builds, real GHCR).
- Binary size / RAM delta.

## Out of scope
- Auto-update without a click (a later option).
- Updating Docker/Podman itself.
- Windows/macOS binaries.
- A pull-through image cache in the hub.
