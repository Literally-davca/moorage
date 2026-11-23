# Feature R7: stack templates

Read `prompts/roadmap-agent-common.md` first; it applies fully. Already in the code: R1 audit log (`auditActions`), R2–R6.

## Goal
Deploy common apps quickly from a catalogue of compose templates, as in Coolify: pick "Postgres", fill in a few values, get a working stack with generated passwords. There is **no central catalogue run by us**:
- templates come from a few bundled examples;
- from a local folder the admin controls;
- from template indexes the admin adds by HTTPS URL.

## Required design
- **Template format** (document it in your report; the lead writes the README). One template = a folder with:
  - `compose.yaml` and an optional `.env` template, both using `${VAR}` placeholders;
  - `template.yaml`:
    - id, name, description, category, website URL;
    - `variables`: name, label, description, default, `secret: true|false`, `generate: password|hex32|uuid` (crypto/rand), `required`, a simple `pattern`;
    - optional `notes` shown after deploy (e.g. "open http://host:8080").
- **Sources:**
  1. **Bundled:** 6–8 small, well-known, MIT/Apache/BSD-friendly images, embedded with `go:embed`. For example: whoami, nginx static site, PostgreSQL, Redis, Uptime-style test target, Gitea, Vaultwarden, Adminer. Pick ones you can verify start with defaults; pin major tags, not `latest`.
  2. **Local folder:** `MOORAGE_TEMPLATES_DIR` (default `<data dir>/templates`). One subfolder per template. Users can `git clone` a community repo there themselves; **Moorage runs no git**.
  3. **Remote indexes:** admins add HTTPS URLs in the UI.
     - Each URL points to an `index.json` that lists templates with relative URLs of their files, plus a format version.
     - The hub fetches with limits: HTTPS only; 1 MiB per file; 200 templates per index; 20 s timeout; no redirects to http; no private-IP targets unless the admin ticks "allow local network".
     - Results are cached in SQLite (refresh on demand and daily). A failing index keeps its last good copy and shows the error.
     - A template from a remote index is marked as such in the UI ("from <host>").
- **Safety (write it as [A] decisions in your report):**
  - a template is just a compose file the admin reviews: the deploy flow **always shows the final compose and .env before creating** the stack, with secrets masked but revealable;
  - no template can run code on the hub;
  - placeholders are only substituted with the admin's values or generated secrets, never evaluated;
  - a template that tries `privileged: true`, host network, the docker socket mount or `pid: host` gets a visible warning (not a block).
- **Deploy flow** (on the selected host):
  1. pick a template;
  2. stack name (validated like today) and variables (generated secrets pre-filled);
  3. review the compose/.env;
  4. create the stack through the **existing create path** (git version, audit `stack.create`), then optionally deploy (the existing up action, op panel).

  The template id and version are stored in the stack's first version message ("Created from template postgres (bundled)"). No new hidden state per stack.
- **API:**
  - `GET /api/templates` (all sources merged, with the source and any problems);
  - `GET /api/templates/{source}/{id}` (files + variables);
  - `POST /api/templates/render` (variables → final compose/.env; no side effects; `auditSkip`);
  - CRUD for remote index URLs (admin, audit map).

  Viewers can browse templates but not deploy.
- **UI:**
  - a "New stack → From template" entry next to the existing "New stack";
  - a catalogue with search and category chips (cards with name, description, source);
  - the variables form;
  - the review step (reuse the compose editor read-only);
  - the create (+ deploy) step with the op panel;
  - "Template sources" management for admins.
  - Must work at 375 px.

## Tests (at least)
- Parsing and validation of `template.yaml` (bad pattern, missing required, unknown generate kind).
- Substitution: only declared variables, `$$` escapes kept, undeclared `${FOO}` left untouched (compose's own env interpolation), generated secrets are unique per render and have the right length/alphabet.
- Remote index:
  - https only;
  - size limits;
  - redirect to http refused;
  - private IP refused unless allowed (test with `httptest` TLS server + a resolver hook);
  - a failing refresh keeps the cache.
- Local folder: a broken template is listed with its error, not hidden, and doesn't break the list.
- Warnings for privileged / socket mount / host network.
- The create path: a stack created from a template has a version with the template note and an audit row; render has no audit row.
- Every bundled template renders and passes the existing compose validation.

## Out of scope
- A hosted Moorage catalogue.
- Template updates for already created stacks.
- Paid templates.
- Executing scripts from templates.
