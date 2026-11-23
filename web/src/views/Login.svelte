<script lang="ts">
  import { api } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import AuthFrame from './AuthFrame.svelte';

  let { onDone }: { onDone: () => void } = $props();

  let username = $state('');
  let password = $state('');
  let code = $state('');
  let error = $state('');
  let busy = $state(false);

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

<AuthFrame title="Sign in" lead="Use your password and the code from your authenticator app.">
  <form onsubmit={submit} class="form">
    <div>
      <label class="label" for="user">Username</label>
      <input id="user" class="field" bind:value={username} autocomplete="username" required />
    </div>
    <div>
      <label class="label" for="pw">Password</label>
      <input id="pw" class="field" type="password" bind:value={password} autocomplete="current-password" required />
    </div>
    <div>
      <label class="label" for="code">Code</label>
      <input id="code" class="field mono code" bind:value={code} inputmode="numeric" autocomplete="one-time-code"
        maxlength="7" required />
    </div>
    {#if error}<p class="error-text" role="alert">{error}</p>{/if}
    <button class="btn btn-primary wide" aria-busy={busy}>Sign in</button>
  </form>
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
  }
  .code {
    letter-spacing: 0.2em;
  }
</style>
