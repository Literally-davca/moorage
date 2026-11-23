# Task: implement milestone M8 "Image auto-update" in Moorage

You are a senior Go + Svelte engineer working directly in the repository `E:\Apps\moorage` (WSL path `/mnt/e/Apps/moorage`). You do the main implementation. Another engineer (Claude) will review your work afterwards, run it against a real Docker daemon and a real local registry, and fix mistakes. Favour clear, conventional code over clever code, and report honestly what you did not finish or could not verify.

This milestone is the first one where Moorage **changes running services on its own**, without a person clicking. Every bug here either restarts something the user didn't want restarted, sends the same notice again and again, or leaves a stack broken with no way back. Section 6 lists the cases that must hold; design for them first.

## 1. Project context

Moorage is a self-hosted, MIT-licensed single Go binary that combines what Dockge (compose stacks), Dozzle (live logs) and Uptime Kuma (uptime checks) do. Milestones M1–M7 are done and reviewed. Read these before you start:
- `PLAN.md`:
  - "Rozhodnutí" (security decisions: no external processes, agent = socket proxy with an allowlist, read-only agents);
  - "Stav milníků" M1 (compose operations), M2 (git history of stacks), M3 (hub/agent tunnel), M4 (checks, deploy silence, alert batching, notifications), M7 (latest review);
  - "Náklady" (RAM limit).
- `README.md` – modes, config table, security notes.
- `.interface-design/system.md` – UI direction. The Soundings strip already reserves deploy anchors for "rollback and auto-update"; the Logbook is the stack's timeline.

Stack:
- Go 1.27, module `moorage`. Single binary with modes `all-in-one`, `hub`, `agent` (`cmd/moorage/main.go`).
- Docker Compose v5.8 Go SDK (no CLI), moby client `github.com/moby/moby/client` v0.6.3.
- SQLite via `modernc.org/sqlite`. Migrations are a list in `internal/store/store.go`; append, never edit old ones.
- UI: Svelte 5 (runes) + TypeScript in `web/`, embedded with `go:embed`.

Relevant code today:

| Path | What it is |
|---|---|
| `internal/engine/ops.go` | `RunAction(name, action)` runs a compose operation in the background with a per-stack lock (`ErrBusy`) and live output (`Op`). Actions: `up`, `down`, `stop`, `restart`, `pull`, `update` (= pull, then up). |
| `internal/engine/containers.go` | `Container` (service, image, state…), `projectContainers`. |
| `internal/engine/history.go` | bare git repo `<data>/history.git`: every save/rollback/external change of `compose.yaml`/`.env` is a version (`record`, author, kind). `RestoreStack` writes an old version back. |
| `internal/engine/stacks.go` | stack listing, `Save` (validates with compose-go, writes files, records a version). |
| `internal/host/host.go` | `Host` interface – what the hub can do on a host (local engine or remote agent). Add new methods here. |
| `internal/host/agentapi.go` | the agent's HTTP API = the built-in socket proxy (fixed allowlist of operations, read-only enforced **on the agent**). `Remote` = the hub's client. A missing endpoint on an older agent returns 404. |
| `internal/monitor/monitor.go` | `StackEvent(hostID, stack, kind, actor, detail)`: kinds in `DeployKinds` (incl. `update`, `rollback`) silence the stack's checks for 3 min and become anchors on the strip. Alert batching and `compose()` message wording. |
| `internal/notify/notify.go` | channels (webhook, ntfy, Telegram, Discord, SMTP), `Send`; all outgoing connections go through `internal/netguard`. |
| `internal/api/api.go`, `internal/api/monitor.go` | HTTP API: `s.user`/`s.admin`, CSRF header; `runAction` records a stack event. |
| `web/src/views/StackDetail.svelte`, `HistoryView.svelte`, `Shell.svelte` | stack page, Logbook/history tab, sidebar. |

## 2. Goal of M8

From `PLAN.md`: check for **new image digests and tags**, then either **notify** or **automatically pull + up**, **per stack**, recorded in history and with **alerts silenced** during the update.

