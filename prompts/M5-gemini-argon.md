# Task: implement milestone M5 "Log alerts" in Moorage

You are a senior Go + Svelte engineer working directly in the repository `E:\Apps\moorage` (WSL path `/mnt/e/Apps/moorage`). You do the main implementation. Another engineer (Claude) will review your work afterwards and fix mistakes, so favour clear, conventional code over clever code, and report honestly what you did not finish or could not verify.

## 1. Project context

Moorage is a self-hosted, MIT-licensed single Go binary that combines what Dockge (compose stacks), Dozzle (live logs) and Uptime Kuma (uptime checks) do. Its central object is the **stack**: stack → containers → logs → checks → alerts. UI language is English; project docs (`PLAN.md`, `ROADMAP.md`) are Czech.

Milestones M1–M4 are done and verified. Read these before you start:
- `PLAN.md` – decisions, milestone status (sections "Stav milníků" M1–M4), RAM limit (section "Náklady").
- `README.md` – features, config, layout.
- `.interface-design/system.md` – UI design direction ("klidný přístav"). Follow it for any UI you add.

Stack:
- Go 1.27, module `moorage`. Docker Compose v5 Go SDK, moby client, SQLite via `modernc.org/sqlite` (no CGO), `coder/websocket`, go-git.
- UI: Svelte 5 SPA (runes) + TypeScript in `web/`, built by Vite and embedded with `go:embed`.

Code map (only what M5 touches):

| Path | What it is |
|---|---|
| `internal/engine/logs.go` | `StreamLogs` (follow), `TailLogs` (no follow), `containerLogs`, `lineEmitter`/`waitCloser`, `LogLine{Time,Container,Stream,Message}` |
| `internal/host/host.go` | `Host` interface: same calls for the local engine and a remote agent |
| `internal/host/local.go` | `Local` = local engine |
| `internal/host/agentapi.go` | `AgentHandler` (agent's HTTP API, served through the mTLS tunnel; it is the built-in socket proxy, read-only enforced on the agent) and `Remote` (hub's client of that API). Existing endpoints `GET /logs` (follow) and `GET /logs/tail` |
| `internal/host/registry.go` | `Registry`: hosts by ID, `LocalID = "local"` |
| `internal/monitor/probe.go` | check kinds (`Kinds` map, `KindHTTP`…`KindStack`), `Settings` (stored as JSON), `Outcome`, `Probe()`; `probeStack` shows how a stack-linked check talks to a host |
| `internal/monitor/monitor.go` | scheduler (`startRunner`/`run`, one goroutine per check, ticker = interval), state machine in `handle` (up → pending → down → up, retries, incidents), deploy silence (`StackEvent`, `silenced`, 3 min), alert batching per stack (10 s), `gatherContext` (exit codes + last 20 log lines), `compose()` builds the notification text, `Ticks`, `Anchors`, `prune` |
| `internal/monitor/monitor_test.go` | tests with fakes – extend in the same style |
| `internal/store/monitor.go` | checks, results, incidents, channels, events in SQLite; migrations are a list in `internal/store/store.go` |
| `internal/api/monitor.go` | `checkInput`, `validateCheck`, DTOs, handlers incl. `POST /api/checks/test` |
| `internal/api/ws.go` | WebSocket log streaming to the browser (uses `Host.StreamLogs`) |
| `web/src/lib/api.ts`, `web/src/lib/checks.ts` | API client and types (`Check`, `CheckKind`, `kindName`, `emptyCheck`, `toInput`) |
| `web/src/views/CheckForm.svelte` | check form: kinds as cards, stack linking, "Test now", channels |
| `web/src/views/CheckDetail.svelte` | check detail: uptime hero, strip with deploy anchors, incidents with containers + logs |

## 2. Goal of M5

From `PLAN.md`: *"pravidla regex/klíčové slovo na kontejner → alert; agent sleduje jen kontejnery s pravidlem"* – rules (regex or keyword) per container produce an alert; the agent only follows containers that have a rule.

Done means: a user can create a log rule for a stack (optionally one container), the rule goes **down** with an alert carrying the matching lines when the pattern occurs often enough, and goes **up** again after a quiet period – on the local host and on a remote agent alike.

## 3. Required design

Implement M5 as a **new check kind `log`** inside the existing monitor, so it reuses the state machine, strip, incidents, uptime, deploy silence, batching and channels. Do not build a parallel alert system.

### 3.1 Check definition
- `Kind = "log"`, `HostID` and `Stack` required. `Target` = container name, empty = all containers of the stack.
- New `Settings` fields (JSON, `omitempty`), e.g.:
  - `pattern` (string, required, max 256 chars),
  - `regex` (bool; false = plain substring),
  - `caseSensitive` (bool, default false),
  - `threshold` (int, 1–1000, default 1) – matches needed,
  - `window` (int seconds, 10 s–24 h, default 300) – sliding window for the threshold and the quiet period for recovery.
- Validate in `validateCheck`: the regex must compile with Go `regexp` (RE2 – linear time, so no ReDoS; still enforce the length limit).

