# Feature R6: checks from multiple locations (agent voting)

Read `prompts/roadmap-agent-common.md` first; it applies fully. Already in the code:
- R1 audit log (`auditActions`);
- R2 maintenance windows;
- R3 `/metrics`;
- R4 stack timeline;
- R5 dependency graph and cascade alerts.

## Goal
Today HTTP / TCP / ping / DNS checks run only from the hub. A blip in the hub's own network becomes a false "down". Agents already run on other machines, so let them probe too:
- a check can run from several **locations** (the hub and chosen agents);
- it counts as down only when enough locations agree.

Uptime Kuma can't do this.

## Required design
### Agent side
- **New agent API endpoint** `POST /probe`. It takes a check definition (kind `http|tcp|ping|dns`, target, timeout, the kind's settings), runs the **same probe code** as the hub (`internal/monitor/probe.go`; move the pure probe functions into a package both can use if needed) and returns ok / latency / message.
  - `stack` and `log` checks are never probed remotely; they already run where the containers are.
- **Agent-enforced limits:**
  - timeout capped at 30 s;
  - at most 8 probes at once (the 9th gets a "busy" error and counts as "no answer", not "down");
  - body read limits as on the hub.
- **Switch:** `MOORAGE_PROBES` (default `true`). A read-only agent may probe, because probing changes nothing on the host. Write down why.
  - Probing lets the hub reach the agent's network: the hub can already run any compose stack there, so this adds no new power. Document it in Security notes anyway.
  - `AgentInfo.probes` reports the switch.
- Older agents (404) and agents with probes off: the location is "unsupported" and excluded, shown in the UI.

### Hub side
- **Locations per check:**
  - new column / child table: a list of location ids (`hub` and agent host ids). Default: `hub` only, so existing checks behave exactly as today;
  - **quorum:** `majority` (default when more than one location), `any` (down if any location fails) or `all` (down only if every answering location fails).
- **One run:**
  - query all locations in parallel, each with the check's timeout plus a small margin;
  - an offline location, an unsupported one or one that gave no answer doesn't vote;
  - if nobody answered, the result is `unknown` with message "no location could run the check". It must not alert as down; say how it shows;
  - otherwise apply the quorum to the votes.
- **Storage:**
  - the aggregated result goes to `check_results` as today (uptime, incidents and alerts unchanged);
  - per-location results go to a new table `check_location_results` (check_id, ts, location, ok, latency_ms, msg) with the same retention as results.

  Bound the growth and state it.
- **Messages:** the aggregated message names the failing locations, e.g. "down from 2 of 3 locations: wd1 (timeout), hub (connection refused)". Alerts include the per-location breakdown (in the alert context; check `AlertContext`).
- Maintenance (R2), deploy silence and cascade grouping (R5) work on the aggregated result unchanged.
- **Metrics (R3):** `moorage_check_location_up{check_id,…,location}` and latency per location, bounded by the number of locations.

### UI
- Check form:
  - a location picker (Hub + agents, with unsupported / offline ones disabled and the reason as text);
  - a quorum select shown only with 2+ locations;
  - a short explanation.
- Check detail: a per-location row with the last status, latency and message, and the latency per location over time if the existing chart allows it cheaply.
- Must work at 375 px.

## Tests (at least)
- Quorum math (majority with 1, 2, 3, 4 voters; ties: say how — e.g. 2 of 4 failing is not a majority), `any`, `all`, abstentions, nobody answers → unknown without an alert.
- Agent endpoint: refuses `stack`/`log`, caps the timeout, the concurrency limit gives busy, `MOORAGE_PROBES=false` refuses with a code, a read-only agent still probes, an older agent (404) is unsupported.
- A default check (hub only) behaves bit for bit as before (existing tests pass untouched).
- Per-location rows written and pruned; the aggregated message and alert breakdown.
- An agent going offline mid-run doesn't block the run beyond the timeout.

## Out of scope
- Public probe locations run by others.
- Geo labels.
- Push / heartbeat checks.
