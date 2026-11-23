<script lang="ts">
  import { untrack } from 'svelte';
  import qrcode from 'qrcode-generator';
  import { api, type Account, type SessionInfo, type TOTPEnrolment } from '../lib/api';
  import Icon from '../lib/Icon.svelte';

  let acc = $state<Account | null>(null);
  let error = $state('');

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

  async function load() {
    try {
      acc = await api.account();
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
</style>
