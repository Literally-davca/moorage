# Feature R5: dependency graph between stacks + cascade alerts

Read `prompts/roadmap-agent-common.md` first; it applies fully. Already in the code:
- R1 audit log (`auditActions` map for new mutating routes);
- R2 maintenance windows;
- R3 `/metrics`;
- R4 stack timeline.

## Goal
Stacks depend on each other: a shared Postgres stack, a reverse proxy, a message broker. When the database stack goes down, today every app that uses it alerts separately, so the person gets five messages for one cause. The graph should:
- show which stacks depend on which;
- turn a cascade into **one** alert that names the cause.

## Required design
### Edges
- **Table `stack_deps`:**
  - from (host, stack) → to (host, stack);
  - source `detected` / `manual`;
  - `ignored` flag for a detected edge the admin rejected;
  - created_by, created_at.

  Edges only within one host, unless an admin adds a manual cross-host edge (allow it).
- **Detection**, on the agent side from the compose model the engine already loads; a new agent API endpoint that returns the facts, with the hub computing the edges. Older agents (404) give no detected edges, and the UI says so.
  - **Shared networks:** stack A declares network `X` (not external), stack B uses `X` as `external: true` (by name, including `name:` overrides) → B depends on A.
  - **Hostnames:** a service in B refers to a service name or container name of A in `depends_on` (only within a stack, so skip), `links`/`external_links`, or in env values that look like `host:port` or a URL host. Only count it when A and B share a network. Mark it "detected (hostname)" and keep it conservative.
  - Recompute on deploy/save and every 10 min; never more than one detection run at a time per host. Detected edges that disappear are removed unless they were made manual.
- **Manual:** admins add or remove edges and ignore detected ones (R1 audit via the map).

### Cascade alerts (the main value; be careful, alerts must never get lost)
- When a check of stack B goes down while an **upstream** stack A (direct or transitive, max depth 3) has an open incident that started at most 10 min earlier or is still open:
  - B's down alert is still recorded as its own incident;
  - its notification is folded into A's: one message "postgres is down — also affected: app1, app2 (probably caused by postgres)";
  - use the existing batching (`enqueue`/`flush`, `BatchWindow`, group keys) and extend the grouping key with the root cause stack.
- If B goes down **before** A (order race), B's alert may already be gone. That is fine; don't delay alerts to wait for a cause. Say how you handle A's alert then (e.g. "also affected: B (alerted earlier)").
- Recoveries: one message when the root recovers, listing which dependents recovered with it. Dependents that stay down after the root recovered alert normally (with a note "still down after postgres recovered").
- Maintenance (R2) and deploy silence keep priority over cascade grouping.
- Cycles in the graph: detect and ignore for grouping (no infinite loops); show a warning in the UI.

### UI
- **Graph view per host** (Hosts or a "Dependencies" view; say where):
  - hand-made SVG with a simple layered layout (no graph library unless tiny and MIT — justify);
  - nodes coloured by status **with text**;
  - edges marked detected / manual / ignored;
  - click a node → stack page.
  - At 375 px, fall back to a list "X depends on Y".
- Stack page: "Depends on" / "Used by" with status, and admin controls to add / remove / ignore.
- The timeline (R4) shows a cascade incident with its root cause.

## Tests (at least)
- Detection from compose fixtures: external network by name and by `name:` override; hostname in env only when a network is shared; no edge without sharing; ignored edges stay ignored after re-detection; manual edges survive.
- Cascade: A down then B down within 10 min → one message naming A, both incidents stored. B down first then A → no lost alert. Recoveries as described. Transitive depth and cycles. Maintenance on B wins (no alert). A dependent without channels doesn't break the grouped message's channel set (union of channels; say how).
- Older agent (404) → no detected edges, manual still work.

## Out of scope
- Kubernetes-style health propagation.
- Auto-restart of dependents.
- Cross-host detection.
