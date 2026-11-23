# Task: implement milestone M7 "Public status page" in Moorage

You are a senior Go + Svelte engineer working directly in the repository `E:\Apps\moorage` (WSL path `/mnt/e/Apps/moorage`). You do the main implementation. Another engineer (Claude) will review your work afterwards, run it against a real browser, a real reverse-proxy-style Host header and a real feed reader, and fix mistakes. Favour clear, conventional code over clever code, and report honestly what you did not finish or could not verify.

This milestone adds the **first page that anyone on the internet can open without signing in**. Every bug here is either a **data leak** (internal hostnames, URLs, error messages, logs reaching the public) or a **routing bug** (the admin UI or API reachable through the public domain). Design for those two first; section 6 lists the cases that must hold.

## 1. Project context

Moorage is a self-hosted, MIT-licensed single Go binary that combines what Dockge (compose stacks), Dozzle (live logs) and Uptime Kuma (uptime checks) do. Milestones M1–M6 are done and verified. Read these before you start:
- `PLAN.md`:
  - "Rozhodnutí" (security and data decisions),
  - "Stav milníků" M4 (checks, incidents, states, deploy silence) and M5 (log checks),
  - "Náklady" (RAM limit).
- `README.md` – modes, config table, security notes.
- `.interface-design/system.md` – UI direction. It already says the public status page defaults to the **light "sailcloth" theme** and uses the **Soundings strip** signature. Follow it.

Stack:
- Go 1.27, module `moorage`. Single binary with modes `all-in-one`, `hub`, `agent` (`cmd/moorage/main.go`). The status page lives on the hub (all-in-one or hub mode), never on an agent.
- SQLite via `modernc.org/sqlite`. Migrations are a list in `internal/store/store.go`; append, never edit old ones.
- UI: Svelte 5 (runes) + TypeScript in `web/`, built by Vite into `internal/web/dist` and embedded with `go:embed`.

Relevant code today:

| Path | What it is |
|---|---|
| `internal/api/api.go` | `Server.Handler()`: one `http.ServeMux`. `s.user(...)` / `s.admin(...)` wrap routes with the session cookie check. `secure()` sets CSP, `X-Frame-Options: DENY`, `nosniff`, `no-referrer`, and requires the `X-Moorage: 1` header on every non-GET request (CSRF). `spa()` serves the embedded UI and falls back to `index.html` for unknown paths. `clientIP` is `RemoteAddr` (no proxy headers are trusted). |
| `internal/api/monitor.go` | checks API: `checkDTO` (state, uptime 24 h/7 d/30 d, ticks, incidents with `AlertContext`, anchors), channels, watchdog. |
| `internal/store/monitor.go` | `Check` (`HostID` + `Stack` link a check to a stack), `Result` (status `up`/`down`/`silenced`), `Incident` (`Cause`, `Context` = JSON with containers and **log lines**), `Uptime(checkID, since)`, `Incidents(checkID, n)`. Raw results are kept **30 days**; there are no hourly or daily aggregates yet. Index `check_results(check_id, ts)`. |
| `internal/monitor/monitor.go` | `Monitor.State(id)` (in memory: `up`, `down`, `pending`, `silenced`, `paused`, `unknown`, plus `Last.Msg`), `Ticks`, alert context. |
| `internal/config/config.go` | `MOORAGE_*` env variables, incl. `MOORAGE_PUBLIC_URL` (base URL of the admin UI, used in alerts). |
| `web/src/App.svelte`, `web/src/views/Shell.svelte` | SPA shell, hash routing (`#/checks`, `#/h/<host>/stacks/<name>`…), sidebar navigation. |
| `web/src/views/ChecksView.svelte`, `CheckDetail.svelte` | how checks, states and the strip are shown in the admin UI today. |
| `web/src/lib/Strip.svelte` | the Soundings strip component. |

## 2. Goal of M7

From `PLAN.md`: a page **without sign-in**, showing a **selection of stacks and checks**, servable on **its own domain**, with an **RSS feed served with the correct content type** (Uptime Kuma served it wrongly).

## 3. Required design

### 3.1 Data model (new migration)
- `status_pages`: id, `slug` (unique), `title`, `description` (plain text, optional), `domain` (unique when set, optional), `published` (bool, default false), `show_incidents` (bool, default true), created/updated timestamps.
- `status_page_items`: id, page id (ON DELETE CASCADE), `position`, optional `section` (a heading text; consecutive items with the same section are grouped), `display_name` (required, what the public sees), and **exactly one** of:
  - `check_id` (FK to `checks`, ON DELETE CASCADE), or
  - `host_id` + `stack` (a stack item).
- Several pages are allowed (e.g. one for friends, one for customers). Each page has its own selection.

