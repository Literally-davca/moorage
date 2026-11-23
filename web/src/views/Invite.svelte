<script lang="ts">
  import qrcode from 'qrcode-generator';
  import { api, type InviteInfo } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import AuthFrame from './AuthFrame.svelte';

  let { token, onDone }: { token: string; onDone: () => void } = $props();

  let info = $state<InviteInfo | null>(null);
  let step = $state<'check' | 'bad' | 'password' | 'totp'>('check');
  let password = $state('');
  let confirm = $state('');
  let secret = $state('');
  let qr = $state('');
  let code = $state('');
  let error = $state('');
  let busy = $state(false);

  async function inspect() {
    try {
      info = await api.inviteInspect(token);
      step = 'password';
    } catch (e) {
      error = (e as Error).message;
      step = 'bad';
    }
  }
  inspect();

  async function start(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    if (password !== confirm) {
      error = "Passwords don't match.";
      return;
    }
    busy = true;
    try {
      const r = await api.inviteStart(token, password);
      secret = r.secret;
      const q = qrcode(0, 'M');
      q.addData(r.otpauthUrl);
      q.make();
      qr = q.createDataURL(5, 2);
      step = 'totp';
    } catch (err) {
      error = (err as Error).message;
    } finally {
      busy = false;
    }
  }

  async function finish(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      app.user = await api.inviteFinish(token, code);
      onDone();
    } catch (err) {
      error = (err as Error).message;
      code = '';
    } finally {
      busy = false;
    }
  }

  const reset = $derived(info?.kind === 'reset');
</script>

{#if step === 'check'}
  <AuthFrame title="Checking your link" lead="One moment.">
    <span class="skeleton" style="height:12px;width:200px;display:block"></span>
  </AuthFrame>
{:else if step === 'bad'}
  <AuthFrame title="This link doesn't work" lead="Invite and reset links work once and expire after 48 hours.">
    <p class="error-text" role="alert">{error}</p>
    <a class="btn wide" href="/">Go to sign-in</a>
  </AuthFrame>
{:else if step === 'password' && info}
  <AuthFrame
    title={reset ? 'Reset your sign-in' : 'Welcome to Moorage'}
    lead={reset
      ? `Choose a new password for ${info.username}. You'll set up your authenticator app again in the next step; your old password and code stop working when you finish.`
      : `You were invited as ${info.role} with the username ${info.username}. Choose a password, then add an authenticator app.`}>
    <form method="post" onsubmit={start} class="form">
      <input type="text" name="username" class="sr-only" autocomplete="username" value={info.username} readonly tabindex="-1" aria-hidden="true" />
      <div>
        <label class="label" for="pw">Password</label>
        <input id="pw" name="new-password" class="field" type="password" bind:value={password} autocomplete="new-password" minlength="12" required />
        <p class="hint">At least 12 characters.</p>
      </div>
      <div>
        <label class="label" for="pw2">Confirm password</label>
        <input id="pw2" name="confirm-password" class="field" type="password" bind:value={confirm} autocomplete="new-password" required />
      </div>
      {#if error}<p class="error-text" role="alert">{error}</p>{/if}
      <button class="btn btn-primary wide" aria-busy={busy}>Continue</button>
    </form>
  </AuthFrame>
{:else}
  <AuthFrame title="Add two-factor sign-in" lead="Scan the code with an authenticator app, then enter the 6-digit code it shows. Every Moorage account needs one.">
    <form method="post" onsubmit={finish} class="form">
      <div class="qr">
        <img src={qr} alt="QR code for your authenticator app" width="180" height="180" />
      </div>
      <details>
        <summary class="meta">Can't scan? Enter the key manually</summary>
        <code class="secret">{secret}</code>
      </details>
      <div>
        <label class="label" for="code">Code</label>
        <input id="code" name="otp" class="field mono code" bind:value={code} inputmode="numeric" autocomplete="one-time-code"
          maxlength="7" required />
      </div>
      {#if error}<p class="error-text" role="alert">{error}</p>{/if}
      <button class="btn btn-primary wide" aria-busy={busy}>{reset ? 'Finish reset' : 'Finish and sign in'}</button>
    </form>
  </AuthFrame>
{/if}

<style>
  .form {
    display: grid;
    gap: 16px;
  }
  .wide {
    width: 100%;
    height: 36px;
    margin-top: 4px;
    justify-content: center;
    text-decoration: none;
  }
  .qr {
    display: grid;
    place-items: center;
    padding: 16px;
    border-radius: var(--r-card);
    background: #fff;
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
  .code {
    font-size: 18px;
    letter-spacing: 0.2em;
    height: 44px;
  }
</style>
