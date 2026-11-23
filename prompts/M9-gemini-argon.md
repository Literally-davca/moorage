# Task: implement milestone M9 "Web terminal" in Moorage

You are a senior Go + Svelte engineer working directly in the repository `E:\Apps\moorage` (WSL path `/mnt/e/Apps/moorage`). You do the main implementation. Another engineer (Claude) will review your work afterwards, run it against a real Docker daemon, a hub and agents, and fix mistakes. Favour clear, conventional code over clever code, and report honestly what you did not finish or could not verify.

This milestone gives a browser a **root-equivalent shell inside containers**. It is the most dangerous feature Moorage has. Every bug here either:
- opens a shell for someone who must not have one;
- opens a shell on a host whose owner did not allow it;
- leaves a shell running after the user left;
- leaves no trace of who did it.

Section 6 lists the cases that must hold; design for them first.

## 1. Project context

Moorage is a self-hosted, MIT-licensed single Go binary that combines what Dockge (compose stacks), Dozzle (live logs) and Uptime Kuma (uptime checks) do. Milestones M1–M8 are done and reviewed. Read these before you start:
- `PLAN.md`:
  - "Rozhodnutí" → "Bezpečnost". Especially: **"[A] Web terminal/exec is off by default, switched on only by the agent's configuration (env/flag), never from the UI"**; the agent is a socket proxy with a fixed allowlist; read-only agents; roles `admin` / `viewer`.
  - "Stav milníků": M3 (hub/agent tunnel), M4 (stack events), M8 (latest review, incl. the bug list).
  - "Náklady" (RAM limit).
- `README.md` – modes, config table, security notes.
- `.interface-design/system.md` – UI direction.

Stack:
- Go 1.27, module `moorage`. Single binary with modes `all-in-one`, `hub`, `agent` (`cmd/moorage/main.go`).
- moby client `github.com/moby/moby/client` v0.6.3. It has:
  - `ExecCreate`, `ExecAttach`, `ExecResize`, `ExecInspect`;
  - check the exact option/result types with `go doc github.com/moby/moby/client.ExecCreateOptions` etc. before you write code.
- WebSockets: `github.com/coder/websocket` (already used). Hub ↔ agent: mutual TLS WebSocket + yamux; the hub talks HTTP to the agent over tunnel streams (`host.NewRemote(dial)` in `internal/host/agentapi.go`).
- SQLite via `modernc.org/sqlite`. Migrations are a list in `internal/store/store.go`; append, never edit old ones.
- UI: Svelte 5 (runes) + TypeScript in `web/`, embedded with `go:embed`.

Relevant code today:

| Path | What it is |
|---|---|
| `internal/api/ws.go` | `accept()` upgrades to WebSocket; coder/websocket rejects a cross-origin `Origin` (cross-site WebSocket hijacking, a bug Dozzle had). Read limit 4096. `wsLogs`, `wsOp`. |
| `internal/api/api.go` | routes; `s.user` / `s.admin` (`requireRole`: session cookie → `auth.SessionUser`); `secure()` sets CSP (`style-src 'self' 'unsafe-inline'`, `script-src 'self'`, `connect-src 'self'`) and the CSRF header check; `hostOf(w, r, write)`. |
| `internal/host/host.go` | `Host` interface – what the hub can do on a host (local engine or remote agent). Error codes for the wire. |
| `internal/host/agentapi.go` | the agent's HTTP API = the built-in socket proxy (fixed allowlist, `write()` wrapper refuses changes on read-only agents **on the agent**). `AgentInfo` (readOnly, version, watchdog). `Remote` = the hub's client; `errRouteNotFound` / `IsUnsupported` for older agents (404). |
| `internal/host/registry.go` (or nearby) | `host.Entry` per host: name, connected, readOnly… filled from `AgentInfo` when the agent connects. |
| `internal/engine/containers.go` | `Container` (ID, name, service, state, image…), `projectContainers` – containers of a compose project by label. |
| `internal/monitor/monitor.go` | `StackEvent(hostID, stack, kind, actor, detail)`; kinds in `DeployKinds` silence checks. Events show in the Logbook and on the strip. |
| `internal/auth/auth.go` | sessions (`SessionUser`, `Logout`), roles. |
| `web/src/views/StackDetail.svelte` | stack page; the Containers table; `HistoryView.svelte` = Logbook. |

