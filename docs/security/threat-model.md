# Moorage threat model

This document describes what Moorage protects, who it protects it from, and which
automated tests keep each protection in place. Every test named here exists in the
repository; run one with `go test ./internal/<package> -run '^TestName$'`.

Reporting a vulnerability: see [SECURITY.md](../../SECURITY.md).

## What Moorage is, in security terms

Moorage is a self-hosted web app that manages Docker Compose stacks on one or more hosts.
It runs with access to the Docker socket, so **an admin of Moorage is root on every
managed host**. The design goal is not to make that access harmless; it is to make sure
that only the right people get it, that everything they do is recorded, and that the parts
of Moorage reachable by others (status pages, agents, the MCP endpoint, outbound requests)
can't be turned into that access.

### Assets

| Asset | Where it lives |
| --- | --- |
| Control of Docker on each host | the Docker socket of the hub and of every agent |
| Compose files and `.env` secrets, including their history | stacks folder, `<data>/history.git` |
| Accounts, sessions, TOTP secrets, passkeys, API and MCP tokens | `<data>/moorage.db` (session tokens and API tokens stored as SHA-256 only) |
| Hub CA and agent certificates | `<data>/ca`, agent data folders |
| Notification channel secrets | `<data>/moorage.db` |
| The integrity of Moorage itself | release binaries and images, the self-updater |

### Actors

- **Admin** – full control; trusted.
- **Viewer** – may read stacks, `.env`, logs and checks; must not change anything.
- **Anonymous visitor** of the admin UI, a public status page or the agent port.
- **A malicious website** visited by a signed-in admin (CSRF, cross-site WebSocket hijacking).
- **A compromised container** or a malicious image on a managed host.
- **A compromised agent host**, or a compromised hub as seen by an agent.
- **An AI assistant** connected over MCP, possibly steered by prompt injection.
- **A supply-chain attacker** who tampers with a release in transit or on a mirror.

### Out of scope

- An attacker who already has an admin session, the data volume or root on the hub.
  Moorage limits what such an attacker leaves unrecorded, not what they can do.
- Weaknesses of Docker, the kernel or the container images Moorage runs for you.
- Denial of service by a client that can already reach the admin UI.

## Protections and the tests that guard them

### 1. The Docker socket

Moorage needs the socket; whoever controls Moorage controls the host. Protections:

- Every request is authenticated (session cookie, API token or the local `0600` socket),
  and every change needs the admin role. Viewers can't change anything.
  - `internal/api/api_test.go`: `TestGuards`
  - `internal/api/users_test.go`: `TestViewerForbiddenOnUserRoutes`, `TestRoleDisableDeleteRevokeSessions`
- Every mutating API route writes an audit row (who, when, from where, outcome) and a new
  route can't skip it; request bodies (passwords, compose files, tokens) never land in it.
  - `internal/api/audit_test.go`: `TestAuditEveryMutatingRouteMapped`, `TestAuditMiddlewareOutcomesAndNoBody`
  - `internal/api/users_test.go`: `TestUserAuditRowsHaveNoSecrets`
- The web terminal is off unless the host's environment turns it on, is admin-only, opens
  only with a one-time ticket, runs only `sh`, `bash` or `ash` in a container of the stack
  (checked by the agent itself), and never records what is typed.
  - `internal/api/wslive_test.go`: `TestTerminalTicketRules`
  - `internal/api/terminal_test.go`: `TestTerminalAccessGuardsAndOlderAgent`, `TestTerminalAuditAndZeroCommandLeak`, `TestTerminalSessionRevocation`
- A read-only agent refuses every change itself, not only in the hub's UI.
  - `internal/tunnel/tunnel_test.go`: `TestPairConnectAndProxy`
  - `internal/updater/updater_test.go`: `TestReadOnlyAndUnsupportedAgent`
- Moorage's own stack can't be stopped, deleted or saved without its socket and data
  mounts, so a click can't cut Moorage off.
  - `internal/engine/self_test.go`: `TestSelfStackRefusesStopDownDelete`, `TestSelfStackValidation`