Split into two parts. Finish and test part A before you start part B. If you run out of time, deliver A complete and say so.
- **Part A – same tag, new digest** (`nginx:1.27` or `:latest` was rebuilt and pushed again). Notify or apply automatically.
- **Part B – newer version tag** (`traefik/whoami:v1.10` → `v1.11` exists). **Notify only**, and offer a one-click change in the UI that edits `compose.yaml`. Never applied automatically in M8.

## 3. Required design

### 3.1 Where things run
- **The agent checks** (it has the Docker daemon and the registry credentials), **the hub decides and remembers** (schedule, policies, results, notices).
- Add `Host` methods and agent endpoints for:
  - **`CheckImages(stack)`** – per service:
    - the image reference from the compose project;
    - the local image ID and repo digest of the tag;
    - the image ID the running container uses;
    - the remote digest;
    - a status (3.2).
  - **`StackImages(stack)`** – per service: the image ID the container runs and its repo digests. Call it before and after an update (3.4).
  - **`RollbackImages(stack, map service → old image ID)`** – see 3.5.
- **Read-only agents** (`MOORAGE_READ_ONLY`):
  - allow `CheckImages` and `StackImages`;
  - refuse `RollbackImages` and every update action.
  - The hub shows such stacks as "notify only".
- **Older agents** (404 on the new endpoints): the hub shows "Update the agent to check images" and never retries in a loop.

### 3.2 Part A – digest check
- Use the daemon's `DistributionInspect` for the remote digest and `ImageInspect` for the local side. Compare the remote descriptor digest with the local `RepoDigests` entry of the same repository; with multi-arch images both are the index digest.
- **Statuses** (fixed set, shown in the UI):

  | Status | Meaning |
  |---|---|
  | `up-to-date` | – |
  | `update-available` | remote digest differs from local |
  | `pulled-not-applied` | the tag was pulled outside Moorage, so the local digest equals the remote, but the container still runs an older image ID |
  | `pinned` | the reference contains `@sha256:` – never updated |
  | `built` | service has `build:` – skipped |
  | `local-only` | the image has no repo digest; it was built or loaded locally |
  | `unreachable` | registry error. Store a short reason, e.g. `unauthorized`, `not found`, `rate limited`, `timeout` |
  | `unsupported` | the agent is too old |