### 3.2 What the public sees – and nothing else
**[security]** The public page and the feed may contain **only**:
- page title and description;
- per item: `display_name`, the public status, uptime %, the daily bars (3.4);
- per incident (if `show_incidents`): item display name, start time, end time or "ongoing", duration.

They must **never** contain: check names (the admin may use internal names – use `display_name`), check kinds, targets (URLs, IPs, hostnames, ports, DNS records), host IDs or host names, stack names, container names, probe messages (`Last.Msg`, `Result.Msg`), incident `Cause` or `Context`, log lines, latency, deploy anchors (they reveal deploy times and actors), check or incident IDs in visible text, users, channels. Build the public data from a **dedicated DTO** that only has the allowed fields; never reuse `checkDTO` or serialise store structs.

Public status per item (map the internal states):

| Internal | Public label | Colour token |
|---|---|---|
| `up`, `pending` (failing but under the retry limit) | Operational | `--afloat` |
| `down` | Down | `--adrift` |
| `silenced` (deploy window) | Maintenance | `--signal` |
| `paused` (stack stopped on purpose) | Paused | `--becalmed` |
| `unknown`, disabled check | No data | `--becalmed` |

- A **stack item** aggregates all **enabled** checks linked to that stack (`HostID` + `Stack`): status = worst of them (Down > Maintenance > Operational > Paused > No data), uptime = all their up/down results together.
- A stack with no linked checks has nothing to show. The admin UI only offers stacks that have at least one check and explains how to add one; an existing item whose stack lost all checks shows "No data".
- Page banner: "All systems operational", or "N of M services down", or "Maintenance in progress" – derived from the items.

### 3.3 Routing, domains and listeners
- **Paths on the main listener** (`MOORAGE_LISTEN`), outside the SPA and outside `/api/`:
  - `GET /status/<slug>` – the page,
  - `GET /status/<slug>/feed.xml` – RSS,
  - static assets for the status page under a fixed prefix, e.g. `/status-assets/…`.
- **Custom domain:** when a request's `Host` (lowercased, port stripped) equals a page's `domain`, the hub serves **only** that page on that host:
  - `/` → the page, `/feed.xml` → its feed, the status assets, `favicon`;
  - **everything else returns 404**, including `/api/…`, `/status/<other-slug>`, the SPA and its assets. The admin UI and API must not be reachable through a status domain.
  - TLS is the reverse proxy's job (PLAN: reverse proxy/TLS is out of Moorage's scope). Document a Caddy/Traefik example in the README.
- **Optional separate listener `MOORAGE_STATUS_LISTEN`** (e.g. `:5085`, empty = off): serves **only** status pages (by path and by domain), never the API or the SPA. This lets the user publish only the status port and keep the admin UI on LAN/VPN. Start it in `main.go` next to the main server, with the same timeouts.
- Unknown slug, unknown domain on the status listener, and an **unpublished** page all return the **same** 404 response, so nobody can probe which slugs exist. An admin can preview an unpublished page from the admin UI (e.g. `GET /api/status-pages/{id}/preview` returning the public DTO, or the page with an admin cookie – your choice, explain it).
- **Validation:**
  - slug `^[a-z0-9][a-z0-9-]{0,62}$`, unique;
  - domain: lowercase hostname (no scheme, port or path), unique, and **not** equal to the host of `MOORAGE_PUBLIC_URL` – otherwise the admin UI would disappear behind the status page. Refuse with a clear message;
  - title ≤ 100 chars, description ≤ 500 chars, display name ≤ 100 chars, at most 100 items per page.

### 3.4 Rendering, caching, cost
- **[A, follow it]** Render the public page **on the server** with Go `html/template` (contextual escaping), not with the Svelte SPA: no JS bundle for visitors, nothing to break, smallest attack surface. Put it in a new package, e.g. `internal/status`.
- Plain text only: no Markdown, no custom HTML, no custom CSS or JS (Kuma had XSS through status page Markdown and custom CSS).
- Stricter headers for status responses: `Content-Security-Policy: default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self' data:; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`, plus the existing `nosniff`, `no-referrer`, `X-Frame-Options: DENY`. No cookies are read or set on public responses. Only GET/HEAD are allowed.
- **Times:** render every time as `<time datetime="RFC 3339 UTC">` with UTC text. One small static script (`/status-assets/status.js`, no inline JS) rewrites them to the visitor's local time. Without JS the page still works and shows UTC.
- **Refresh:** `<meta http-equiv="refresh" content="60">` is enough; no WebSocket, no polling API.
- **Daily bars (the strip on the status page):** one bar per day for the last 30 days (data retention is 30 days), UTC days. Bar colour by the day's worst state, bar tooltip (`title`) with the date and uptime % of that day. Silenced results never count as down. Days without data are grey. Follow the strip measurements in `system.md`; no deploy anchors.
- **Uptime %:** 24 h and 30 d per item, from `up`/`down` results only (as `Store.Uptime` does).
- **Cache:** anonymous traffic must never hit the database per request. Build each page's public data at most once per **60 s** (TTL), with one builder at a time per page (no stampede when 100 requests arrive together), and invalidate on page edits. Send `Cache-Control: public, max-age=60`. Make the clock and TTL injectable for tests.
- Add one store query for the daily bars that reads all items' checks in one pass (e.g. `GROUP BY check_id, day`), using the existing index.
- **Fonts:** the design uses Onest and JetBrains Mono, embedded in the SPA build with hashed names. Either serve them from the embedded dist by finding the file at startup (glob on `onest-latin-400-normal-*.woff2` etc.) or fall back to the system font stack. Do not add a second copy of the font files. Say which you chose.
- Light theme by default (`sailcloth` tokens), dark via `prefers-color-scheme: dark`. Works at 360 px width.

