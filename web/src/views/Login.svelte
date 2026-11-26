<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type LoginInfo } from '../lib/api';
  import { app, loginParams } from '../lib/state.svelte';
  import AuthFrame from './AuthFrame.svelte';
  import { getPasskey, passkeysSupported } from '../lib/webauthn';

  let { login, proxyError = '', onDone }: { login: LoginInfo; proxyError?: string; onDone: () => void } = $props();

  // Plain words for the codes the SSO callback sends back in #/login?sso=<code>.
  const ssoErrors: Record<string, string> = {
    denied: "Your account has no access to Moorage. Ask an admin to add you to a Moorage group in your identity provider.",
    disabled: 'An admin disabled your Moorage account. Ask them to enable it.',
    setup: "Moorage isn't set up yet. The first admin must finish setup with a local account.",
    expired: 'The sign-in took too long or was started in another browser. Try again.',
    idp: 'Your identity provider cancelled the sign-in.',
    failed: "Sign-in with your identity provider failed. An admin can see why in Settings → Users.",
    unavailable: "Single sign-on isn't available right now. An admin can see why in Settings → Users.",
  };

  const params = loginParams();
  // ?local=1 always shows the password form: the break-glass admin signs in there when
  // MOORAGE_OIDC_LOCAL_LOGIN=false.
  const showLocal = $derived(login.localLogin || params.get('local') === '1');
  const ssoCode = params.get('sso') ?? '';

  let username = $state('');
  let password = $state('');
  let code = $state('');
  // The first message only: later errors come from the form.
  let error = $state(ssoCode ? (ssoErrors[ssoCode] ?? ssoErrors.failed) : untrack(() => proxyError));
  let busy = $state(false);

  // Passkeys are bound to the public URL; at any other address the browser refuses them.
  const passkeyOK = $derived(!!login.passkeyOrigin && window.location.origin === login.passkeyOrigin && passkeysSupported());

  /** Password plus a passkey instead of the code. */
  async function passkeySecondFactor() {
    error = '';
    if (!username.trim() || !password) {
      error = 'Enter your username and password first, then use your passkey.';
      return;
    }
    busy = true;
    try {
      const c = await api.passkeyLoginOptions();
      const response = await getPasskey(c.options);
      app.user = await api.loginWithPasskey(username, password, c.ceremony, response);
      onDone();
    } catch (err) {
      error = (err as Error).message;
    } finally {
      busy = false;
    }
  }

  /** Passwordless: a passkey the user allowed to sign in on its own. */
  async function passkeyOnly() {
    error = '';
    busy = true;
    try {
      const c = await api.passkeyLoginOptions();
      const response = await getPasskey(c.options);
      app.user = await api.passkeyLogin(c.ceremony, response);
      onDone();
    } catch (err) {
      error = (err as Error).message;
    } finally {
      busy = false;
    }
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      app.user = await api.login(username, password, code);
      onDone();
    } catch (err) {
      error = (err as Error).message;
      code = '';
    } finally {
      busy = false;
    }
  }
</script>

<AuthFrame
  title="Sign in"
  lead={showLocal ? 'Use your password and the code from your authenticator app.' : 'Sign in with your organisation account.'}>
  {#if error}<p class="error-text top" role="alert">{error}</p>{/if}
  {#if login.sso}
    <a class="btn wide {showLocal ? '' : 'btn-primary'}" href="/api/auth/oidc/start">{login.ssoLabel || 'Sign in with SSO'}</a>
    {#if showLocal}<p class="or meta"><span>or with a Moorage account</span></p>{/if}
  {/if}
  {#if showLocal}
    <form method="post" onsubmit={submit} class="form">
      <div>
        <label class="label" for="user">Username</label>
        <input id="user" name="username" class="field" bind:value={username} autocomplete="username" required />
      </div>
      <div>
        <label class="label" for="pw">Password</label>
        <input id="pw" name="password" class="field" type="password" bind:value={password} autocomplete="current-password" required />
      </div>
      <div>
        <label class="label" for="code">Code</label>
        <input id="code" name="otp" class="field mono code" bind:value={code} inputmode="numeric" autocomplete="one-time-code"
          maxlength="7" required />
      </div>
      <button class="btn btn-primary wide" aria-busy={busy}>Sign in</button>
      {#if passkeyOK}
        <button type="button" class="btn btn-ghost wide" disabled={busy} onclick={passkeySecondFactor}>
          Use a passkey instead of the code
        </button>
      {/if}
    </form>
    {#if passkeyOK}
      <p class="or meta"><span>or</span></p>
      <button type="button" class="btn wide" disabled={busy} onclick={passkeyOnly}>Sign in with a passkey</button>
      <p class="meta hint-pk">Works for passkeys you allowed to sign in without a password on your Account page.</p>
    {/if}
  {/if}
</AuthFrame>

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
  .code {
    letter-spacing: 0.2em;
  }
  .hint-pk {
    margin-top: 8px;
    text-align: center;
  }
  .top {
    margin-bottom: 16px;
  }
  .or {
    display: flex;
    align-items: center;
    gap: 10px;
    margin: 18px 0 14px;
  }
  .or::before,
  .or::after {
    content: '';
    flex: 1;
    border-top: 1px solid var(--rope-strong);
  }
</style>
