<script lang="ts">
  import qrcode from 'qrcode-generator';
  import { api } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import AuthFrame from './AuthFrame.svelte';

  let { onDone }: { onDone: () => void } = $props();

  let step = $state<1 | 2>(1);
  let token = $state('');
  let username = $state('admin');
  let password = $state('');
  let confirm = $state('');
  let secret = $state('');
  let qr = $state('');
  let code = $state('');
  let error = $state('');
  let busy = $state(false);

  async function start(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    if (password !== confirm) {
      error = "Passwords don't match.";
      return;
    }
    busy = true;
    try {
      const r = await api.setupStart(token, username, password);
      secret = r.secret;
      const q = qrcode(0, 'M');
      q.addData(r.otpauthUrl);
      q.make();
      qr = q.createDataURL(5, 2);
      step = 2;
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
      app.user = await api.setupFinish(token, username, code);
      onDone();
    } catch (err) {
      error = (err as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

{#if step === 1}
  <AuthFrame title="Set up Moorage" lead="Create the admin account. You'll add an authenticator app in the next step.">
    <form onsubmit={start} class="form">
      <div>
        <label class="label" for="token">Setup token</label>
        <input id="token" class="field mono" bind:value={token} autocomplete="off" required spellcheck="false" />
        <p class="hint">Printed in the Moorage log on first start.</p>
      </div>
      <div>
        <label class="label" for="user">Username</label>
        <input id="user" class="field" bind:value={username} autocomplete="username" required />
      </div>
      <div>
        <label class="label" for="pw">Password</label>
        <input id="pw" class="field" type="password" bind:value={password} autocomplete="new-password" minlength="12" required />
        <p class="hint">At least 12 characters.</p>
      </div>
      <div>
        <label class="label" for="pw2">Confirm password</label>
        <input id="pw2" class="field" type="password" bind:value={confirm} autocomplete="new-password" required />
      </div>
      {#if error}<p class="error-text" role="alert">{error}</p>{/if}
      <button class="btn btn-primary wide" aria-busy={busy}>Continue</button>
    </form>
  </AuthFrame>
{:else}
  <AuthFrame title="Add two-factor sign-in" lead="Scan the code with an authenticator app, then enter the 6-digit code it shows.">
    <form onsubmit={finish} class="form">
      <div class="qr">
        <img src={qr} alt="QR code for your authenticator app" width="180" height="180" />
      </div>
      <details>
        <summary class="meta">Can't scan? Enter the key manually</summary>
        <code class="secret">{secret}</code>
      </details>
      <div>
        <label class="label" for="code">Code</label>
        <input id="code" class="field mono code" bind:value={code} inputmode="numeric" autocomplete="one-time-code"
          maxlength="7" required />
      </div>
      {#if error}<p class="error-text" role="alert">{error}</p>{/if}
      <button class="btn btn-primary wide" aria-busy={busy}>Finish setup</button>
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