### 3.5 RSS feed
- `Content-Type: application/rss+xml; charset=utf-8`. Valid RSS 2.0 with `atom:link rel="self"`. Generate it with `encoding/xml`, never by string concatenation.
- One `<item>` per state change of the page's items in the last 30 days:
  - "<display name> is down" when an incident starts,
  - "<display name> is back after 12 min" when it ends.
  - Stable `<guid isPermaLink="false">` per event (e.g. `inc-<id>-down` / `inc-<id>-up`) so readers don't show duplicates after a refresh; `pubDate` in RFC 1123Z; newest first; max 50 items.
- No cause, no context – same rules as 3.2.
- If `show_incidents` is off, the feed has no items (but still exists and is valid).
- Absolute links: `https://<domain>/` when the page has a domain, otherwise `MOORAGE_PUBLIC_URL` + `/status/<slug>`. If neither is set, use the request `Host` with `http://` and mention it in the README. Never build links from `X-Forwarded-*` headers.
- `<link rel="alternate" type="application/rss+xml">` in the page head.

### 3.6 Admin API and UI
- **API (admin only for writes, signed-in user for reads):**
  - `GET /api/status-pages`, `GET /api/status-pages/{id}`, `POST /api/status-pages`, `PUT /api/status-pages/{id}`, `DELETE /api/status-pages/{id}`;
  - the preview from 3.3.
- **UI:** a new sidebar entry **Status pages** (after Checks):
  - list: title, public URL (copy button), published or draft, number of items, overall state;
  - editor: title, slug, description, domain (with one sentence: "Point the domain at your reverse proxy and forward it to Moorage. On this domain Moorage serves only this page."), published switch, "Show incident history" switch;
  - items: add a check or a stack (stacks without checks are disabled with a hint "Add a check to this stack first"), display name (prefilled from the check or stack name, with a note that the public sees only this name), section heading, reorder (up/down buttons are enough), remove;
  - **Preview** before publishing; **Open public page** after.
- Follow `.interface-design/system.md`. Works at phone width.

Svelte 5 pitfalls already hit in this repo:
- an `$effect` reading props or app state refreshed by polling must use `$derived` keys or `untrack`, or it loops and floods requests;
- an `$effect` must not return a Promise; wrap async code in a block.

### 3.7 Limits
- RAM limit for the all-in-one process is **100 MB PSS** with the UI open; today it uses ~49 MiB. The cache holds only small DTOs.
- No new dependencies unless unavoidable. Explain any you add.
- Out of scope (do not build): status badges/SVG embeds, custom CSS, manual incident posts or announcements, scheduled maintenance windows (ROADMAP), per-page time zone setting, OIDC/forward-auth and viewer-role management (separate milestone work), multi-language status pages.

## 4. Working rules
- Do **not** commit, do **not** run `git init`, do **not** push. The repo is not under git yet.
- Do **not** stop or restart the Moorage dev processes. Claude owns them:
  - the dev hub on `127.0.0.1:5080` (UI) and `:5081` (agent port);
  - three test agents (`wd1`–`wd3`, ports 5082–5084, data in `~/moorage-dev/wd1..3`);
  - a webhook receiver on `127.0.0.1:18099`.
  Never run `pkill moorage`. For your own manual runs use other ports (e.g. 5090/5091/5095) and a separate data directory under `~/moorage-dev/m7-*`.
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

