# Feature R2: planned maintenance windows

Read `prompts/roadmap-agent-common.md` first; it applies fully. Feature R1 (audit log) was just added. New mutating routes must be added to its route→action map, or its test fails.

## Goal
An admin plans maintenance ahead (e.g. Sunday 02:00–03:00 DB upgrade). During it:
- no alerts are sent for the affected checks and log rules;
- the status page shows "Maintenance" with the admin's message instead of "Down".

## Existing mechanism to build on
`internal/monitor/monitor.go`:
- `DeployKinds` and `silence` (host/stack → until) already silence checks for a while after a deploy;
- a failing check in that window gets `StatusSilenced` and no alert;
- log watcher matches carry `silenced`.

Maintenance is a planned, named, persisted version of that. Read how silencing and recovery alerts work before you design it.

## Required design
- **Table `maintenance_windows`** (migration):
  - id, title, message (shown publicly, optional);
  - scope: all checks, or a list of host/stack pairs and/or check ids. Store it in a child table or JSON; your choice, say why;
  - schedule: **one-off** (start, end) or **weekly** (weekdays, start time, duration, IANA time zone; default the hub's time zone);
  - enabled, created_by, created_at.
- **Effect while active:**
  - Checks in scope still run and store results.
  - A failure is marked with a new state `maintenance` (not `down`, not `silenced`) and sends no alert.
  - Log-rule matches in scope send no alert.
  - **No alert is lost.** If a check is still down when the window ends, the normal down alert goes out right after the end (once). If it recovered during the window, no "recovered" alert for a down that was never announced. Write this down as a decision.
  - Uptime %: maintenance time is excluded from the uptime calculation of checks and status pages (say how). Daily bars on status pages show maintenance as its own colour or state.
- **Status page:**
  - while a window covering any item is active, show a "Maintenance" banner with the title, message and planned end;
  - item status `maintenance`;
  - upcoming windows (next 7 days) listed as "Planned maintenance";
  - RSS: an item when a window starts.
- **Hub watchdog (M6)** and host-offline alerts: windows don't silence those unless the scope is "all" — say what you chose and why.
- **Times:**
  - all computation in UTC;
  - weekly windows expand in their time zone (DST: a window at 02:30 on a day that skips 02:30 starts at the next valid time; test it);
  - "is active now" must be cheap per check run (precomputed in memory, refreshed on change and every minute). Restart-safe: everything comes from the DB.
- **API:** CRUD under `/api/maintenance` (admin), list for viewers (read-only). Validation:
  - end > start;
  - duration 5 min – 7 days;
  - scope not empty unless "all";
  - unknown checks rejected.
- **UI:**
  - A "Maintenance" list (where it fits next to Checks / Status pages; say why). Each window shows active / upcoming / past with text, plus create/edit with schedule and scope pickers.
  - A stack page or check page in an active window shows a "Maintenance until …" pill.
  - A check in state `maintenance` has its own label in lists.
  - Must work at 375 px.
- **Audit:** create/update/delete go through R1's map. Window start and end are not audit rows; they are stack events of kind `maintenance` (not a deploy kind), so they appear in the stack's Logbook.

## Tests (at least)
- A failing check in an active window: state `maintenance`, no alert. Still down at the end: exactly one down alert. Recovered during the window: no alert.
- A log rule in scope: no alert during the window.
- Out of scope: alerts as usual.
- Weekly expansion incl. DST skip and fall-back (no double window), across midnight, multiple weekdays.
- Uptime excludes maintenance time.
- Status page: banner, item status, upcoming list.
- Restart in the middle of a window keeps it active (rebuilt from the DB).
- Validation errors; viewer can list but not change.

## Out of scope
- Notifying subscribers about planned maintenance (there are no subscribers).
- Calendar (iCal) export.