- Templates and the compose linter warn about services that mount the Docker socket, run
  privileged or share the host's network or processes.
  - `internal/templates/template_test.go`: `TestWarnings`
- The **Docker socket proxy** add-on lets other containers (and Moorage's agents) use a
  filtered socket instead of the raw one. *Not covered by an automated test.*

Residual risk: an admin session is root-equivalent. Put Moorage behind HTTPS and a VPN or
an SSO proxy, and keep the number of admins small.

### 2. Sign-in, sessions and passkeys

- Session cookie `HttpOnly`, `SameSite=Strict`, `Secure` behind HTTPS; strict CSP, no
  framing; every write needs the `X-Moorage` header (CSRF guard).
  - `internal/api/wslive_test.go`: `TestSessionCookieAndSecurityHeaders`
  - `internal/api/api_test.go`: `TestGuards`
- Every local account has a password (argon2id) and TOTP; codes can't be reused; sign-in is
  rate limited. At least one active local admin with TOTP always remains (break-glass when
  the identity provider is down).
  - `internal/api/audit_test.go`: `TestAuditLoginLogout`
  - `internal/api/users_test.go`: `TestInviteViewerNeedsTOTP`, `TestInviteExpiredRevokedAndLimiter`, `TestLastLocalAdminProtection`, `TestDisabledLocalLogin`, `TestAccountPasswordTOTPAndSessions`, `TestResetRevokesSessionsOnFinish`
- A revoked sign-in (sign-out, password change, disabled user, role change, revoked token)
  closes open terminals and log streams at once.
  - `internal/api/wslive_test.go`: `TestLiveConnectionsClosedOnRevocation`, `TestRevalidateLiveClosesOnlyRevoked`
- Forward-auth headers count only from a trusted proxy address; OIDC uses PKCE, state and
  nonce and checks the ID token.
  - `internal/api/users_test.go`: `TestForwardAuth`, `TestForwardAuthIgnoredOutsideTheApp`
  - `internal/sso/oidc_test.go`: `TestProxyTrust`, `TestOIDCHappyPathWithPKCE`, `TestOIDCRejectsBadTokens`

**Passkeys (WebAuthn)** are a second factor next to TOTP, never instead of it:

- A passkey replaces the TOTP code at sign-in (password + passkey). Passwordless sign-in
  works only for a passkey its owner marked as passwordless, and only when the
  authenticator verified the user (PIN or biometrics). OIDC and proxy users have no
  passkeys; their factors live at the identity provider.
- Adding a passkey needs the password **and** a current TOTP code, so a stolen session
  can't add a persistent factor. Removing one needs only the session (it lowers access).
- TOTP is never removed, so losing every passkey locks no one out, and the break-glass
  rule is unchanged. Finishing an admin's **reset link** removes the user's passkeys
  together with the old password and TOTP.
- Sign-in challenges are discoverable (no username first), so they don't reveal which
  accounts or passkeys exist; each challenge is kept only in memory, expires after five
  minutes and can be answered once. A sign counter that goes back (a cloned authenticator)
  refuses the sign-in.
- Adding, changing and removing passkeys and every passkey sign-in are audited
  (`account.passkey_*`, `auth.login` / `auth.login_failed` with target `passkey` or
  `passwordless passkey`).

**Relying party rule.** The WebAuthn relying party comes only from the configured public
URL (`MOORAGE_PUBLIC_URL` or **Settings → General**), never from the request's `Host`
header:

1. The public URL must be set; without it passkeys are off and the UI says why.
2. It must use `https://`, or `http://` only for `localhost` and `*.localhost`.
3. Its host must be a domain name; IP addresses can't be WebAuthn relying parties.
4. The relying party id is the host name without port; the only accepted origin is the
   scheme, host and port of the public URL (a path is ignored).

So an assertion made at any other origin (a phishing proxy, a look-alike host) is refused,
and a request can't choose the site its challenge is bound to. Changing the host of the
public URL makes existing passkeys unusable: TOTP keeps working and the user adds them
again (the Account page marks passkeys registered for another host).

- `internal/api/passkeys_test.go`: `TestPasskeySecondFactorAndPasswordless`, `TestPasskeyBoundToItsAccount`, `TestPasskeyPasswordlessNeedsUserVerification`, `TestPasskeysNeedPublicURL`, `TestPasskeyWrongOriginRefused`, `TestResetLinkRemovesPasskeys`, `TestRelyingPartyFor`

### 3. Cross-site WebSocket hijacking (CSWSH)

Browsers send cookies with WebSocket upgrades from any site, so a malicious page could
open a log stream or terminal as the signed-in admin. Every WebSocket goes through one
function that checks `Origin`: it must match the request `Host`, the host of the public
URL, or `X-Forwarded-Host` from a trusted proxy. No `*`, no `InsecureSkipVerify`, no
direct `websocket.Accept`. Terminals additionally need a one-time ticket, so the cookie
alone never opens one.

- `internal/api/wslive_test.go`: `TestEveryWebSocketAcceptChecksOrigin`, `TestTerminalOriginBehindProxy`, `TestTerminalTicketRules`
- `internal/api/terminal_test.go`: `TestTerminalAccessGuardsAndOlderAgent` (foreign `Origin` refused)

### 4. Server-side request forgery (SSRF): `netguard`

Checks, notifications, registry lookups and template indexes make the server connect to
addresses a user typed. `internal/netguard` refuses link-local ranges (cloud metadata at
169.254.169.254 and friends), known metadata addresses, multicast and unspecified
addresses on the **address actually dialed**, after DNS resolution, so DNS rebinding
doesn't get around it. Private home-network ranges stay allowed on purpose: monitoring a
homelab needs them. A single check may opt in to link-local addresses.

- `internal/monitor/monitor_test.go`: `TestGuardBlocksMetadata`
- `internal/notify/notify_test.go`: `TestChannels` (webhook to 169.254.169.254 refused)
- `internal/engine/images_test.go`: `TestSelectNewerTagsFromRegistry` (registry at a metadata IP refused)
- `internal/templates/remote_test.go`: `TestFetchMetadataAlwaysBlocked`, `TestFetchRefusals`

Residual risk: an admin can still point a check at any private address; that is the point
of a monitoring tool. Viewers can't create checks.

### 5. Path traversal

Stack names, history versions, template sources and ids are user input that ends up in
file paths. Stack names must match `^[a-z0-9][a-z0-9_-]{0,62}$` before any path is built;
history refs, template sources and template ids are validated the same way.

- `internal/engine/engine_test.go`: `TestStackNameValidation` (`../etc`, `a/b` refused)
- `internal/engine/history_test.go`: `TestHistorySaveDiffRestore` (version `../x` refused)
- `internal/api/deps_test.go`: `TestDepsAPI` (stack `../x` refused)
- `internal/api/templates_test.go`: `TestTemplatesBrowseRenderCreate` (template source `../etc` refused)
- `internal/templates/template_test.go`: `TestParseMeta` (template id `../App` refused)

Compose folders outside the stacks folder (`MOORAGE_EXTRA_STACKS_DIRS`, or the working
directory of a compose project Moorage didn't start) are used only after the same name
validation, and a project's working directory only when it is an absolute, clean path
without `..` and without symlinks, outside system folders (`/etc`, `/usr`, `/var/lib/docker`
and the like), holds a compose file and is writable. Starting, stopping or restarting a compose project
Moorage doesn't manage needs `MOORAGE_MANAGE_EXTERNAL`. *These working-directory rules (`internal/engine/external.go`)
aren't covered by a dedicated test yet.*

### 6. Public status pages

Status pages are the only part meant for anonymous visitors. They show only the public
display names chosen for them: internal check names, targets, host ids, stack names, probe
messages, incident causes and log lines never reach the HTML, the RSS feed or the preview.
They set no cookies, have their own strict CSP, refuse `POST`, and a custom
status domain serves nothing but the page. `MOORAGE_STATUS_LISTEN` serves status pages
only, never the admin UI, the API or `/metrics`.

- `internal/api/status_test.go`: `TestStatusPageNoDataLeakAndEscaping`, `TestStatusPageRoutingDomainsListenerAndHeaders`, `TestStatusPageDraftPreviewDoesNotSave`, `TestStatusPageValidation`
- `internal/api/metrics_test.go`: `TestMetricsTokenAndListeners`

### 7. Agents

The hub and agents talk over mutual TLS with the hub's own CA. The join token carries the
CA fingerprint, so an agent never sends its token to the wrong hub; the agent's private key
never leaves its host. An agent exposes stack operations, never the Docker API. Removing a
host on the hub disconnects it and its certificate stops working. An agent verifies
self-update artifacts with the release key built into its own binary, so even a
compromised hub can't install anything else on it.

- `internal/tunnel/tunnel_test.go`: `TestEnrollRejectsWrongHub`, `TestPairConnectAndProxy`
- `internal/tunnel/pairing_test.go`: `TestRedeemJoinTokenRetryUntilFirstConnect`, `TestReenrollAfterRemovalWithNewToken`
- `internal/tunnel/watchdog_test.go`: `TestWatchdogPeerMTLSRejectsForeignCAAndRemovedAgent`
- `internal/selfupdate/service_test.go`: `TestAgentRefusesUnsignedAndMismatchedArtifactsEvenFromHub`

Residual risk: a compromised hub controls every agent's stacks (that is what a hub is for),
but not the agents' Docker APIs beyond stack operations, and not their binaries.

### 8. MCP server (AI assistants)

The MCP endpoint (`/mcp`) exists only while the **MCP Server** add-on is on (404
otherwise). It needs its own bearer token with an expiry and a rate limit, scoped to hosts
and stack patterns. `.env` values are masked before they reach the assistant, Moorage's
own add-ons stack and disallowed actions (such as `down`) are refused, and a token can require that every
change becomes a **draft** an admin approves in the UI. Prompt injection inside logs or
compose files therefore can't do more than the token's scope allows, and with approval on,
nothing changes without a human.

- `internal/api/addons_wave_test.go`: `TestMCPServerSafetyAndDrafts`

### 9. Releases and supply chain

- Release binaries and `images.json` (the image digests) are listed in `SHA256SUMS`, signed
  with the project's Ed25519 key. Every node checks that signature with the key built into
  its own binary before installing, and refuses downgrades.
  - `internal/selfupdate/verify_test.go`: `TestVerifySignaturesAndChecksums`
  - `internal/selfupdate/service_test.go`: `TestAgentRefusesUnsignedAndMismatchedArtifactsEvenFromHub`
- Since M18B every release also carries SBOMs (SPDX and CycloneDX, generated with Syft) for
  the binaries and for each platform of the image, a keyless **cosign** signature of the
  image (GitHub OIDC, Sigstore), SBOM attestations on the image and GitHub build
  provenance attestations for the binaries and the image. The release notes show the
  `cosign verify` and `gh attestation verify` commands. These are produced in their own
  workflow job that never sees the Ed25519 key; the key's job runs only the Go standard
  library. *The workflow itself isn't covered by a Go test; it runs on every release.*
- Third-party GitHub Actions are pinned to full commit SHAs.
- The UI loads nothing from the internet; npm install scripts don't run in CI.

## Operator checklist

- Serve Moorage over HTTPS, behind a VPN or an SSO proxy; never expose the agent port
  (`MOORAGE_AGENT_LISTEN`) wider than your agents need.
- Set `MOORAGE_PUBLIC_URL` to the address you open Moorage at (needed for passkeys, OIDC
  and the WebSocket origin rule behind proxies).
- Treat the data volume like a secret: it holds `.env` history, the CA and the database.
- Keep at least two local admins with TOTP, and give everyone else the viewer role.
- Put public status pages on `MOORAGE_STATUS_LISTEN` or their own domain.
- Turn the web terminal and the MCP add-on on only where you need them; give MCP tokens
  the narrowest scope and keep draft approval on.
