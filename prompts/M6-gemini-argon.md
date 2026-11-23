# Task: implement milestone M6 "Hub watchdog" in Moorage

You are a senior Go + Svelte engineer working directly in the repository `E:\Apps\moorage` (WSL path `/mnt/e/Apps/moorage`). You do the main implementation. Another engineer (Claude) will review your work afterwards, run it against real processes and fix mistakes. Favour clear, conventional code over clever code, and report honestly what you did not finish or could not verify.

This milestone is a small **distributed system**: several processes on different machines must agree to send **exactly one** message. Most bugs will be in timing and in restarts, not in the happy path. Section 6 lists the scenarios that must hold; design for them first.

## 1. Project context

Moorage is a self-hosted, MIT-licensed single Go binary that combines what Dockge (compose stacks), Dozzle (live logs) and Uptime Kuma (uptime checks) do. Milestones M1–M5 are done and verified. Read these before you start:
- `PLAN.md`:
  - section **"Watchdog hubu – koordinace (M6)"** – the user's decisions; follow them;
  - sections "Rozhodnutí", "Stav milníků" M3 (hub/agent tunnel) and M4/M5 (monitor, notifications);
  - section "Náklady" (RAM limit).
- `README.md` – modes, config table, security notes.
- `.interface-design/system.md` – UI direction; follow it for any UI.

Stack:
- Go 1.27, module `moorage`. Single binary with modes `all-in-one`, `hub`, `agent` (`cmd/moorage/main.go`).
- SQLite via `modernc.org/sqlite` (migrations are a list in `internal/store/store.go`; append, never edit old ones).
- `coder/websocket` + `hashicorp/yamux`.
- UI: Svelte 5 (runes) + TypeScript in `web/`, embedded with `go:embed`.

How hub and agents talk today (M3):

| Path | What it is |
|---|---|
| `internal/tunnel/pki.go` | hub CA (ECDSA P-256, `<data>/ca`). `SignAgent` issues a **client-only** cert (`ExtKeyUsageClientAuth`, CN = agent ID `ag_…`). `ServerCertificate` = hub's server cert (`HubServerName`). Join tokens. |
| `internal/tunnel/hub.go` | hub's agent port (`MOORAGE_AGENT_LISTEN`, default `:5081`): `POST /enroll`, `GET /connect` (mTLS WebSocket → yamux; the hub is the yamux **client** and calls the agent's HTTP API over streams). `Reg.Attach`/`Detach` on connect/disconnect. |
| `internal/tunnel/agent.go` | `Agent.Run`: enrol once, then reconnect loop (backoff 1 s → 30 s); `session` serves `Handler` over the yamux session. Agent files live in `Agent.Dir` (key, cert, CA). |
| `internal/host/agentapi.go` | `AgentHandler(h, readOnly, version)` – the agent's HTTP API (also the built-in socket proxy). `Remote` = the hub's client of that API (`call`, `stream`). Errors cross as codes (`ErrorCode`/`FromCode`). |
| `internal/host/registry.go` | `Registry` of hosts; `Entry` is what the UI sees (`Connected`, `LastSeen`, `Version`…). |
| `internal/store/agents.go` | `agents` table: Agents, AgentByID, TouchAgent, RenameAgent, DeleteAgent. |
| `internal/notify/notify.go` | channels (webhook, ntfy, Telegram, Discord, SMTP): `Config`, `Validate`, `Send(ctx, kind, cfg, Message)`, secret masking. Outgoing connections go through `internal/netguard` (SSRF guard). |
| `internal/monitor/monitor.go` | checks, incidents, alert batching, `compose()` message wording, `TestChannel`. |
| `internal/api/api.go` | HTTP API (`s.user`/admin wrappers, `X-Moorage` CSRF header on writes). Hosts: `GET /api/hosts`, `PATCH/DELETE /api/hosts/{host}`, `/api/agents` for join tokens. |
| `web/src/views/HostsView.svelte` | Hosts page (status, version, add host, rename, remove). |
| `internal/config/config.go` | `MOORAGE_*` env variables. |

## 2. Goal of M6

From `PLAN.md`: when the hub is down, agents notice it and the user gets **one** emergency notice, and **one** "hub is back" notice, sent directly by an agent through a configured channel. 10 agents must not send 10 messages. A full mesh / consensus was considered and rejected; follow the "coordinated watchdog" design in PLAN.

