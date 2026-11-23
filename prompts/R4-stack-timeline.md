# Feature R4: stack timeline (incident timeline)

Read `prompts/roadmap-agent-common.md` first; it applies fully. Already in the code:
- R1 audit log (`audit_log`; new mutating routes go into `auditActions` in `internal/api/audit.go`);
- R2 maintenance windows (check state `maintenance`, stack events of kind `maintenance`);
- R3 `/metrics`.

## Goal
One place that answers **"what changed before it went down?"**. Today the stack's History tab (`web/src/views/HistoryView.svelte`) stacks separate sections on top of each other:
- Audit log;
- Maintenance;
- Terminal sessions;
- Image updates;
- versions by day.

Incidents of the stack's checks are elsewhere (check pages). Merge it all into one timeline.

## Sources (all exist already; do not collect new data)
- versions (git history of compose/.env, with diff on click, as today);
- stack events (deploy, restart, stop, down, update, rollback, terminal, maintenance; `StackEvent` in the monitor and its store table);
- incidents of the stack's checks, including log-rule checks (open/close, cause);
- image updates (`image_updates`);
- audit rows of the stack (R1);
- terminal sessions of the stack (M9).

## Required design
- **API:** `GET /api/hosts/{host}/stacks/{name}/timeline?before=<cursor>&limit=<n>&kinds=<csv>`:
  - one merged list, newest first;
  - each entry has `at`, `kind` (`version`, `event`, `incident`, `update`, `audit`, `terminal`), a short title, actor, detail and a reference id (version hash, check id + incident id, update id…).
  - **Paging must be stable across sources:** cursor = (at, kind, id); test that no entry repeats or is skipped across pages when several sources have the same second.
  - Each source query is bounded by `limit` (no full-table loads). Default limit 100, max 500.
- **Roles:** viewers see versions, events, incidents and updates. Audit and terminal entries are admin-only, filtered on the server (not just hidden in the UI).
- **Avoid duplicates:** an action today often leaves both an audit row and a stack event (e.g. "deploy"). Show it once:
  - merge an audit row and a stack event of the same action within a few seconds into one entry (actor from the audit row, outcome from the event);
  - say exactly which pairs you merge, and test it.
- **"Before it went down":**
  - an incident entry has a "Show what changed before" action that scrolls/filters the timeline to the 2 hours before the incident start and highlights the changes in that span;
  - the check page's incident list links to the stack timeline at that point (`#/h/{host}/stacks/{name}/history?at=<unix>` or the router's equivalent — say what you chose).
- **UI:** replace the stacked sections in HistoryView with the timeline:
  - day headings;
  - kind filter chips;
  - an icon and text label per kind (never colour alone);
  - version entries keep the existing diff panel and "Restore";
  - "Load more" at the end;
  - must work at 375 px.

  Keep the existing behaviours of the sections you replace (e.g. version restore, the update rollback link).
- **Performance:**
  - one request per page, not one per source from the browser;
  - git history reads must stay bounded (the history package already pages; reuse it).

## Tests (at least)
- Merge order and stable paging with equal timestamps across sources.
- Viewer gets no audit/terminal entries (server-side); admin does.
- The audit + event de-duplication.
- `kinds` filter.
- An incident's "before" window returns the changes in the 2 h before its start.
- Older agents: version history comes from the host; when that fails, the timeline still shows hub-side sources and says that versions are unavailable.

## Out of scope
- New data sources (container restart counts from Docker events, metrics).
- Cross-stack timeline (that comes with R5's dependency graph later).