- **Credentials:**
  - read only the `auths` entries of the Docker config file (`$DOCKER_CONFIG/config.json` or `~/.docker/config.json`) and pass them as registry auth;
  - **never run credential helpers** (`credsStore`, `credHelpers`). Moorage runs no external processes (PLAN, Kuma's Apprise RCE). For such registries report `unreachable: credential helper not supported` and document how to mount an `auths` entry instead.
- **Registry load (Docker Hub rate limits):**
  - deduplicate references per host and run: 20 stacks using `postgres:16` = 1 request;
  - check interval default **6 h**, allowed 1 h–7 days, with random jitter (±10 %);
  - "Check now" per stack or for everything, at most once per minute per stack;
  - per-request timeout 20 s, at most 4 requests in parallel per host.

### 3.3 Part B – newer version tags (notify only)
- Only for tags that look like versions: `^v?\d+(\.\d+){0,2}(-[a-z0-9.]+)?$`, e.g. `1.27`, `v1.10`, `16.4-alpine`. Never for `latest` or arbitrary names.
- A **candidate** must have:
  - the same prefix (`v` or none);
  - the same number of components;
  - the same suffix (`-alpine` stays `-alpine`);
  - a higher version.
- Report:
  - the highest candidate within the **same major**;
  - separately, the highest **newer major** if there is one. Example: `v1.10` → "v1.11 available · v2.1 is a new major version".
- **Listing tags:** the agent calls the registry HTTP API v2 itself (`GET /v2/<repo>/tags/list`, `n`, `Link` paging, cap 10 000 tags):
  - follow the `WWW-Authenticate: Bearer realm=…,service=…,scope=…` token flow, anonymously or with the `auths` credentials from 3.2;
  - map Docker Hub names correctly (`nginx` → `registry-1.docker.io/library/nginx`);
  - use the `netguard` dialer (an image reference is user input, e.g. `169.254.169.254/x:1`);
  - timeout 20 s;
  - no new dependencies for this – plain `net/http`.
- **Applying a tag change (UI button "Change to v1.11"):**
  - rewrite the `image:` line of that service in `compose.yaml` with a **targeted text edit**: only when the file contains exactly that service's `image: <old ref>` value once, keeping quotes, comments and formatting;
  - if the value comes from a variable (`${TAG}`, `.env`) or appears more than once, refuse with "Edit compose.yaml manually" and open the editor;
  - save through the existing save path, so it is validated and becomes a **git version** with message `Change web image traefik/whoami v1.10 → v1.11` and the user as author;
  - then run the normal `update` action. The user's existing rollback (M2 history) covers it.

### 3.4 Policies, schedule and automatic apply (Part A)
- **Per-stack policy:**
  - `off`;
  - `notify` – **default** for every stack, also for existing ones after the migration;
  - `auto` – **only for managed, writable stacks**.
  - Stored on the hub per host + stack. A stack renamed or deleted on the host simply drops out of results.
- **Global settings (hub, admin):**
  - enabled;
  - check interval;
  - an **apply window** (e.g. 03:00–05:00, in the hub's time zone; import `time/tzdata` so `TZ` works in the `FROM scratch` image, and show the zone in the UI);
  - the notification channels for update notices.
- **Automatic apply** of `update-available` / `pulled-not-applied` on an `auto` stack, only inside the window. Sequence:
  1. Skip if the stack is busy (`ErrBusy`), stopped (no running containers) or the host is offline; try in the next window.
  2. `StackImages` → remember the old image ID per service.
  3. `StackEvent(host, stack, "update", "auto-update", "<summary>")`. This silences checks for 3 min and adds an anchor and a Logbook entry.
  4. Run the `update` action with author `auto-update`; follow the op to the end.
  5. `StackImages` again → store an **update record** (3.5) with old and new image IDs and digests per service.
  6. Notify (3.6).
- One stack at a time per host. Never more than one automatic update per stack per window.
- **[A, decided]** A digest update does **not** create a git version. `compose.yaml` and `.env` don't change, so an empty commit would clutter the history, and restoring it would not bring the old image back. The update record + stack event (Logbook, strip anchor) are its history. Tag changes from part B do change the file and are git versions. Write this decision into PLAN.

### 3.5 Update records and rollback
- **Hub table `image_updates`:**
  - id, host, stack, started/finished time, actor (`auto-update` or user);
  - result (`ok`, `pull-failed`, `up-failed`, `rolled-back`);
  - per service: ref, old image ID + digest, new image ID + digest;
  - the last 20 lines of the operation output on failure.
  - Keep 90 days.
- **"Roll back update"** (admin, UI on the stack and in the notice link). `RollbackImages` on the agent:
  1. for each service, check that the old image ID still exists locally (`ImageInspect`). If it was pruned, pull `repo@<old digest>`;
  2. `ImageTag(oldImageID, ref)`;
  3. run `up` without pulling, so compose recreates the containers on the old image;
  4. record a `rollback` stack event and set the record's result to `rolled-back`.
- **After a rollback, set the stack's policy to `notify`** and remember the rolled-back remote digest as **skipped**. Otherwise the next window would install the same broken image again. A newer digest than the skipped one is offered again.
- **Pull failure:** nothing changed. Notify once per remote digest, not every interval.
- **Up failure:** the stack may be degraded. Notify with the output tail and the rollback link. **No automatic rollback in M8** (ROADMAP: "auto-rollback when checks fail after an update").

### 3.6 Notifications (fixed wording, no templates)
- **Available updates** (policy `notify`, or `auto` outside the window): **one message per check run**, listing only updates not announced before (remember the announced remote digest / candidate tag per service). Example:
  - title `2 image updates available`;
  - text lines `whoami · web: traefik/whoami:v1.10 has a new build` and `whoami · web: v1.11 available`;
  - then the public URL link.
- **Applied:** `Updated whoami: web traefik/whoami:v1.10 (sha256:1a2b3c4 → 5d6e7f8)`.
- **Failed:** `Update of whoami failed: <first error line>` + last lines + "Roll back" link.
- Use the global update channels and the existing `notify.Send`. Batch per run like alerts.
- If no channel is set, results are only shown in the UI.

### 3.7 UI
- **Sidebar entry "Updates"** (after Status pages):
  - settings card: on/off, interval, apply window with time zone, channels;
  - a list of stacks with updates, grouped by host: stack, service, ref, status badge, new tag candidates, policy, last check, last update with result.
- **Stack detail, card "Images":**
  - per service: ref, status badge, "Check now";
  - policy selector Off / Notify / Auto. Auto is disabled with the reason for read-only and unmanaged stacks;
  - **Update now** (runs `update` with the user as author, same recording as auto);
  - tag candidates with **Change to …**;
  - last update record with **Roll back update**.
- **Logbook** shows update records (`auto-update` actor clearly marked). The strip already gets the anchor from the stack event.
- Follow `.interface-design/system.md`. Status badges always have text. Max one primary button per view. Works at phone width.

Svelte 5 pitfalls already hit in this repo:
- an `$effect` reading props or app state refreshed by polling must use `$derived` keys or `untrack`, or it loops and floods requests;
- an `$effect` must not return a Promise; wrap async code in a block.

### 3.8 Limits
- RAM limit **100 MB PSS** for all-in-one with the UI open. Today the hub uses ~18 MiB anonymous / ~48 MiB RSS; the binary sets a 48 MiB soft Go memory limit (`GOMEMLIMIT` overrides it). Tag lists can be large: stream-parse them and keep only parsed versions, never whole responses.
- No new dependencies unless unavoidable. Explain any you add.
- Out of scope:
  - automatic rollback on failing checks;
  - automatic tag changes;
  - vulnerability scanning;
  - pruning old images;
  - credential helpers;
  - Watchtower-style label configuration.

## 4. Working rules
- Do **not** commit, do **not** run `git init`, do **not** push. The repo is not under git yet.
- Do **not** stop or restart the Moorage dev processes. Claude owns them:
  - the dev hub on `127.0.0.1:5080` (UI), `:5081` (agent port), `:5085` (status pages);
  - three test agents `wd1`–`wd3` (ports 5082–5084, data in `~/moorage-dev/wd1..3`);
  - a webhook receiver on `127.0.0.1:18099`.
  Never run `pkill moorage`.
- Do **not** update, pull or recreate the dev stacks `whoami` and `crashy` in `~/moorage-dev/stacks`; the dev hub's checks watch them.
- For your own manual runs:
  - use other ports (e.g. 5090/5091) and a separate data and stacks directory under `~/moorage-dev/m8-*`;
  - for a realistic digest test, run a throwaway local registry (`docker run -d --name m8-registry -p 127.0.0.1:5000:5000 registry:2`);
  - push an image as `127.0.0.1:5000/m8/app:1.0`, deploy a stack using it, then push a different image under the same tag and check. Also push `1.1` and `2.0` for part B;
  - remove your registry and stacks when done.
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

## 5. Lessons from the M5–M7 reviews – avoid these
Each of your last three milestones passed its own tests, but the review found 4–9 real bugs. They came from cases the tests didn't model:
- duplicates after a reconnect or restart (M5, M6);
- in-memory state lost on restart (M5, M6);
- rounding and time-window edges (M5, M6, M7);
- **a "preview" that silently saved and published** (M7);
- **a public link that leaked the admin URL** (M7);
- **a database query on every request** (M7);
- several sources of the same event shown twice (M7).

For M8 the analogues are:
- **repeats:** the same "update available" notice every 6 h; the same digest auto-applied again after a rollback; a notice repeated after a hub restart (announced digests must be persisted);
- **unintended restarts:**
  - a stack updated outside the window, or while busy, or while it was stopped on purpose;
  - a `pinned` or `built` service pulled anyway;
  - a read-only agent updating;
  - "Check now" triggering an update;
- **broken way back:** rollback when the old image was pruned; rollback that pulls the tag again and gets the new image; policy not switched to `notify` after a rollback;
- **registry load:** the same reference checked once per stack instead of once per run; errors retried in a tight loop;
- **edits:**
  - the tag change rewriting the wrong service, a commented line, or a value that comes from `${VAR}`;
  - CRLF line endings broken.

Write a test for each.

## 6. Tests you must add
Put the registry and Docker calls behind small interfaces so the logic is testable without a network.

**Part A (pure functions and fakes):**
1. Status per service: up-to-date, update-available, pulled-not-applied, pinned, built, local-only, unreachable (each reason).
2. Deduplication: 3 stacks with the same reference → 1 remote lookup per run.
3. Decision function `ShouldApply(policy, status, window, now, busy, stopped, readOnly, skippedDigest, lastAutoUpdate)` table-tested. Include window edges (02:59:59, 03:00:00, 04:59:59, 05:00:00), a window over midnight (23:00–02:00), and a DST change day in `Europe/Prague`.
4. Notices: announced once per remote digest/tag, again for a newer one, not again after a hub restart (state reloaded from SQLite).
5. Auto-update sequence with a fake host: stack event recorded with kind `update` and actor `auto-update`, op followed to the end, update record with old/new IDs, exactly one notice.
6. Failures: pull failure → nothing recorded as applied, one notice per digest; up failure → record `up-failed` with output tail.
7. Rollback: old image present → tag + up without pull; old image pruned → pull by digest first; policy becomes `notify` and the digest is skipped; a newer digest is offered again.
8. Read-only agent: check works, update and rollback refused **by the agent**.

**Part B:**
1. Version parsing and candidate selection, including:
   - `v` prefix;
   - suffixes;
   - different component counts;
   - pre-release-looking tags (`1.28-rc1` is not a candidate for `1.27`);
   - `latest` ignored;
   - a newer major reported separately.
2. Tag listing against an `httptest` registry: bearer token flow, paging via `Link`, Docker Hub `library/` mapping, the 10 000 cap, a blocked metadata address via netguard.
3. Compose rewrite:
   - only the right service changes;
   - quotes and comments are kept;
   - CRLF files keep CRLF;
   - `${TAG}` and duplicate values are refused;
   - the result validates and becomes one git version with the right message and author.

All existing tests must still pass.

## 7. Documentation
- `PLAN.md`:
  - add `### M8 – hotovo (<date>)` under "Stav milníků", in Czech, in the same style as M4–M7: what works, decisions marked **[A]** with reasons (incl. "digest update ≠ git version" and "no credential helpers"), what was verified, known limits;
  - change "Další krok" to M9 (web terminal).
- `README.md`:
  - status line M8;
  - a feature bullet;
  - a short "Image updates" section: policies, window, what is never touched (pinned, built, local), Docker Hub rate limits, private registries (mount `auths`, no credential helpers), rollback.
- `ROADMAP.md`: add "auto-rollback when checks fail after an update" and "automatic tag changes (with rules)" as ideas.

## 8. Report back (your final message)
1. Files changed or added, one line each.
2. Design decisions you made that this prompt did not dictate, with reasons.
3. Exact result summary of `go vet`, `go test ./...` and `npm run check` (pass/fail counts). Do not claim a pass you did not see.
4. What you did not finish or could not verify, and anything you suspect is wrong. List especially any case from section 6 that you could not cover, and whether you ran the local-registry scenario from section 4.

## Appendix – current state (2026-11-22)
- M1–M7 are done and reviewed; see `PLAN.md` "M7 – hotovo" (incl. the review notes).
- RAM: hub ~18 MiB anonymous / ~48 MiB RSS with a 48 MiB soft memory limit; agents 15–20 MiB anonymous. The limit is 100 MB PSS.
- Docker 29.8 runs in WSL with internet access.
- Dev stacks in `~/moorage-dev/stacks`:
  - `whoami` (`traefik/whoami:v1.10` + `busybox:1.37`) – a real part B candidate; **read-only for you**;
  - `crashy` (`busybox:1.37`).
- Nothing is committed and the project is not a git repository yet.
- Known stale bits in `PLAN.md` are out of scope: header "návrh, čeká na schválení", the RAM estimate in "Technologie", old RAM notes under M1–M3.
