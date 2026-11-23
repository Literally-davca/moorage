# Common brief for roadmap feature agents (Moorage)

You implement one roadmap feature in Moorage. Another engineer (Claude, the lead) reviews your work afterwards, runs it live against Docker, a hub and agents, and fixes mistakes. Features are built one at a time by separate agents, so keep your change focused on your feature. Report honestly what you did not finish or could not verify.

## Project
Moorage is a self-hosted, MIT-licensed single Go binary that combines what Dockge (compose stacks), Dozzle (live logs) and Uptime Kuma (uptime checks) do. Milestones M1–M9 are done and reviewed.

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
