# Feature R3: Prometheus metrics endpoint (`/metrics`) + Grafana dashboard

Read `prompts/roadmap-agent-common.md` first; it applies fully. Earlier roadmap features (R1 audit log, R2 maintenance windows) are already in the code. New mutating routes must be in R1's route→action map.

## Goal
People already run Grafana + Prometheus/VictoriaMetrics. Give them Moorage's state there instead of duplicating graphs:
- checks;
- hosts;
- stacks;
- containers;
- image updates.

## Required design
- **Off by default.**
  - Enable with `MOORAGE_METRICS_TOKEN=<secret>` on the hub/all-in-one. Requests must send `Authorization: Bearer <secret>`; compare in constant time.
  - Without the token set, `/metrics` answers 404.
  - Never served on the public status-page listener or status domains.
  - Not behind the session cookie (Prometheus can't sign in).
  - Add the variable to the config with validation (min 16 chars).
- **No new dependency.** Write the Prometheus text exposition format by hand:
  - `# HELP` / `# TYPE`;
  - label value escaping (`\`, `"`, newline) — test it.
- **Cheap scrapes.**
  - A scrape must not call Docker or the agents synchronously for each request. Build from the in-memory state the monitor and registry already keep (check states, latencies, host connection).
  - For container/stack data, use a snapshot refreshed in the background at most every 30 s while scrapes happen, and stop refreshing after 5 min without a scrape.
  - Bound the work per host with a timeout. An offline host yields `moorage_host_up 0` and no container series, not a slow scrape.
- **Metrics** (adjust names to Prometheus conventions, base units: seconds, bytes):
  - `moorage_build_info{version}` 1;
  - `moorage_host_up{host}`, `moorage_host_read_only{host}`, `moorage_agent_info{host,version}`;
  - `moorage_check_up{check_id,check,type,host,stack}` (1 up, 0 down; with R2, add `moorage_check_maintenance` or a `state` label — pick one, explain);
  - `moorage_check_latency_seconds{…}` (last), `moorage_check_uptime_ratio{…,window="24h"|"30d"}`;
  - `moorage_stack_containers{host,stack,state}`;
  - `moorage_container_up{host,stack,service,container}`, `moorage_container_restarts_total`, `moorage_container_cpu_ratio`, `moorage_container_memory_bytes`, `moorage_container_memory_limit_bytes` (only if the existing stats code can provide them cheaply; otherwise leave CPU/RAM out and say so);
  - `moorage_image_update_available{host,stack,service}` from the M8 `image_checks`;
  - `moorage_alerts_sent_total{channel_type}` counter if easy.
  - **Cardinality:** no unbounded labels (no error messages, no image digests, no user names).
- **Grafana dashboard** JSON in `deploy/grafana/moorage-dashboard.json`:
  - datasource as a variable;
  - panels for host up, checks up/down + latency, uptime table, containers per stack, update-available table.
  - Validate that it is well-formed JSON.
- **UI:** no new view. On Settings/Hosts (wherever the config hints live), show one line whether metrics are enabled and the scrape URL, never the token.
- **Example Prometheus scrape config** in your report (the lead puts it in README).

## Tests (at least)
- 404 without the env; 401 with a wrong or missing token; 200 with the right one; status listener/domain never serves it.
- The output parses as valid exposition format: write a small checker in the test (metric name regex, label syntax, HELP/TYPE before samples, no duplicate series).
- Label escaping.
- A scrape doesn't hit the host when the snapshot is fresh (fake host counting calls); an offline host doesn't block.
- Uptime/latency values match the monitor's state for a fake check.

## Out of scope
- OpenTelemetry push, remote write.
- Per-user metrics.
- Histogram of latencies (a gauge of the last value is enough).