## 2. Goal of M9

From `PLAN.md`: **exec into a container from the browser, only when the agent allows it; an audit log of who did it and when.**

## 3. Required design

### 3.1 Switching it on (decided, do not change)
- **Off by default.** One new env variable `MOORAGE_TERMINAL` (`true` / `false`, default `false`), read by:
  - the **agent** (for its host);
  - the **all-in-one** binary (for the local host).
  
  A `hub` without Docker ignores it.
- **The UI can never switch it on.** No setting, no API.
- **A read-only agent never allows it**, even with `MOORAGE_TERMINAL=true`. Read-only means "Moorage changes nothing here", and a shell changes anything. Log a warning at start when both are set.
- **The agent enforces it, not the hub.** The agent's exec endpoint refuses (403, code `terminal_disabled`) when the terminal is off. A hub that is compromised or buggy must not be able to open a shell on an agent that didn't allow it. The hub additionally hides the button.
- The agent reports the flag in `AgentInfo` (`terminal: true`). The hub keeps it in `host.Entry`.
- Older agents without the endpoint (404): the UI says "Update the agent to use the terminal".

### 3.2 Who may open a terminal
- **Admins only** (`s.admin`). Viewers never see the button and get 403.
- **Only containers of a stack**: the container must carry the compose project label of the stack named in the URL. Check this **on the agent** by resolving the container among `projectContainers(stack)`. Never accept a bare container ID from the browser without that check. This prevents a shell in arbitrary containers, e.g. Moorage's own container.
- The container must be **running**. Otherwise answer with a clear message.
- **Unmanaged stacks** (compose projects outside the stacks folder) are allowed: they are containers on a host whose owner switched the terminal on.

### 3.3 What runs
- **No arbitrary commands.** The browser picks a shell from a fixed list:
  - `sh` (default);
  - `bash`;
  - `ash`.

  The agent maps the name to `/bin/sh`, `/bin/bash`, `/bin/ash`. Anything else → 400.
- `ExecCreate` with `Tty: true`, `AttachStdin/Stdout/Stderr: true`, the container's default user, environment `TERM=xterm-256color`. No `Privileged`, no `User` override.
- If the shell doesn't exist in the image (distroless, scratch), show "This container has no /bin/bash. Try sh." (or "…has no shell"). Detect it by `ExecInspect` exit code / the exec error and show it in the terminal. Do not leave a blank screen.

### 3.4 Data path
```
browser (xterm.js) ⇄ WS /api/hosts/{host}/stacks/{name}/terminal?container=…&shell=…&cols=…&rows=…
                   ⇄ hub ⇄ (local engine | tunnel stream → agent WS /stacks/{name}/terminal …)
                   ⇄ docker exec (hijacked TTY stream)
```
- **Protocol, browser ⇄ hub and hub ⇄ agent (the same, so the hub only forwards):**
  - **binary frames** = raw terminal bytes, in both directions;
  - **text frames** = JSON control messages:
    - `{"type":"resize","cols":N,"rows":N}` browser → host; clamp to 1–500;
    - `{"type":"exit","code":N}` host → browser, when the shell ends;
    - `{"type":"error","message":"…"}` host → browser.