### 3.2 Watching logs (event-driven, not polled)
- A log check runs a **watcher goroutine** for as long as the check is enabled. It is started and stopped together with the check's runner (reuse the cancel func in `startRunner`), so editing, disabling or deleting the check stops it.
- The watcher follows the logs through the `Host` interface and records matches (time + `LogLine`) in a bounded ring buffer (e.g. the last 100 matches; clip long lines).
- The existing ticker still calls `Probe` every `Interval`. For `log`, the probe only reads the watcher's buffer:
  - matches in the last `window` ≥ `threshold` → failed `Outcome`, message like `3 matches of "FATAL" in 5 min: <first line, clipped>`;
  - otherwise OK, message like `no matches in the last 5 min`.
  - Recovery therefore happens automatically once the window has no matches.
- Host offline or stack has no containers: behave like `probeStack` does today (look at it and mirror its choice between failing and `Paused`). A stopped stack must not raise a log alert.
- The watcher must survive container restarts and re-creates (new container IDs after Deploy/Update): when the stream ends or fails, reconnect with backoff (e.g. 2 s doubling to 60 s, reset after a healthy stream). Recreated containers must be picked up within about 30 s.
- **No replay and no duplicates:** never re-read history on (re)connect. Start following from "now" (Docker `Since`) or with tail 0. ⚠ Pitfall: `engine.itoa(n)` maps `n <= 0` to `"all"`, so passing `tail = 0` to the existing code streams the **entire** history. Do not rely on it.

### 3.3 Filtering on the agent
- Add a host-level call that streams only matching lines, e.g. `WatchLogs(ctx, stack, container string, f LogFilter, emit func(engine.LogLine)) error` on the `Host` interface, implemented by `Local` and by `Remote` (new agent endpoint such as `GET /logs/watch?stack=&container=&pattern=&regex=&cs=`). The agent compiles the pattern itself and sends only matching lines through the tunnel. Unmatched lines never leave the agent host.
- The hub must also apply the filter to what it receives, so an older agent that ignores the new parameters still produces correct results. Handle a 404 from an old agent by falling back to the existing `/logs` stream with hub-side filtering.
- Read-only agents must allow this endpoint (it only reads).
- Keep `ws.go` and the browser log viewer working unchanged.

### 3.4 Alert content
- Extend the alert context (`AlertContext` in `monitor.go`) with the matching lines (up to ~10, newest last). For `log` checks, show them in the notification built by `compose()` and in the incident in `CheckDetail.svelte`. Keep the existing "last 20 lines of the stack" context as well.
- Keep the notification wording fixed, as it is now. No user templates.
- Discord: matched log lines must stay inside the code block. Reuse the existing escaping. The existing tests for `@everyone` cover the pattern.

### 3.5 Test now
`POST /api/checks/test` for a `log` check runs the pattern over `TailLogs(stack, 200)`. It returns how many lines matched and the first few, so the user can tune the pattern without waiting. It must not start a watcher.

### 3.6 UI
- `CheckForm.svelte`: new kind card "Log". Fields:
  - host + stack (reuse the existing stack linking, required for this kind),
  - optional container,
  - pattern with toggles "Regular expression" and "Case sensitive",
  - "Alert when [N] matches within [M] minutes".
  - "Test now" shows the matches from 3.5.
  - Show the regex compile error inline.
- `checks.ts`: `kindName`, `emptyCheck` defaults, `toInput`.
- `CheckDetail.svelte`: show the pattern in the header and matched lines in incidents.
- Follow `.interface-design/system.md`. The UI must work at phone width.
- Svelte 5 pitfalls already hit in this repo:
  - an `$effect` that reads parent props or app state refreshed by polling must use `$derived` keys or `untrack`, or it loops and floods requests;
  - an `$effect` must not return a Promise; wrap async code in a block.

### 3.7 Limits
- RAM limit for the whole all-in-one process is **100 MB PSS** in steady state with the UI open. Today it uses 39–45 MiB. One watcher should cost a goroutine and a small buffer, not a copy of the logs.
- Never store logs beyond the ring buffer and the incident context (PLAN: "Logy se neukládají").
- No new dependencies unless unavoidable. If you add one, say why in the report.

## 4. Working rules
- Do **not** commit, do **not** run `git init`, do **not** push. The repo is not under git yet.
- Do **not** stop or restart the Moorage dev server that is running on `http://localhost:5080` (a background task owned by Claude). Claude will rebuild it and verify the result in the browser.
- Keep the existing code style:
  - English comments explaining *why*,
  - small functions,
  - errors wrapped with context,
  - LF line endings,
  - `gofmt`.
- Do not refactor unrelated code.
- Go runs inside WSL (not on Windows). From Windows:
  - vet: `wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && go mod tidy && go vet ./...'`
  - tests: `wsl -d Ubuntu -- bash -lc 'export PATH=$HOME/.local/go/bin:$PATH; cd /mnt/e/Apps/moorage && go test -count=1 ./...'` (`-race` is unavailable because CGO is off)
  - In Git Bash prefix these with `MSYS_NO_PATHCONV=1`.
