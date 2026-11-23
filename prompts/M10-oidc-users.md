# Milestone M10: user management + OIDC sign-in (+ forward-auth)

Read `prompts/roadmap-agent-common.md` first; it applies fully. Already in the code: M1–M9, R1–R8. In this brief, "R1 audit" means: every new mutating route goes into `auditActions` (`internal/api/audit.go`), and `TestAuditEveryMutatingRouteMapped` enforces it.

## Today
- `internal/auth`:
  - one admin is created at first-run setup (setup token in the log, argon2id password, TOTP mandatory);
  - `Login` = username + password + TOTP code;
  - sessions are hashed tokens in `sessions` with a 7-day TTL;
  - a per-IP limiter.
- Table `users`: `role IN ('admin','viewer')`, `active`. `store.SetRole` and `store.DeleteUser` exist but nothing uses them.
- There is **no API or UI** to add a second user. Viewers exist only as a role.

## Goal
1. **User management:** admins add, change and remove users; everyone manages their own account.
2. **OIDC sign-in** (Authentik, Keycloak, Zitadel, Google…) with role mapping from groups.
3. **Forward-auth:** a header set by a trusted reverse proxy (Authelia, Authentik outpost, oauth2-proxy). This is the M10 row in PLAN.md and is small once 1 and 2 exist.

## 1. User management
### Model
Add a migration (append only):
- `users.auth_source`: `local` | `oidc` | `proxy`;
- `users.disabled`;
- `users.created_by`;
- `users.last_login_at`;
- `users.display_name`, `users.email` (from OIDC claims, informational only);
- an `oidc_identities` table (or columns) keyed by **(issuer, subject)**. Never key by e-mail or username.

### Local users: invite links, the admin never knows a password
- An admin creates a user with username, role and optional display name. The result is a one-time **invite link** with a random token, stored hashed. It is valid 48 h and shown once, with a copy button.
- The invitee opens `/#/invite/<token>`, sets a password, and then:
  - an **admin** must enrol TOTP (same flow as setup);
  - a **viewer** may enrol TOTP optionally. Say in your report whether TOTP is still required for viewers today and keep that behaviour unless it's clearly wrong; justify.
- The user becomes active only after finishing. An unfinished invite can be revoked or re-issued.
- **Reset:** the admin issues a "reset link" with the same mechanics. It replaces password + TOTP on finish. Existing sessions are revoked when the reset finishes, not when it is issued.
- **Rate limit:** invite/reset endpoints share the per-IP limiter. A wrong or expired token gives one generic error.

