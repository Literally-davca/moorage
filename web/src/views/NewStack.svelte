<script lang="ts">
  import { api } from '../lib/api';
  import { link } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import CodeEditor from '../lib/CodeEditor.svelte';

  let { onCreated }: { onCreated: (name: string) => void } = $props();

  const starter = `services:
  app:
    image: nginx:1.27-alpine
    restart: unless-stopped
    ports:
      - "8080:80"
`;

  let name = $state('');
  let compose = $state(starter);
  let env = $state('');
  let error = $state('');
  let busy = $state(false);
  let validation = $state('');

  // convert dialog
  let dlg: HTMLDialogElement;
  let runCmd = $state('');
  let convError = $state('');
  let convWarnings = $state<string[]>([]);
  let converted = $state('');
  let convService = $state('');

  const nameOk = $derived(/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name));

  $effect(() => {
    const c = compose;
    const e = env;
    const n = nameOk ? name : 'stack';
    const t = setTimeout(async () => {
      try {
        const r = await api.validate(n, c, e);
        validation = r.ok ? '' : (r.error ?? '');
      } catch {
        /* ignore */
      }
    }, 600);
    return () => clearTimeout(t);
  });

  async function create(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      await api.create(name, compose, env);
      onCreated(name);
    } catch (err) {
      error = (err as Error).message;
    } finally {
      busy = false;
    }
  }

  async function convert() {
    convError = '';
    converted = '';
    try {
      const r = await api.convert(runCmd);
      converted = r.yaml;
      convWarnings = r.warnings;
      convService = r.service;
    } catch (err) {
      convError = (err as Error).message;
    }
  }

  function useConverted() {
    compose = converted;
    if (!name && /^[a-z0-9][a-z0-9_-]{0,62}$/.test(convService)) name = convService;
    dlg.close();
  }
</script>

<form class="card card-pad new" onsubmit={create}>
  <div class="head">
    <h1 class="title">New stack</h1>
    <div class="head-actions">
      <a class="btn" href={link('templates')}><Icon name="template" /> From template</a>
      <button type="button" class="btn" onclick={() => { converted = ''; convError = ''; dlg.showModal(); }}>
        <Icon name="wand" /> Convert docker run
      </button>
    </div>
  </div>

  <div class="name">
    <label class="label" for="sname">Name</label>
    <input id="sname" class="field" bind:value={name} placeholder="paperless" spellcheck="false" autocomplete="off" required />
    <p class="hint" class:error-text={name !== '' && !nameOk}>
      Folder and compose project name: lowercase letters, digits, dash or underscore.
    </p>
  </div>

  <div>
    <span class="label">compose.yaml</span>
    <CodeEditor bind:value={compose} label="Compose file" minHeight={340} />
    {#if validation}
      <div class="invalid" role="alert"><Icon name="alert" /><span class="mono">{validation}</span></div>
    {/if}
  </div>

  <div>
    <span class="label">.env <span class="meta">(optional)</span></span>
    <CodeEditor bind:value={env} language="plain" label=".env file" minHeight={80} />
  </div>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
  <div class="row">
    <a class="btn btn-ghost" href={link('')}>Cancel</a>
    <button class="btn btn-primary" disabled={!nameOk || !!validation || busy} aria-busy={busy}>Create stack</button>
  </div>
  <p class="meta">Creating saves the files. Deploy starts the containers from the stack page.</p>
</form>

<dialog bind:this={dlg} aria-labelledby="conv-title">
  <div class="dlg">
    <h2 id="conv-title">Convert docker run</h2>
    <p class="meta">Paste a <code>docker run</code> command. Moorage turns it into a compose service.</p>
    <textarea class="field" rows="5" bind:value={runCmd} spellcheck="false"
      placeholder="docker run -d --name uptime -p 3001:3001 -v uptime:/app/data louislam/uptime-kuma:2"></textarea>
    {#if convError}<p class="error-text">{convError}</p>{/if}
    {#if converted}
      <pre class="out mono">{converted}</pre>
      {#if convWarnings.length}
        <ul class="warns">
          {#each convWarnings as w}<li><Icon name="alert" /> {w}</li>{/each}
        </ul>
      {/if}
    {/if}
    <div class="row">
      <button type="button" class="btn" onclick={() => dlg.close()}>Cancel</button>
      {#if converted}
        <button type="button" class="btn btn-primary" onclick={useConverted}>Use this compose file</button>
      {:else}
        <button type="button" class="btn btn-primary" onclick={convert} disabled={!runCmd.trim()}>Convert</button>
      {/if}
    </div>
  </div>
</dialog>

<style>
  .new {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 18px;
    max-width: 920px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    flex-wrap: wrap;
    gap: 12px;
  }
  .head-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .name {
    max-width: 360px;
  }
  .row {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
  .btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
  .invalid {
    display: flex;
    gap: 10px;
    margin-top: 10px;
    padding: 10px 14px;
    border-radius: var(--r-md);
    background: var(--adrift-wash);
    color: var(--adrift-text);
    font-size: 12px;
    white-space: pre-wrap;
  }
  .dlg {
    padding: 22px 24px 20px;
    display: grid;
    gap: 12px;
  }
  h2 {
    font-size: 16px;
    font-weight: 600;
  }
  .out {
    margin: 0;
    padding: 12px 14px;
    border-radius: var(--r-md);
    background: var(--well);
    font-size: 12px;
    line-height: 1.6;
    max-height: 280px;
    overflow: auto;
  }
  .warns {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
    font-size: 13px;
    color: var(--signal-text);
  }
  .warns li {
    display: flex;
    gap: 8px;
  }
</style>
