<script lang="ts">
  import { api, setUnauthorizedHandler } from './lib/api';
  import { app } from './lib/state.svelte';
  import Setup from './views/Setup.svelte';
  import Login from './views/Login.svelte';
  import Shell from './views/Shell.svelte';

  let phase = $state<'loading' | 'setup' | 'login' | 'app' | 'offline'>('loading');
  let offlineMsg = $state('');

  async function load() {
    try {
      const s = await api.session();
      app.version = s.version;
      app.user = s.user;
      phase = s.user ? 'app' : s.setupRequired ? 'setup' : 'login';
    } catch (e) {
      offlineMsg = (e as Error).message;
      phase = 'offline';
    }
  }

  setUnauthorizedHandler(() => {
    app.user = null;
    phase = 'login';
  });

  $effect(() => {
    document.documentElement.dataset.theme = app.theme;
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
  <Setup onDone={() => (phase = 'app')} />
{:else if phase === 'login'}
  <Login onDone={() => (phase = 'app')} />
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