## 5. Lessons from the M5 and M6 reviews – avoid these
Your M5 and M6 code passed its own tests, but the review found 4 real bugs in each, all against real processes and real output. They came from cases the tests didn't model:
- **M5:** duplicates after a reconnect, events lost between polls, a false "back up" after a restart, deploy-window errors alerting after the window.
- **M6:**
  - a duplicate "hub is back" from a returning agent;
  - a wrong "offline since" time taken from before the hub restart;
  - a 2 min outage reported as "back after 1 min" because of rounding;
  - an ID shown where a name belonged.

For M7 the analogues are:
- **leaks:** an internal name, URL or error message reaching the public HTML, the feed or a JSON response (also through a `title` attribute, an HTML comment or an error page);
- **routing:** the admin UI, the API or another page reachable through a status domain or the status listener; `Host` with a port or in upper case not matching;
- **time:** day boundaries (a result at 23:59:59 vs 00:00:00 UTC), durations rounded the wrong way ("back after 0 min"), ongoing incidents;
- **names vs IDs:** check names or IDs shown where the display name belongs;
- **cache:** a stale page after an edit, or a stampede of DB queries when the cache expires.

Write a test for each.

## 6. Tests you must add
- **No leak (most important):** create a check with name `internal-db-check`, target `http://secret.lan:8080/health`, a stack `privstack` on host `pi-home`, a down result with message `password=hunter2`, an incident with cause and context containing `logline-xyz`. Put them on a page with display names `Database` and `Website`. Assert that **none** of those internal strings appears in the page HTML, the feed or the preview JSON – check the raw bytes, not a parsed view.
- **Escaping:** a display name or title `<script>alert(1)</script> & "x"` is escaped in the HTML and in the RSS (the RSS parses as XML).
- **Routing:**
  - `Host: status.example.com` → that page at `/`, its feed at `/feed.xml`;
  - the same host with `/api/session`, `/`-SPA assets, `/status/<other-slug>`, `/#/checks` → 404;
  - `Host: STATUS.example.com:443` matches;
  - the status listener never serves `/api/…` or the SPA;
  - unknown slug, unpublished page and unknown domain → identical 404 body and status.
- **Validation:** domain equal to the host of `MOORAGE_PUBLIC_URL` is refused; duplicate slug/domain refused; bad slug refused.
- **States:** pending shows Operational, silenced shows Maintenance and doesn't lower uptime, a stack item shows the worst of its checks, a disabled check shows No data, a deleted check disappears from the page (cascade).
- **Daily bars:** results at `23:59:59` and `00:00:00` UTC land in different days; a day with only silenced results is not red; 30 bars always.
- **RSS:** content type exactly `application/rss+xml; charset=utf-8`; parses with `encoding/xml`; guids stable across two builds; an ongoing incident has only the "down" item; durations are not rounded to "0 min" for a 40 s outage (use "under 1 min" or seconds) and a 2 min outage says 2 min.
- **Cache:** 100 concurrent requests within the TTL build the data once; after an edit the next request shows the change; after the TTL it rebuilds once.
- **Headers:** status responses carry the strict CSP, no `Set-Cookie`, and POST to a status path is refused.

All existing tests must still pass.

## 7. Documentation
- `PLAN.md`:
  - add `### M7 – hotovo (<date>)` under "Stav milníků", in Czech, in the same style as M4–M6: what works, decisions marked **[A]** with reasons, what was verified, known limits;
  - change "Další krok" to M8 (auto-update image).
- `README.md`:
  - status line M7;
  - a feature bullet;
  - `MOORAGE_STATUS_LISTEN` in the config table;
  - a short "Status pages" section: what is public and what never is, custom domain with a Caddy and a Traefik example, the separate listener, the feed URL.

## 8. Report back (your final message)
1. Files changed or added, one line each.
2. Design decisions you made that this prompt did not dictate, with reasons.
3. Exact result summary of `go vet`, `go test ./...` and `npm run check` (pass/fail counts). Do not claim a pass you did not see.
4. What you did not finish or could not verify, and anything you suspect is wrong. List especially any case from section 6 that you could not cover.

## Appendix – current state (2026-11-22)
- M1–M6 are done and reviewed; see `PLAN.md` "M5 – hotovo" and "M6 – hotovo".
- RAM: ~49 MiB PSS all-in-one with the UI open; hub 21 MiB anon in the M6 setup. The limit is 100 MB.
- The dev hub DB has checks (whoami/crashy stack checks, a DNS check, log checks 4 and 5) and webhook channel 1. You may read it for ideas, but do not change the dev hub's data; use your own data directory.
- Docker 29.8 runs in WSL. Sample stacks are in `~/moorage-dev/stacks` (`whoami`, `crashy`).
- Nothing is committed and the project is not a git repository yet.
- Known stale bits in `PLAN.md` are out of scope: header "návrh, čeká na schválení", the RAM estimate in "Technologie", old RAM notes under M1–M3.