- UI checks run in Windows: `cd E:\Apps\moorage\web; npm run check; npm run build`. svelte-check must report 0 errors.
- Docker 29.8 runs in WSL. Sample stacks are in `~/moorage-dev/stacks`:
  - `whoami` has a `ticker` service that prints `tick N` and sometimes `WARN slow response …`,
  - `crashy` exits with code 3 after printing `FATAL: out of shared memory`.

  Use them for manual checks if you need to, but do not change them.

## 5. Tests you must add
- Monitor:
  - threshold and window counting,
  - recovery after the quiet window,
  - silenced during deploy (existing `StackEvent`),
  - stopped stack does not alert,
  - ring buffer bound,
  - alert context contains the matched lines.
- Filter:
  - substring vs. regex,
  - case sensitivity,
  - invalid regex rejected by validation,
  - pattern length limit.
- Agent path: extend `internal/tunnel/tunnel_test.go` (or the host tests) so that a watch through the tunnel delivers only matching lines.
- Reconnect: a fake host whose stream ends must be re-followed without duplicating or replaying lines.

All existing tests must still pass.

## 6. Documentation
- `PLAN.md`: add a section `### M5 – hotovo (<date>)` under "Stav milníků" in Czech, in the same style as M4:
  - what works,
  - decisions marked **[A]** with reasons,
  - what was verified,
  - known limits.

  Change "Další krok" to M6.
- `README.md`: one feature bullet for log alerts, and update the status line to M5.

## 7. Report back (your final message)
1. Files changed or added, one line each.
2. Design decisions you made that this prompt did not dictate, with reasons.
3. Exact output summary of `go vet`, `go test ./...` and `npm run check` (pass/fail counts). Do not claim a pass you did not see.
4. What you did not finish or could not verify, and anything you suspect is wrong.

## Appendix – status note from the previous session (Czech, verbatim)

# Moorage – co není v PLAN.md (stav 2026-11-20, 17:06)

## Zastaralé údaje v PLAN.md (neopraveno)
- Hlavička: „Stav: návrh, čeká na schválení“ – plán je schválený, M1–M4 jsou hotové.
- Technologie (ř. 58): „Odhad: 15–40 MB RAM“ – skutečnost je 39–45 MiB PSS, limit je 100 MB.
- M1 „Otevřené: RAM ~89 MB RSS (cíl < 50 MB)“ a poznámky o RAM u M2/M3 – vyřešené měřením v části Náklady. RSS bylo zkreslené tím, že započítává stránky binárky.
- Tabulka v části Výchozí analýza: Dozzle má vlastní alerty (výrazy nad logy → webhook), takže „Notifikace: alerty, webhooky“ sedí. M5 se od nich má odlišit kontextem a vazbou na stack.

## M5 – rozpracovaný návrh (nic není v kódu)
- Pravidlo z logu by byl **nový typ kontroly `log`** v `internal/monitor`. Dostane tak zdarma pásek, incidenty, ztišení při deployi, seskupení alertů i kanály.
- Nastavení pravidla:
  - host,
  - stack a volitelně kontejner (služba),
  - vzor (regex nebo klíčové slovo, velká a malá písmena volitelně),
  - práh N shod za okno W.
- Stav: **down** po N shodách v okně, **up** po okně bez shody. Kontextem incidentu jsou nalezené řádky a kolem nich několik řádků logu.
- **Filtrování na agentovi:** `StreamLogs` dostane filtr a agent posílá přes tunel jen shodné řádky. Tím se splní požadavek z PLAN „agent sleduje jen kontejnery s pravidlem“ a šetří se tunel.
- Regex v Go (RE2) běží lineárně, takže ReDoS nehrozí. Délku vzoru je přesto potřeba omezit.
- Kontejnery se při restartu nebo novém deployi mění (nové ID). Sledování se proto po skončení streamu musí znovu připojit, a to s `since=now`, aby se staré řádky nehlásily dvakrát.
- Dotčený kód: `internal/host/host.go` (`StreamLogs`), `internal/host/agentapi.go`, `internal/engine/logs.go`, `internal/monitor/{probe,monitor}.go`, `internal/api/monitor.go`, `web/src/views/CheckForm.svelte`.

## Prostředí (mimo repo)
- Dev server Moorage běží na `http://localhost:5080` (background task bqzgyr2ej).
- Ve WSL zůstaly image `louislam/dockge:1`, `amir20/dozzle:latest` a `louislam/uptime-kuma:2`, dohromady asi 3,8 GB. Kontejnery a data s testovacími účty jsou smazané.
- Měřicí skripty jsou v repu v `scripts/ram/` (`trio.sh`, `mem.sh`, `pss.sh`, `nodebase.sh`). Běží ve WSL, `pss.sh` jako root.
- Pomocné skripty pro vývoj jsou jen v dočasném scratchpadu a po restartu zmizí: `gocmd.sh`, `run.sh`, `agent.sh`, `hookrecv.py`, `whoami.yaml`, `crashy.yaml`.
- Nic není commitnuté a projekt zatím není git repozitář.

Note for this task: the "stale data in PLAN.md" items above are out of scope. Leave them for Claude unless one blocks you.
