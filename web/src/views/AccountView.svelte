<script lang="ts">
  import { untrack } from 'svelte';
  import qrcode from 'qrcode-generator';
  import {
    api,
    type Account,
    type APIToken,
    type ComposeViewMode,
    type EnvViewMode,
    type ExperienceMode,
    type Role,
    type SessionInfo,
    type TOTPEnrolment,
    type UserPreferences,
  } from '../lib/api';
  import { app, isAdmin, userPrefs } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import PushToggle from '../lib/PushToggle.svelte';
  import Select from '../lib/Select.svelte';

  let acc = $state<Account | null>(null);
  let error = $state('');

  // Experience & editor preferences
  let prefs = $state<UserPreferences>(userPrefs());
  let prefBusy = $state(false);
  let prefMsg = $state('');
  let prefError = $state('');

  function applyPreset(m: 'beginner' | 'advanced') {
    const curOverrides = prefs.manualOverrides ?? {};
    prefs =
      m === 'beginner'
        ? {
            mode: 'beginner',
            composeView: 'form',
            envView: 'vars',
            linterShowTips: true,
            newStackDefault: 'templates',
            guidesMode: 'extended',
            confirmActions: 'all',
            onboardingDone: true,
            manualOverrides: curOverrides,
          }
        : {
            mode: 'advanced',
            composeView: 'yaml-comments',
            envView: 'raw',
            linterShowTips: false,
            newStackDefault: 'blank',
            guidesMode: 'compact',
            confirmActions: 'destructive',
            onboardingDone: true,
            manualOverrides: curOverrides,
          };
    prefMsg = '';
  }

  function detectMode(p: UserPreferences): ExperienceMode {
    const isBeg =
      p.composeView === 'form' &&
      p.envView === 'vars' &&
      p.linterShowTips &&
      p.newStackDefault === 'templates' &&
      p.guidesMode === 'extended' &&
      p.confirmActions === 'all';
    if (isBeg) return 'beginner';
    const isAdv =
      p.composeView === 'yaml-comments' &&
      p.envView === 'raw' &&
      !p.linterShowTips &&
      p.newStackDefault === 'blank' &&
      p.guidesMode === 'compact' &&
      p.confirmActions === 'destructive';
    if (isAdv) return 'advanced';
    return 'custom';
  }

  function patchPref(patch: Partial<UserPreferences>) {
    const next: UserPreferences = { ...prefs, ...patch, onboardingDone: true };
    next.mode = detectMode(next);
    prefs = next;
    prefMsg = '';
  }

  async function savePrefs() {
    prefBusy = true;
    prefMsg = prefError = '';
    try {
      const saved = await api.savePreferences({ ...prefs, onboardingDone: true });
      prefs = saved;
      if (app.user) app.user = { ...app.user, preferences: saved };
      prefMsg = 'Preferences saved.';
    } catch (e) {
      prefError = (e as Error).message;
    } finally {
      prefBusy = false;
    }
  }

  const overrideKeys = $derived(Object.keys(prefs.manualOverrides ?? {}).filter((k) => prefs.manualOverrides?.[k]));

  async function clearOverrides() {
    await patchPref({ manualOverrides: {} });
    await savePrefs();
  }

  // password
  let current = $state('');
  let next = $state('');
  let confirm = $state('');
  let pwBusy = $state(false);
  let pwMsg = $state('');
  let pwError = $state('');

  // TOTP re-enrolment
  let tPassword = $state('');
  let tCode = $state('');
  let enrol = $state<TOTPEnrolment | null>(null);
  let qr = $state('');
  let newCode = $state('');
  let tBusy = $state(false);
  let tMsg = $state('');
  let tError = $state('');

  let sessError = $state('');

  // API tokens (admins)
  let tokens = $state<APIToken[]>([]);
  let tokName = $state('');
  let tokRole = $state<Role>('admin');
  let tokScope = $state('admin');
  let tokDays = $state('0');
  let tokBusy = $state(false);
  let tokError = $state('');
  let tokSecret = $state('');
  let tokCopied = $state(false);

  async function load() {
    try {
      acc = await api.account();
      if (acc.user.role === 'admin') {
        const res = await api.apiTokens();
        tokens = res.tokens ?? [];
      }
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }
  $effect(() => {
    untrack(() => void load());
  });

  const local = $derived(acc?.user.source === 'local' || !acc?.user.source);

  async function changePassword(e: SubmitEvent) {
    e.preventDefault();
    pwMsg = pwError = '';
    if (next !== confirm) {
      pwError = "New passwords don't match.";
      return;
    }
    pwBusy = true;
    try {
      await api.changePassword(current, next);
      current = next = confirm = '';
      pwMsg = 'Password changed. Your other sessions were signed out.';
      await load();
    } catch (err) {
      pwError = (err as Error).message;
    } finally {
      pwBusy = false;
    }
  }

  async function startTOTP(e: SubmitEvent) {
    e.preventDefault();
    tMsg = tError = '';
    tBusy = true;
    try {
      enrol = await api.totpStart(tPassword, tCode);
      const q = qrcode(0, 'M');
      q.addData(enrol.otpauthUrl);
      q.make();
      qr = q.createDataURL(5, 2);
      tPassword = tCode = '';
    } catch (err) {
      tError = (err as Error).message;
      tCode = '';
    } finally {
      tBusy = false;
    }
  }

  async function finishTOTP(e: SubmitEvent) {
    e.preventDefault();
    if (!enrol) return;
    tError = '';
    tBusy = true;
    try {
      await api.totpFinish(enrol.secret, enrol.ticket, newCode);
      enrol = null;
      newCode = '';
      tMsg = 'Two-factor sign-in now uses your new authenticator. Your other sessions were signed out.';
      await load();
    } catch (err) {
      tError = (err as Error).message;
      newCode = '';
    } finally {
      tBusy = false;
    }
  }

  async function revoke(s: SessionInfo) {
    sessError = '';
    try {
      await api.revokeSession(s.id);
      await load();
    } catch (err) {
      sessError = (err as Error).message;
    }
  }

  async function revokeOthers() {
    sessError = '';
    try {
      await api.revokeOtherSessions();
      await load();
    } catch (err) {
      sessError = (err as Error).message;
    }
  }

  async function createToken(e: SubmitEvent) {
    e.preventDefault();
    tokError = '';
    tokSecret = '';
    tokCopied = false;
    tokBusy = true;
    try {
      const scopes = tokRole === 'viewer' ? ['read'] : [tokScope];
      const res = await api.createAPIToken({
        name: tokName.trim(),
        role: tokRole,
        scopes,
        expiresDays: Number(tokDays) || 0,
      });
      tokSecret = res.secret;
      tokName = '';
      tokens = [res.token, ...tokens];
    } catch (err) {
      tokError = (err as Error).message;
    } finally {
      tokBusy = false;
    }
  }

  async function revokeToken(t: APIToken) {
    tokError = '';
    try {
      await api.deleteAPIToken(t.id);
      tokens = tokens.filter((item) => item.id !== t.id);
    } catch (err) {
      tokError = (err as Error).message;
    }
  }

  // Host CLI help (docs/cli.md): the commands people copy onto the Docker host.
  const installCLI = 'curl -fsSL https://raw.githubusercontent.com/Literally-davca/moorage/main/scripts/install-cli.sh | sh';
  const cliSnippets = [
    { id: 'exec', label: "Inside the container, no install (in the folder of Moorage's compose.yaml)", cmd: 'docker compose exec moorage moorage stacks ls' },
    { id: 'wrapper', label: 'On the host: a moorage command that runs docker exec for you', cmd: installCLI },
    { id: 'native', label: 'Or the native binary from GitHub Releases', cmd: `${installCLI} -s -- --native` },
  ];
  let cliCopied = $state('');
  async function copyCLI(id: string, cmd: string) {
    if (!navigator.clipboard) return;
    await navigator.clipboard.writeText(cmd);
    cliCopied = id;
    setTimeout(() => {
      if (cliCopied === id) cliCopied = '';
    }, 2000);
  }

  async function copySecret() {
    if (!tokSecret || !navigator.clipboard) return;
    await navigator.clipboard.writeText(tokSecret);
    tokCopied = true;
    setTimeout(() => {
      tokCopied = false;
    }, 2000);
  }

  const when = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  const fmt = (iso?: string) => (iso ? when.format(new Date(iso)) : '');
  const sourceText: Record<string, string> = {
    oidc: 'You sign in through your identity provider (SSO). Your password and two-factor sign-in are managed there.',
    proxy: 'You sign in through the reverse proxy in front of Moorage. Your password is managed there.',
  };

  /** A short browser name from the user agent, never the whole string as the headline. */
  function browser(ua: string): string {
    if (!ua) return 'Unknown browser';
    for (const [k, n] of [['Edg/', 'Edge'], ['Firefox/', 'Firefox'], ['Chrome/', 'Chrome'], ['Safari/', 'Safari']] as const)
      if (ua.includes(k)) return n;
    return ua.slice(0, 40);
  }
</script>

<div class="page">
  <section class="card card-pad head">
    <h1 class="title">Account</h1>
    {#if acc}
      <p class="meta">
        Signed in as <strong>{acc.user.username}</strong>{acc.user.displayName ? ` (${acc.user.displayName})` : ''} ·
        {acc.user.role === 'admin' ? 'Admin' : 'Viewer'}{acc.email ? ` · ${acc.email}` : ''}
      </p>
      {#if !local}<p class="meta">{sourceText[acc.user.source ?? ''] ?? ''}</p>{/if}
    {/if}
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  <section class="card card-pad grid1" aria-label="Experience and editor preferences">
    <div class="pref-head">
      <div>
        <p class="section-title">Experience &amp; editor preferences</p>
        <p class="meta">Choose Beginner, Advanced, or customize each editor default individually.</p>
      </div>
      <span class="badge">{prefs.mode === 'custom' ? 'Custom' : prefs.mode === 'beginner' ? 'Beginner' : 'Advanced'}</span>
    </div>

    <div class="preset-row">
      <button type="button" class="btn" class:btn-primary={prefs.mode === 'beginner'} onclick={() => applyPreset('beginner')}>
        Beginner preset
      </button>
      <button type="button" class="btn" class:btn-primary={prefs.mode === 'advanced'} onclick={() => applyPreset('advanced')}>
        Advanced preset
      </button>
    </div>

    <div class="two">
      <div>
        <span class="label">Default Compose view</span>
        <Select
          label="Default Compose view"
          value={prefs.composeView}
          items={[
            { value: 'form', label: 'Form (lossless fields)' },
            { value: 'yaml-comments', label: 'YAML (with comments)' },
            { value: 'yaml-clean', label: 'YAML (clean, no comments/blanks)' },
          ]}
          onchange={(v) => patchPref({ composeView: v as ComposeViewMode })}
        />
      </div>
      <div>
        <span class="label">Default .env view</span>
        <Select
          label="Default .env view"
          value={prefs.envView}
          items={[
            { value: 'vars', label: 'Variables table' },
            { value: 'raw', label: 'Raw .env text' },
          ]}
          onchange={(v) => patchPref({ envView: v as EnvViewMode })}
        />
      </div>
    </div>

    <div class="two">
      <div>
        <span class="label">New stack starting point</span>
        <Select
          label="New stack starting point"
          value={prefs.newStackDefault}
          items={[
            { value: 'templates', label: 'Templates catalog first' },
            { value: 'blank', label: 'Blank Compose editor' },
          ]}
          onchange={(v) => patchPref({ newStackDefault: v as 'templates' | 'blank' })}
        />
      </div>
      <div>
        <span class="label">In-app guides &amp; setup hints</span>
        <Select
          label="In-app guides and hints"
          value={prefs.guidesMode}
          items={[
            { value: 'extended', label: 'Extended guides & checklists' },
            { value: 'compact', label: 'Compact hints' },
          ]}
          onchange={(v) => patchPref({ guidesMode: v as 'extended' | 'compact' })}
        />
      </div>
    </div>

    <div class="two">
      <div>
        <span class="label">Action confirmations</span>
        <Select
          label="Action confirmations"
          value={prefs.confirmActions}
          items={[
            { value: 'all', label: 'Confirm all actions (stop, restart, pull, delete)' },
            { value: 'destructive', label: 'Confirm destructive actions only' },
          ]}
          onchange={(v) => patchPref({ confirmActions: v as 'all' | 'destructive' })}
        />
      </div>
      <div class="chk-wrap">
        <span class="label">Linter tips</span>
        <label class="chk-line">
          <input
            type="checkbox"
            checked={prefs.linterShowTips}
            onchange={(e) => patchPref({ linterShowTips: e.currentTarget.checked })}
          />
          <span>Expand linter tips by default</span>
        </label>
      </div>
    </div>

    {#if overrideKeys.length > 0}
      <div class="override-bar">
        <span class="meta">Manual add-on overrides active for: {overrideKeys.join(', ')}</span>
        <button type="button" class="btn btn-ghost" disabled={prefBusy} onclick={clearOverrides}>Reset overrides</button>
      </div>
    {/if}

    {#if prefError}<p class="error-text" role="alert">{prefError}</p>{/if}
    {#if prefMsg}<p class="meta" role="status">{prefMsg}</p>{/if}
    <div class="row">
      <button type="button" class="btn btn-primary" aria-busy={prefBusy} disabled={prefBusy} onclick={savePrefs}>Save preferences</button>
    </div>
  </section>

  {#if acc && local}
    <section class="card card-pad grid1" aria-label="Password">
      <p class="section-title">Password</p>
      <form class="form" method="post" onsubmit={changePassword}>
        <input type="text" name="username" class="sr-only" autocomplete="username" value={acc.user.username} readonly tabindex="-1" aria-hidden="true" />
        <div>
          <label class="label" for="cur">Current password</label>
          <input id="cur" name="current-password" class="field" type="password" bind:value={current} autocomplete="current-password" required />
        </div>
        <div class="two">
          <div>
            <label class="label" for="np">New password</label>
            <input id="np" name="new-password" class="field" type="password" bind:value={next} autocomplete="new-password" minlength="12" required />
          </div>
          <div>
            <label class="label" for="np2">Confirm new password</label>
            <input id="np2" name="confirm-password" class="field" type="password" bind:value={confirm} autocomplete="new-password" required />
          </div>
        </div>
        <p class="hint">At least 12 characters. Your other sessions are signed out.</p>
        {#if pwError}<p class="error-text" role="alert">{pwError}</p>{/if}
        {#if pwMsg}<p class="meta" role="status">{pwMsg}</p>{/if}
        <div class="row"><button class="btn" aria-busy={pwBusy}>Change password</button></div>
      </form>
    </section>

    <section class="card card-pad grid1" aria-label="Two-factor sign-in">
      <p class="section-title">Two-factor sign-in</p>
      {#if !enrol}
        <p class="meta">Moving to a new phone? Confirm with your password and a code from your current authenticator, then scan a new QR code.</p>
        <form class="form" method="post" onsubmit={startTOTP}>
          <input type="text" name="username" class="sr-only" autocomplete="username" value={acc.user.username} readonly tabindex="-1" aria-hidden="true" />
          <div class="two">
            <div>
              <label class="label" for="tp">Password</label>
              <input id="tp" name="current-password" class="field" type="password" bind:value={tPassword} autocomplete="current-password" required />
            </div>
            <div>
              <label class="label" for="tc">Current code</label>
              <input id="tc" name="otp" class="field mono" bind:value={tCode} inputmode="numeric" autocomplete="one-time-code" maxlength="7" required />
            </div>
          </div>
          {#if tError}<p class="error-text" role="alert">{tError}</p>{/if}
          {#if tMsg}<p class="meta" role="status">{tMsg}</p>{/if}
          <div class="row"><button class="btn" aria-busy={tBusy}><Icon name="key" /> Set up a new authenticator</button></div>
        </form>
      {:else}
        <form class="form" method="post" onsubmit={finishTOTP}>
          <div class="qr"><img src={qr} alt="QR code for your new authenticator" width="180" height="180" /></div>
          <details>
            <summary class="meta">Can't scan? Enter the key manually</summary>
            <code class="secret">{enrol.secret}</code>
          </details>
          <div>
            <label class="label" for="nc">Code from the new authenticator</label>
            <input id="nc" name="otp" class="field mono" bind:value={newCode} inputmode="numeric" autocomplete="one-time-code" maxlength="7" required />
          </div>
          {#if tError}<p class="error-text" role="alert">{tError}</p>{/if}
          <div class="row gap">
            <button type="button" class="btn btn-ghost" onclick={() => { enrol = null; tError = ''; }}>Cancel</button>
            <button class="btn" aria-busy={tBusy}>Confirm</button>
          </div>
        </form>
      {/if}
    </section>
  {/if}

  {#if acc}
    <section class="card card-pad grid1" aria-label="Sessions">
      <p class="section-title">Where you're signed in</p>
      <ul class="list">
        {#each acc.sessions as s (s.id)}
          <li class="item">
            <div class="item-main">
              <p class="name">{browser(s.userAgent)}{#if s.current} <span class="badge up">This session</span>{/if}</p>
              <p class="meta">
                {s.ip || 'unknown address'} · signed in {fmt(s.createdAt)} · last seen {fmt(s.lastSeen)}
              </p>
              {#if s.userAgent}<p class="meta ua">{s.userAgent}</p>{/if}
            </div>
            {#if !s.current}
              <div class="item-actions">
                <button type="button" class="btn btn-ghost" onclick={() => revoke(s)}><Icon name="logout" /> Sign out</button>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
      {#if sessError}<p class="error-text" role="alert">{sessError}</p>{/if}
      {#if acc.sessions.length > 1}
        <div class="row"><button type="button" class="btn" onclick={revokeOthers}>Sign out all other sessions</button></div>
      {/if}
      {#if acc.user.source === 'oidc'}
        <p class="meta">Signing out at your identity provider doesn't end these sessions; they last 12 hours or until you sign out here.</p>
      {:else if !local}
        <p class="meta">Your proxy checks who you are on every request; signing out there ends your access here too.</p>
      {/if}
    </section>

    <PushToggle />

    {#if isAdmin()}
      <section class="card card-pad grid1" aria-label="Command line on the host">
        <p class="section-title">Command line on the host</p>
        <p class="meta">
          Run <code>moorage</code> commands (<code>stacks ls</code>, <code>stacks logs -f</code>, <code>cli</code> for the full-screen view) from
          the server's own terminal. The wrapper needs no token: it uses Moorage's local socket inside the container. The native binary
          uses the socket in Moorage's data folder, or <code>MOORAGE_URL</code> and an API token from below for another machine.
        </p>
        {#each cliSnippets as c (c.id)}
          <div class="cli-snip">
            <span class="meta">{c.label}</span>
            <div class="cli-row">
              <code class="secret">{c.cmd}</code>
              <button type="button" class="btn btn-ghost" onclick={() => copyCLI(c.id, c.cmd)}>{cliCopied === c.id ? 'Copied' : 'Copy'}</button>
            </div>
          </div>
        {/each}
        <p class="meta">Details: <code>docs/cli.md</code> in the Moorage repository.</p>
      </section>

      <section class="card card-pad grid1" aria-label="API tokens">
        <p class="section-title">API tokens (CLI &amp; remote access)</p>
        <p class="meta">
          Bearer tokens let the <code>moorage</code> CLI or scripts connect to a remote hub over HTTP(S). Local CLI commands on the server use the Unix socket automatically without a token.
        </p>

        {#if tokSecret}
          <div class="token-reveal">
            <p class="meta"><strong>Copy this token now — it won't be shown again:</strong></p>
            <code class="secret">{tokSecret}</code>
            <p class="meta">Example: <code>moorage --url {window.location.origin} --token {tokSecret} stacks ls</code></p>
            <div class="row gap">
              <button type="button" class="btn btn-primary" onclick={copySecret}>{tokCopied ? 'Copied' : 'Copy token'}</button>
              <button type="button" class="btn btn-ghost" onclick={() => { tokSecret = ''; }}>Dismiss</button>
            </div>
          </div>
        {/if}

        <form class="form" onsubmit={createToken}>
          <div class="two">
            <div>
              <label class="label" for="tok-name">Token name</label>
              <input id="tok-name" class="field" bind:value={tokName} maxlength="64" placeholder="e.g. laptop-cli or ci-deploy" required />
            </div>
            <div>
              <span class="label">Role</span>
              <Select
                label="Role"
                value={tokRole}
                items={[
                  { value: 'admin', label: 'Admin' },
                  { value: 'viewer', label: 'Viewer (read-only)' },
                ]}
                onchange={(v) => { tokRole = v as Role; }}
              />
            </div>
          </div>
          <div class="two">
            <div>
              <span class="label">Scope</span>
              {#if tokRole === 'viewer'}
                <p class="meta" style="margin-top: 8px">Viewer tokens are locked to <code>read</code> scope.</p>
              {:else}
                <Select
                  label="Scope"
                  value={tokScope}
                  items={[
                    { value: 'admin', label: 'Full admin (stacks, settings, tokens)' },
                    { value: 'deploy', label: 'Deploy & stacks (deploy, rollback, restore, checks)' },
                    { value: 'read', label: 'Read only (status, logs, diff)' },
                  ]}
                  onchange={(v) => { tokScope = v; }}
                />
              {/if}
            </div>
            <div>
              <span class="label">Expiration</span>
              <Select
                label="Expiration"
                value={tokDays}
                items={[
                  { value: '0', label: 'No expiration' },
                  { value: '30', label: '30 days' },
                  { value: '90', label: '90 days' },
                  { value: '365', label: '1 year' },
                ]}
                onchange={(v) => { tokDays = v; }}
              />
            </div>
          </div>
          {#if tokError}<p class="error-text" role="alert">{tokError}</p>{/if}
          <div class="row"><button class="btn btn-primary" aria-busy={tokBusy} disabled={tokBusy}>Create API token</button></div>
        </form>

        {#if tokens.length > 0}
          <ul class="list">
            {#each tokens as t (t.id)}
              <li class="item">
                <div class="item-main">
                  <p class="name">
                    {t.name}
                    <code class="meta" style="margin-left: 6px">{t.prefix}…</code>
                    <span class="badge" style="margin-left: 6px">{t.role}</span>
                    {#each t.scopes as sc}
                      <span class="badge" style="margin-left: 4px">{sc}</span>
                    {/each}
                  </p>
                  <p class="meta">
                    Created {fmt(t.createdAt)} by {t.createdBy}
                    {#if t.lastUsedAt} · last used {fmt(t.lastUsedAt)}{/if}
                    {#if t.expiresAt} · expires {fmt(t.expiresAt)}{/if}
                  </p>
                </div>
                <div class="item-actions">
                  <button type="button" class="btn btn-ghost" onclick={() => revokeToken(t)}>Revoke</button>
                </div>
              </li>
            {/each}
          </ul>
        {:else}
          <p class="meta">No API tokens created yet.</p>
        {/if}
      </section>
    {/if}
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 760px;
  }
  .head {
    display: grid;
    gap: 6px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .grid1 {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .form {
    display: grid;
    gap: 12px;
  }
  .two {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
    gap: 12px;
  }
  .row {
    display: flex;
    justify-content: flex-end;
  }
  .gap {
    gap: 8px;
  }
  .qr {
    display: grid;
    place-items: center;
    padding: 16px;
    border-radius: var(--r-card);
    background: #fff;
    justify-self: start;
  }
  .qr img {
    image-rendering: pixelated;
  }
  .secret {
    display: block;
    margin-top: 8px;
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    font-size: 13px;
    word-break: break-all;
    user-select: all;
  }
  summary {
    cursor: pointer;
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
  }
  .item {
    display: flex;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
    padding: 12px 0;
    border-top: 1px solid var(--rope-strong);
  }
  .item:first-child {
    border-top: 0;
    padding-top: 0;
  }
  .item-main {
    display: grid;
    gap: 4px;
    min-width: 0;
    flex: 1 1 260px;
  }
  .name {
    font-weight: 600;
    font-size: 14px;
  }
  .ua {
    font-size: 11px;
    overflow-wrap: anywhere;
  }
  .item-actions {
    display: flex;
    gap: 6px;
  }
  .pref-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
  }
  .preset-row {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .chk-wrap {
    display: grid;
    align-content: start;
  }
  .chk-line {
    display: flex;
    align-items: center;
    gap: 8px;
    min-height: 36px;
    font-size: 13px;
    color: var(--foam);
    cursor: pointer;
  }
  .override-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
    padding: 8px 10px;
    border-radius: var(--r-sm);
    background: var(--well);
  }
  .cli-snip {
    display: grid;
    gap: 4px;
  }
  .cli-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .cli-row .secret {
    flex: 1;
    min-width: 0;
    margin-top: 0;
  }
  .token-reveal {
    display: grid;
    gap: 8px;
    padding: 12px;
    border-radius: var(--r-md);
    border: 1px solid var(--rope-strong);
    background: var(--well);
  }
</style>
