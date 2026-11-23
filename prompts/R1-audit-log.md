# Feature R1: audit log of all actions

Read `prompts/roadmap-agent-common.md` first; it applies fully.

## Goal
Answer "who stopped this service, when and from where?". Every change made through Moorage is recorded: who, when, from which IP, what, on which host/stack, and whether it succeeded. Today only terminal sessions are audited (M9, table `terminal_sessions`).

## Required design
- **Table `audit_log`** (new migration):
  - id;
  - at (unix);
  - actor (username, or `system` for automatic actions);
  - ip;
  - action (stable key, e.g. `stack.up`, `stack.save`, `image.rollback`, `user.role`);
  - host_id, stack (empty when not applicable);
  - target (e.g. check id, user name, channel id; from path values, never from the body);
  - outcome (`ok` / `failed`).

  Index by `at`, and by `(host_id, stack, at)`. Keep 1 year (prune with the existing daily maintenance, like `PruneTerminalSessions`).
- **One middleware, not edits in every handler.** Wrap mutating requests (`POST`, `PUT`, `PATCH`, `DELETE`) under `/api/`:
  - derive the action from Go's `r.Pattern` (Go 1.22+ mux) through one map from route pattern to action key;
  - take host/stack/target from `r.PathValue`;
  - record after the handler with the status code: 2xx → `ok`. 4xx from validation / busy / refused is **not** recorded, because nothing happened. 5xx → `failed`.

  A route that is mutating but missing from the map must fail a test, so new routes can't silently skip the audit (test: walk the registered patterns, or keep the list next to the routes).
- **Never record request bodies or query values that can hold secrets** (passwords, TOTP codes, compose/.env content, tokens, webhook URLs). Only the action key and path values.
- **Sign-in:** record `auth.login` ok and `auth.login_failed` (with the username tried and IP), `auth.logout`, setup. Use the same IP logic as the rate limiter (`clientIP`).
- **Automatic actions** with actor `system`: auto-update applied, its rollback, and anything else the hub does on its own (check `internal/updater`). Use one small function, e.g. `store.Audit(ctx, entry)`, also from the middleware.
- **Terminal sessions:** opening one also writes an audit row (`terminal.open`, target = container · shell). The detailed session row stays in `terminal_sessions`.
- **Async operations** (up/pull/update return an op id at once): record when the request is accepted. The op's own result is already in stack events and the op log; do not add a second mechanism.
- **API:** `GET /api/audit` (admin only), with filters `host`, `stack`, `actor`, `action` (prefix), `before` (id, for paging) and `limit` (default 100, max 500). The response includes the host name.
- **UI:**
  - A global **Audit log** view for admins:
    - pick the place that fits the current navigation (Settings area or Hosts) and say why;
    - a table with time, actor, IP, action as readable text (e.g. "Stopped stack"), host / stack, target, outcome;
    - filters (actor, stack, action group) and "Load more";
    - works at 375 px.
  - On the stack's **History** (Logbook), admins see that stack's audit entries; viewers don't.
  - Readable action labels live in one map in the UI.
- Viewers never see the audit log (403 on the API).

## Tests (at least)
- Every mutating route pattern is mapped (fails on an unmapped one).
- `ok` on 2xx, nothing on a refused 4xx (e.g. busy, validation), `failed` on 5xx.
- No body content in any row: post a request with a password / compose secret and assert it is absent from the DB.
- Login ok / failed / logout rows with IP.
- `system` rows from the updater.
- Viewer gets 403 on `/api/audit`; filters and paging work.
- Pruning after 1 year.

## Out of scope
- Shipping the log to an external system.
- Tamper-proofing (hash chains).
- Recording reads.