### Admin actions (UI "Users", admin only)
- **List:** username, display name, source, role, status (active / invited / disabled) with text, last sign-in, created by.
- Change role, disable/enable, delete, issue reset (local users only), revoke all sessions.
- **Safety rules, enforced on the server:**
  - there is always **at least one active, enabled, local admin with TOTP** (break-glass if the IdP is down). Refuse any action that would remove the last one (demote, disable, delete, reset of yourself only if…; decide and test). Also refuse disabling OIDC/forward-auth config at startup if it would lock everyone out (see below);
  - an admin cannot delete or demote themself through the list (use another admin); say if you allow self-disable (don't);
  - role change, disable and delete **revoke that user's sessions immediately**. `SessionUser` must also check `disabled` and the current role on every request, never the role cached at login;
  - usernames are validated by the existing `usernameRe`, unique, case-insensitive.

### Own account (every user: "Account" page)
- Change password: needs the current password; revokes other sessions.
- Re-enrol TOTP: needs password + current code.
- List own sessions (created, last seen, IP, user agent, "this session") and sign out one or all others.
  - Add `last_seen_at` / `ip` / `user_agent` to sessions. Throttle the last-seen write to at most once per minute per session; don't write on every request.
- OIDC/proxy users see their source and have no password/TOTP controls.

### Audit (R1)
- New actions: `user.create`, `user.role`, `user.disable`, `user.enable`, `user.delete`, `user.reset`, `user.sessions_revoke`, `account.password`, `account.totp`, `account.session_revoke`, `auth.invite_accept`, `auth.oidc_login`, `auth.oidc_denied`, `auth.proxy_login`.
- Audit rows never contain tokens, passwords, codes or ID tokens.

## 2. OIDC
### Config (env, not UI)
Secrets stay out of SQLite and the UI.
- `MOORAGE_OIDC_ISSUER`, `MOORAGE_OIDC_CLIENT_ID`;
- `MOORAGE_OIDC_CLIENT_SECRET` or `MOORAGE_OIDC_CLIENT_SECRET_FILE`;
- `MOORAGE_PUBLIC_URL` (needed for the redirect URI `<public>/api/auth/oidc/callback`). Reuse it if something similar exists; check config;
- `MOORAGE_OIDC_SCOPES` (default `openid profile email groups`);
- `MOORAGE_OIDC_GROUPS_CLAIM` (default `groups`);
- `MOORAGE_OIDC_ADMIN_GROUPS`, `MOORAGE_OIDC_VIEWER_GROUPS` (comma lists);
- `MOORAGE_OIDC_DEFAULT_ROLE` (`none` default = refuse, or `viewer`);
- `MOORAGE_OIDC_LABEL` (button text, default "Sign in with SSO");
- `MOORAGE_OIDC_LOCAL_LOGIN` (`true` default; `false` hides the password form, but the break-glass local admin can still sign in at `/#/login?local=1`; document it).

Validate at startup:
- issuer is https (allow http only for `localhost`/127.0.0.1 for dev);
- the discovery document loads;
- the redirect URL is consistent.

A broken config must **not** stop Moorage from starting. Log it clearly, hide the button, and show the problem to admins in the Users page.

### Flow
- Authorization code + **PKCE (S256)**, `state` and `nonce`, stored in a short-lived (10 min) HttpOnly, SameSite=Lax cookie or a server-side table. One use only.
- Verify the ID token:
  - signature via JWKS (cache keys, refetch on unknown `kid`, rate-limited);
  - `iss`, `aud` (incl. `azp` when several audiences), `exp`/`iat` with small leeway, `nonce`;
  - allowed algs RS256/ES256 (and PS256 if free); never `none`/HS* with a public key.
- Groups:
  - from the ID token claim; if absent, from the userinfo endpoint;
  - support a string, a list, or a nested claim path like `realm_access.roles` (Keycloak). Decide and document.
- **Role on every login:** admin group > viewer group > default role > refuse with a clear page ("your account has no access to Moorage, ask an admin"). Refusals are audited as `auth.oidc_denied` with the subject and reason, not the token.
- **First login creates the user** (`auth_source=oidc`):
  - username from `preferred_username` → `email` local part → `sub`, sanitised to `usernameRe`;
  - on collision with an existing user, a numeric suffix. **Never** auto-link to an existing local account by name or e-mail, which would be an account takeover. Say in your report how an admin can link one if you implement it (optional; not required).
- An admin can't change an OIDC user's role in the UI (the IdP owns it). They can disable or delete the user; disabled wins over the IdP.
- An OIDC user's session lifetime = Moorage `SessionTTL`; document that IdP logout doesn't end it. RP-initiated logout is optional (if `end_session_endpoint` exists, the logout button may redirect; say what you did).
- **Dependencies:** prefer `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` (Apache-2.0/BSD, MIT-compatible, widely reviewed) over hand-rolled JWT/JWKS code. Report the binary size and RAM delta.

### UI
- Login page:
  - "Sign in with <label>" button (the one primary action when local login is off; secondary when both exist);
  - errors from the callback shown in plain words.
- Users page: an "SSO" panel for admins with read-only config status:
  - issuer, client id;
  - redirect URI to paste into the IdP, with a copy button;
  - group mapping;
  - last error.
- Must work at 375 px.

## 3. Forward-auth (trusted proxy header)
- `MOORAGE_TRUSTED_PROXIES` (CIDR list, empty = off).
- `MOORAGE_PROXY_USER_HEADER` (e.g. `Remote-User`), `MOORAGE_PROXY_GROUPS_HEADER` (e.g. `Remote-Groups`), with the same group → role mapping vars as OIDC, but separate names: `MOORAGE_PROXY_ADMIN_GROUPS` / `_VIEWER_GROUPS` / `_DEFAULT_ROLE`.
- The header is honoured **only** when the TCP peer (`r.RemoteAddr`, not `X-Forwarded-For`) is in the list. From anyone else the header is ignored and stripped. Check how `clientIP` works today and don't let it be spoofed.
- A valid header creates/updates a `proxy` user and a normal session cookie, so WebSockets and the rest work unchanged.
  - If the header later carries a different user, the old session is dropped.
  - If the header is missing, the existing session cookie rules apply.
- Never applies to the agent port, the status page, `/metrics` or public routes.

## Tests (at least)
- **Users:**
  - invite flow (admin needs TOTP; expired/used/revoked token; generic error; limiter);
  - reset revokes sessions on finish;
  - last local admin protection for demote/disable/delete/self-actions;
  - role change / disable / delete revoke sessions and `SessionUser` sees the new role on the next request;
  - viewer gets 403 on every user-management route (table test over the routes);
  - audit rows for each action, with no secrets in any audit row (scan the JSON for the token/password/code values used in the test).
- **OIDC** against a fake IdP (`httptest` TLS server: discovery, JWKS, token endpoint, userinfo):
  - happy path with PKCE checked by the fake;
  - bad signature, wrong `aud`, wrong `iss`, expired, wrong/missing nonce, `alg: none`, replayed state, unknown `kid` → refetch once;
  - group mapping incl. nested claim and userinfo fallback;
  - default role none → refused + audit;
  - first login creates user, second login updates role from groups;
  - username collision gets a suffix and never links;
  - disabled OIDC user refused;
  - broken issuer at startup → Moorage starts, button hidden, error exposed to admins.
- **Forward-auth:**
  - header from a trusted peer signs in;
  - from an untrusted peer it is ignored;
  - `X-Forwarded-For` spoofing doesn't help;
  - groups → role;
  - user switch drops the old session;
  - the agent/status/metrics routes ignore it.
- All existing tests pass (setup, login, TOTP replay, audit mapping).

## Report
- Decisions as **[A]** lines (I'll move them to PLAN.md).
- Env var table (for the README).
- IdP setup notes for Authentik and Keycloak (redirect URI, scopes, groups claim/mapper).
- Anything unverified.
- RAM / binary size delta.

## Out of scope
- SAML, LDAP.
- SCIM provisioning.
- Per-stack or per-host permissions (only `admin` / `viewer`).
- WebAuthn/passkeys (roadmap).
- E-mail delivery of invites (links are copied by hand).
