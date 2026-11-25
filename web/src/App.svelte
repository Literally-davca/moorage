<script lang="ts">
  import { api, setUnauthorizedHandler, type LoginInfo } from './lib/api';
  import { app, inviteToken, loadCoolifyNames } from './lib/state.svelte';
  import Setup from './views/Setup.svelte';
  import Login from './views/Login.svelte';
  import Invite from './views/Invite.svelte';
  import Shell from './views/Shell.svelte';

  let phase = $state<'loading' | 'setup' | 'login' | 'invite' | 'app' | 'offline'>('loading');
  let offlineMsg = $state('');
  let login = $state<LoginInfo>({ sso: false, localLogin: true, proxy: false });
  let proxyError = $state('');

  async function load() {
    try {
      const s = await api.session();
      app.version = s.version;
      app.bootId = s.bootId ?? '';
      app.user = s.user;
      if (s.user) void loadCoolifyNames();
      if (s.login) login = s.login;
      proxyError = s.proxyError ?? '';
      // An invite or reset link wins over an existing session: the link is for someone else
      // or for a fresh password, and finishing it signs in as that user.
      if (inviteToken() && !s.setupRequired) phase = 'invite';
      else phase = s.user ? 'app' : s.setupRequired ? 'setup' : 'login';
    } catch (e) {
      offlineMsg = (e as Error).message;
      phase = 'offline';
    }
  }

  function enterApp() {
    if (inviteToken() || location.hash.startsWith('#/login')) location.hash = '#/';
    phase = 'app';
  }

  setUnauthorizedHandler(() => {
    app.user = null;
    phase = 'login';
  });

  $effect(() => {
    document.documentElement.dataset.theme = app.theme;
  });

  // An invite link pasted into a tab that already shows Moorage only changes the hash, so
  // the phase chosen at load would stay (the login page or the app). Re-check on hash change.
  window.addEventListener('hashchange', () => {
    if (inviteToken() && phase !== 'invite' && phase !== 'setup') load();
  });

  load();
</script>

{#if phase === 'loading'}
  <div class="center" aria-busy="true"><span class="sr-only">Loading</span></div>
{:else if phase === 'offline'}
  <div class="center">
    <div class="card card-pad" style="max-width:420px">
      <h1 style="font-size:18px;font-weight:600">Moorage isn't reachable</h1>
      <p class="meta" style="margin:8px 0 16px">{offlineMsg}</p>
      <button class="btn" onclick={load}>Try again</button>
    </div>
  </div>
{:else if phase === 'setup'}
  <Setup onDone={enterApp} />
{:else if phase === 'invite'}
  <Invite token={inviteToken()} onDone={enterApp} />
{:else if phase === 'login'}
  <Login {login} {proxyError} onDone={enterApp} />
{:else}
  <Shell onSignedOut={() => (phase = 'login')} />
{/if}

<style>
  .center {
    min-height: 100vh;
    display: grid;
    place-items: center;
    padding: 16px;
  }
</style>