- The hub ⇄ agent leg is a WebSocket over a tunnel stream. coder/websocket's `Dial` accepts an `HTTPClient`; reuse the `Remote`'s tunnel transport. Do **not** add a second connection type to the tunnel.
- **Local host** (all-in-one): the hub calls the engine directly; no loopback WebSocket.
- Put the exec part behind a small interface on `Host` (e.g. `Terminal(ctx, stack, container, shell, cols, rows) (TerminalSession, error)` with `Read`/`Write`/`Resize`/`Close`/`Wait`). Then the API code is the same for local and remote, and tests can use a fake.
- **Limits:**
  - WebSocket read limit for this endpoint 64 KiB (a paste); the 4096 elsewhere stays;
  - **idle timeout 15 min** with no input from the browser → close with a message;
  - **hard limit 8 h** per session;
  - **at most 4 concurrent terminals per hub** → the 5th gets "Too many open terminals";
  - output is forwarded as it comes, with a bounded buffer. A slow browser must not make the hub buffer without limit; block the reader instead (backpressure).
- **Cleanup must be total.** When any side ends (browser tab closed, network drop, agent disconnect, hub shutdown, idle/hard timeout, shell exit), the others end too:
  - the exec's TTY stream is closed;
  - the shell process gets EOF/hangup. Docker has no "kill exec"; closing the hijacked connection sends SIGHUP to the shell. Verify that the process is gone (`ExecInspect` → `Running: false`) and describe it in the report.
  - No goroutine waits forever. Write the cleanup as one function every path calls.

### 3.5 Session revocation
- A terminal is a long-lived WebSocket opened with a session cookie. If that session ends while the terminal is open, the terminal must end within 60 s:
  - the user signs out;
  - the admin deletes the user or changes their role;
  - the session expires.
  
  Re-check `SessionUser` (and the admin role) every 30 s.
- A terminal must not keep a session alive. It does not extend the session's expiry.

### 3.6 Audit log
- **Hub table `terminal_sessions`:**
  - id;
  - user (username);
  - client IP. Use the same IP logic as the sign-in rate limiter; read how it treats `X-Forwarded-For` and do the same, do not invent a new rule;
  - host, stack, container name, service, shell;
  - started_at, ended_at;
  - end reason: `exit`, `closed`, `idle`, `limit`, `revoked`, `error`, `hub-stopped`;
  - exit code if known.
  
  Keep 1 year.
- **Never record what was typed or printed.** Commands contain passwords; output contains secrets. Write this decision into PLAN as **[A]** with the reason.
- The row is written **when the session starts** (so a crash still leaves a trace) and updated when it ends. On hub start, mark rows that have no end as ended with reason `hub-stopped`.
- **Stack event:** `StackEvent(host, stack, "terminal", user, "<service> · <shell>")` at start. `terminal` must **not** be a deploy kind: it must not silence checks or add a deploy anchor. It shows in the Logbook.
- **Server log line** (slog) at start and at end, with user, IP, host, stack, container, shell, duration, reason.
- **UI: a "Terminal sessions" list** (admin only): on the Logbook of the stack (as entries) and as a global list under **Hosts** or **Settings**; pick the place that fits the current navigation and say why. Show user, IP, container, shell, start, duration, end reason.

### 3.7 UI
- **Containers table:** a "Terminal" action per **running** container, only when:
  - the user is an admin;
  - the host reports `terminal: true`.

  When the host has the terminal off, show no button. Explain in the README how to switch it on.
- **Terminal view:** opens in the stack page as a panel, the way the op output panel does, or as a full-height view. It shows:
  - a toolbar with container · service · shell picker (`sh`, `bash`, `ash`; switching opens a new session);
  - a status (connecting / connected / ended + reason + exit code);
  - a **Disconnect** button;
  - xterm.js filling the area, fitted on resize (fit addon), dark theme from the design tokens. Mono font: the bundled one.