## 3. Required design

### 3.1 Configuration (hub → agents)
- **Hub DB (new migration):**
  - a single watchdog settings row: enabled, one `channel_id` (an existing notification channel), `grace` in seconds (default 120, allowed 60–3600), `maintenance_until` (timestamp, nullable);
  - a per-agent flag `watchdog` (participates or not).
- **Hub API (admin only):**
  - `GET /api/watchdog` returns the settings and per agent: participates, advertised watchdog address, agent version, and "supported" (false for an agent that doesn't know the endpoints below);
  - `PUT /api/watchdog` saves them;
  - `POST /api/watchdog/maintenance` with minutes (0 = end) pauses the watchdog, e.g. for a planned hub upgrade;
  - `POST /api/watchdog/test` sends a test notice **through one participating agent**, proving the agent can reach the channel by itself.
- **Pushing to agents:** the hub pushes the config to every participating agent over the existing tunnel, as a new agent endpoint (e.g. `PUT /watchdog`). It pushes on every agent connect, after every settings change, and after an agent is added or removed. Non-participating agents get `enabled: false` and must delete the stored config.
- **What the pushed config contains:**
  - enabled, grace, maintenance-until;
  - hub display name and `MOORAGE_PUBLIC_URL`;
  - the channel kind + **full** config including its secret;
  - the list of participating peers: ID, name, watchdog address;
  - the agent's own peer certificate (3.3).
- **Agent side:**
  - store the config in `<Agent.Dir>/watchdog.json` with mode 0600, so it survives an agent restart while the hub is down;
  - keep the outage state (3.4) in the same file or next to it.
- **[security]** The selected channel's secret is copied to the participating agents. The UI must say so plainly next to the channel picker. Only the one selected channel is copied, never the others. A read-only agent (`MOORAGE_READ_ONLY`) may still participate: the watchdog only reads and sends a notice.

### 3.2 Agent-side configuration
- `MOORAGE_WATCHDOG_LISTEN` – address the agent's peer endpoint listens on, e.g. `:5082`. Empty = no peer listener.
- `MOORAGE_WATCHDOG_ADDR` – `host:port` other agents use to reach it (it can differ from the listen address behind NAT or port mapping). The agent reports it in its info to the hub (extend `AgentInfo`).
- Rule: with **one** participating agent, no listener is needed. With **two or more**, every participant needs an address. The UI disables the participation toggle with an explanation when this does not hold.
- Add both to `README.md` (config table, and a short "Hub watchdog" section) and to the agent `compose.yaml` that the Hosts page generates (commented port).

### 3.3 Peer channel (agent ↔ agent)
- Peers talk **mTLS over TLS 1.3**, trusting only the hub CA. Existing agent certs are client-only, so the hub issues a second certificate per participating agent:
  - the hub asks the agent for a CSR over the tunnel (e.g. `POST /watchdog/csr`); the agent generates and keeps a separate key `watchdog.key`, which never leaves the host;
  - the hub signs it with `ExtKeyUsageServerAuth` + `ExtKeyUsageClientAuth`, CN = agent ID, `DNSNames` = [agent ID], validity ~1 year, renewed on connect when under 30 days;
  - the hub sends the cert in the pushed config.
- A peer accepts a connection only if the client cert chains to the hub CA **and** its CN is in the current peer list. A client checks that the server cert's CN equals the expected peer ID (use `ServerName` = peer ID). A removed agent drops out of the list with the next push and is refused.
- One endpoint: `GET /watchdog/status` returns this agent's view as JSON:
  - `id`;
  - `hubReachable` (bool);
  - `hubDownSince` (time, if not reachable);
  - `reportedDown` (the time of the outage it already sent a notice for, if any);
  - `configVersion` (or the config hash), to detect peers with a stale config.
- Every participating agent polls each peer every 15 s with a 5 s timeout.
- Do not add any other peer endpoint. Peers exchange no data beyond this status.

### 3.4 Detection and the single reporter (deterministic, no elections)
- **"Hub reachable" for one agent:** its tunnel session is up. "Down since" = when the session was lost. A lost session counts as an outage only after `grace` without reconnecting (agent reconnect backoff is up to 30 s, and a normal Moorage restart takes seconds).
- **Quorum:** let `N` = number of participating agents in the config. The hub is **down** when more than `N/2` participants report `hubReachable = false` for longer than grace, counting itself, with fresh statuses (< 45 s old). Peers that don't answer do not count as a vote either way.
  - N = 1: the single agent decides alone.
  - N = 2: both must see the hub down.
- **Reporter:** among the participants that answered (plus self) and see the hub down, the one with the **lowest agent ID** sends the notice. Nobody else sends.
- **Exactly once per outage:**
  - The outage is identified by its start, the earliest `hubDownSince` among the down votes, rounded to the minute.
  - An agent that sent the notice stores `reportedDown` (persisted). It never sends again for the same outage, also not after its own restart.
  - Before sending, the reporter checks the peers' `reportedDown`. If another peer already reported this outage (e.g. a reporter with a lower ID came back late), it doesn't send.
- **Recovery notice:** when the hub is reachable again, the agent that reported the outage sends one "hub is back after X min" notice and clears `reportedDown`.
  - If that agent is unreachable at that moment, the lowest-ID alive agent that saw a peer's `reportedDown` for this outage sends it instead.
  - If no notice was sent for an outage (quorum never reached), no recovery notice is sent.
- **Maintenance:** while `maintenance_until` is in the future, no notices are sent and an outage that starts during maintenance is never reported.
- **Agent without hub ("agent X lost the connection"):** this is not a hub outage. The **hub** sends a normal alert when a participating agent has been disconnected longer than grace, and a recovery when it is back. Send it through the watchdog channel with the existing batching. Wording like `Host <name> is offline since 17:05` / `Host <name> is back after 12 min`.
- **Message wording** (fixed, no templates), e.g.:
  - Down: title `Moorage hub <name> is unreachable`, text `Since 17:05 (seen by 2 of 3 agents: vps, pi2). Stacks keep running. Alerts from Moorage are paused until the hub is back.`
  - Recovery: `Moorage hub <name> is back after 12 min.`
  - Include the public URL when it is set.
- Make all timings injectable (grace, poll interval, status freshness, clock) so tests run in milliseconds.

### 3.5 UI
On the Hosts page, add a card **"Hub watchdog"**:
- on/off switch;
- channel picker (only enabled channels), with the secret-copy note from 3.1;
- grace in minutes;
- a participation toggle per agent, showing its watchdog address or why it can't participate (no address, version too old, offline);
- **Send test via agent**;
- maintenance: **Pause for 1 h** / **End pause**, showing until when it is paused.

Explain the quorum in one or two sentences under the card, e.g. "The hub counts as down when most watching agents lose it. With 1 agent it decides alone, with 2 both must lose it." Follow `.interface-design/system.md`, works at phone width.

Svelte 5 pitfalls already hit in this repo:
- an `$effect` reading props or app state refreshed by polling must use `$derived` keys or `untrack`, or it loops and floods requests;
- an `$effect` must not return a Promise; wrap async code in a block.

### 3.6 Compatibility and limits
- An agent from M5 (no `/watchdog` endpoints) must keep working normally. The hub shows it as "update the agent to use the watchdog" and never pushes to it. Detect it via a 404 or the version.
- RAM limit for the all-in-one process is **100 MB PSS** with the UI open; today it uses ~49 MiB. The watchdog on an agent should cost goroutines and a few KB, nothing more.
- No new dependencies unless unavoidable. Explain any you add.
- Keep `netguard` for the notice, as `notify.Send` already does. Do not run external programs.

## 4. Working rules
- Do **not** commit, do **not** run `git init`, do **not** push. The repo is not under git yet.
- Do **not** stop or restart the Moorage dev server: it runs on `127.0.0.1:5080` (UI) and `:5081` (agent port) and is owned by Claude. For your own manual runs use other ports and a separate data directory under `~/moorage-dev/m6-*`.
- Code style:
  - English comments explaining *why*,
  - small functions,
  - errors wrapped with context,
  - LF line endings,
  - `gofmt`.
- Do not refactor unrelated code.
- Go runs inside WSL, not on Windows. From Windows:
  - vet: `wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && go mod tidy && go vet ./...'`
  - tests: `wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && go test -count=1 ./...'` (no `-race`, CGO is off)
  - In Git Bash prefix with `MSYS_NO_PATHCONV=1`.
- UI: `cd E:\Apps\moorage\web; npm run check; npm run build`. svelte-check must report 0 errors.

## 5. Lessons from the M5 review – avoid these
Your M5 code passed its own tests but had four real bugs, all found only against real processes. All four came from timing, restarts or state that the tests didn't model:
- duplicates after a reconnect, because it read again from the poll time instead of the last seen item;
- events lost between two polls (a container ran for less than one poll interval);
- a false "back up" right after a Moorage restart, because the in-memory state started empty;
- errors from the deploy-silence window alerting right after the window ended.

For M6 the analogues are:
- duplicate notices after an agent restart;
- a missed outage between two polls;
- a false "hub is back" after an agent restart;
- notices sent during maintenance.

Write a test for each.

## 6. Tests you must add
Write the coordination logic as a **pure function** (inputs: own status, peer statuses with ages, N, own ID, config, now; output: send down / send recovery / nothing). Table-test it, at least:
1. N=1, hub lost > grace → exactly one down notice; hub back → exactly one recovery.
2. N=3, all three lose the hub → only the lowest ID sends; the other two send nothing.
3. N=3, only one agent loses the hub (its own network) → no hub notice from agents; the hub's "host offline" alert covers it.
4. N=3, lowest-ID agent is itself down → the next lowest that sees the hub down sends.
5. Hub drops for less than grace (restart) → nothing.
6. Reporter restarts during the outage (persisted state reloaded) → no second down notice; recovery is still sent once.
7. Lowest-ID agent comes back late during an outage that another agent already reported → it does not send again (`reportedDown` from peers).
8. Maintenance active → nothing, also not after maintenance ends for an outage that started during it.
9. Stale peer status (> 45 s) is ignored.
10. N=2 with one agent unreachable → no quorum, no notice. Document this as intended.

Then an **integration test** in the style of `internal/tunnel/tunnel_test.go`, in-process:
- one hub and three agents, short timings, a fake notifier counting messages;
- pair the agents, push the config, stop the hub → exactly 1 down message;
- start the hub again on the same CA and data → exactly 1 recovery message.

Also test:
- the peer mTLS: a cert from another CA is refused, and a removed agent's cert is refused after a push;
- the config file is written with mode 0600 and deleted when participation is turned off.

All existing tests must still pass.

## 7. Documentation
- `PLAN.md`:
  - add `### M6 – hotovo (<date>)` under "Stav milníků", in Czech, in the same style as M4/M5: what works, decisions marked **[A]** with reasons, what was verified, known limits;
  - update the section "Watchdog hubu – koordinace (M6)" if your design differs anywhere, and say why;
  - change "Další krok" to M7.
- `README.md`:
  - status line M6;
  - a feature bullet;
  - the two new env variables;
  - a short "Hub watchdog" section: what it does, quorum, ports, secret copied to agents.

## 8. Report back (your final message)
1. Files changed or added, one line each.
2. Design decisions you made that this prompt did not dictate, with reasons.
3. Exact result summary of `go vet`, `go test ./...` and `npm run check` (pass/fail counts). Do not claim a pass you did not see.
4. What you did not finish or could not verify, and anything you suspect is wrong. List especially any scenario from section 6 that you could not cover.

## Appendix – current state (2026-11-20)
- M1–M5 are done. M5 (log alerts as check kind `log`, agent-side filtering via `GET /logs/watch`) was reviewed and fixed; see `PLAN.md` "M5 – hotovo".
- RAM: 48.5 MiB PSS with the UI open, 5 checks and 2 log rules. The limit is 100 MB.
- The dev server runs on `127.0.0.1:5080` / `:5081`. A local webhook receiver listens on `127.0.0.1:18099` and writes to `~/moorage-dev/hooks.jsonl`; the dev hub has a webhook channel pointing to it (channel id 1). You may use the receiver for manual tests, but do not change the dev hub's data.
- Docker 29.8 runs in WSL. Sample stacks are in `~/moorage-dev/stacks` (`whoami`, `crashy`). M6 doesn't need them.
- Nothing is committed and the project is not a git repository yet.
- Known stale bits in `PLAN.md` are out of scope:
  - header "návrh, čeká na schválení";
  - RAM estimate in "Technologie";
  - old RAM notes under M1–M3.