- **Leaving the view or closing the tab ends the session** (close the WebSocket in the component's cleanup).
- **Dependencies:**
  - add `@xterm/xterm` and `@xterm/addon-fit` (MIT). Nothing else;
  - **load them lazily** (`import()` when the terminal opens), so the main bundle doesn't grow. Report the main bundle size before and after.
- Works at phone width (375 px): the terminal fits and the toolbar wraps. A real phone keyboard is out of scope.
- Follow `.interface-design/system.md`. Max one primary button per view. Status always has text, not only colour.

Svelte 5 pitfalls already hit in this repo:
- an `$effect` reading props or app state refreshed by polling must use `$derived` keys or `untrack`, or it loops and floods requests. Here that would mean reconnecting the terminal every 5 s when the stack polls!
- an `$effect` must not return a Promise; wrap async code in a block.

### 3.8 Limits and out of scope
- RAM limit **100 MB PSS** for all-in-one with the UI open. Today the hub uses ~18 MiB anonymous / ~48 MiB RSS, with a 48 MiB soft Go memory limit (`GOMEMLIMIT` overrides it). An open terminal must cost little: no per-session buffers larger than 64 KiB.
- No new Go dependencies.
- Out of scope:
  - recording sessions;
  - file upload/download;
  - arbitrary commands;
  - a terminal on the host itself;
  - `docker attach` to the main process;
  - per-user permissions beyond admin/viewer;
  - OIDC.

## 4. Working rules
- Do **not** commit, do **not** run `git init`, do **not** push. The repo is not under git yet.
- Do **not** stop or restart the Moorage dev processes. Claude owns them:
  - the dev hub on `127.0.0.1:5080` (UI), `:5081` (agent port), `:5085` (status pages);
  - three test agents `wd1`–`wd3` (ports 5082–5084, data in `~/moorage-dev/wd1..3`);
  - a webhook receiver on `127.0.0.1:18099`.

  Never run `pkill moorage`.
- Do **not** exec into, restart or change the dev stacks `whoami` and `crashy` in `~/moorage-dev/stacks`, or the `EmptyPockets` containers.
- For your own manual runs:
  - use other ports (e.g. 5090/5091) and a separate data and stacks directory under `~/moorage-dev/m9-*`;
  - use a throwaway stack with three services: `busybox` (has `sh`, no `bash`), an image with `bash` (e.g. `debian:12-slim` running `sleep infinity`), and a distroless or `scratch`-based image (no shell);
  - remove them when done.
- Code style:
  - English comments explaining *why*;
  - small functions;
  - errors wrapped with context;
  - LF line endings;
  - `gofmt`.
- Do not refactor unrelated code.
- Go runs inside WSL, not on Windows. From Windows:
  - vet: `wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && go mod tidy && go vet ./...'`
  - tests: `wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && go test -count=1 ./...'` (no `-race`, CGO is off)
  - In Git Bash prefix with `MSYS_NO_PATHCONV=1`.
- UI: `cd E:\Apps\moorage\web; npm install; npm run check; npm run build`. svelte-check must report 0 errors.

## 5. Lessons from the M6–M8 reviews – avoid these
Each of your last milestones passed its own tests, but the review found 9–12 real bugs. They came from cases the tests didn't model:
- **state that ended in the UI but not on the server** (M8: rollback marked `rolled-back` before it finished);
- **long work tied to the HTTP request** (M8: "Update now" waited for the pull inside the request);
- **a check done only on the hub** where the agent must enforce it (M3 decision: read-only is enforced on the agent);
- **duplicates after a reconnect or restart** (M5, M6);
- **in-memory state lost on restart** (M5, M6);
- **an event recorded although the action never started** (M8: busy stack);
- **a setting with a surprising default** (M8: global `credsStore` broke public registries).

For M9 the analogues are:
- **a shell left running:** the browser closes, but the exec keeps running on the host; the agent disconnects and the hub keeps a goroutine; the idle timer is reset by output instead of input;
- **a shell for the wrong person:**
  - a viewer opens one;
  - a signed-out session keeps its terminal;
  - a cross-origin page opens the WebSocket;
- **a shell on the wrong host or container:**
  - the hub opens a terminal on an agent with the terminal off;
  - a read-only agent with `MOORAGE_TERMINAL=true` allows it;
  - a container ID from another stack, or Moorage's own container, is accepted;
- **no trace:**
  - the audit row is written only at the end and lost on a crash;
  - an open row after a hub restart stays "running" forever;
  - typed text ends up in a log;
- **the UI:** the terminal reconnects on every stack poll; leaving the page doesn't close it; the main bundle grows by xterm.js.

Write a test for each where it can be tested without a browser.

## 6. Tests you must add
Put the Docker exec behind a small interface so the session logic is testable without Docker.

1. **Switch:**
   - the agent with `MOORAGE_TERMINAL` unset or `false` refuses the endpoint with `terminal_disabled`;
   - a read-only agent with `MOORAGE_TERMINAL=true` refuses too;
   - `AgentInfo.terminal` reflects both.
2. **Access:**
   - a viewer gets 403;
   - a request with a foreign `Origin` is rejected;
   - a container that is not in the stack is refused **by the agent**;
   - a stopped container is refused;
   - an unknown shell name gets 400.
3. **Data path** (fake exec): bytes in both directions arrive unchanged (incl. binary/UTF-8); a resize reaches the exec; shell exit sends `{"type":"exit","code":N}` and closes.
4. **Cleanup** (fake exec with counters): each of these closes the exec exactly once and leaves no goroutine (count with `runtime.NumGoroutine` before/after, with a short settle time):
   - browser close;
   - agent/tunnel close;
   - idle timeout (use an injectable clock or short durations);
   - hard limit;
   - shell exit;
   - hub context cancel.
5. **Idle timer** is reset by input, not by output (a shell printing every second still times out).
6. **Concurrency limit:** the 5th terminal is refused; after one closes, a new one is allowed.
7. **Revocation:**
   - sign-out ends an open terminal within the re-check interval (short interval in the test), with reason `revoked`;
   - a role change to viewer does the same.
8. **Audit:**
   - the row exists while the session runs;
   - it gets `ended_at`, reason and exit code at the end;
   - rows without an end are closed with `hub-stopped` on start;
   - the stack event has kind `terminal`, and `terminal` is not in `DeployKinds`;
   - no typed bytes appear in the row, the event or the log output (capture the slog handler in the test).
9. **Older agent** (404) → the hub reports "Update the agent to use the terminal".

All existing tests must still pass.

## 7. Documentation
- `PLAN.md`:
  - add `### M9 – hotovo (<date>)` under "Stav milníků", in Czech, in the same style as M4–M8:
    - what works;
    - decisions marked **[A]** with reasons: off by default and on only via the agent's env; read-only wins; no arbitrary commands; nothing typed or printed is recorded; limits;
    - what was verified;
    - known limits.
  - M9 is the last numbered MVP milestone. Change "Další krok" to: the remaining MVP row "OIDC / forward-auth, role admin/viewer" and then release v1.0 – "to be decided with the user". Do not plan it further.
- `README.md`:
  - status line M9;
  - a feature bullet;
  - the `MOORAGE_TERMINAL` row in the config table;
  - a short "Web terminal" section:
    - how to switch it on (agent/all-in-one env);
    - who can use it;
    - what is logged and what never is;
    - limits;
    - "access equals root in that container".
  - Add a line to "Security notes".

## 8. Report back (your final message)
1. Files changed or added, one line each.
2. Design decisions you made that this prompt did not dictate, with reasons (incl. where the global terminal-session list lives).
3. Exact result summary of `go vet`, `go test ./...` and `npm run check` (pass/fail counts), plus the main JS bundle size before and after. Do not claim a pass you did not see.
4. What you did not finish or could not verify, and anything you suspect is wrong. List especially:
   - any case from section 6 that you could not cover;
   - whether you ran the manual scenario from section 4 against a real Docker daemon;
   - whether you confirmed that the shell process is gone after the browser closes.

## Appendix – current state (2026-11-22)
- M1–M8 are done and reviewed; see `PLAN.md` "M8 – hotovo" (incl. the review notes).
- RAM: hub ~18 MiB anonymous / ~48 MiB RSS with a 48 MiB soft memory limit; agents 15–20 MiB anonymous. The limit is 100 MB PSS.
- Docker 29.8 runs in WSL (containerd image store).
- The dev agents `wd1`–`wd3` run an older binary (before M8). Claude rebuilds them after your work; your code must handle older agents (404).
- Nothing is committed and the project is not a git repository yet.
