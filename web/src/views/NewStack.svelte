<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type DeletedStack, type ImageLookup, type ImageCandidate, type LintIssue, type LintResult } from '../lib/api';
  import { link, userPrefs } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import ComposeEditor from '../lib/ComposeEditor.svelte';
  import EnvEditor from '../lib/EnvEditor.svelte';
  import SetupHint from '../lib/SetupHint.svelte';

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
  let issues = $state<LintIssue[]>([]);
  let envConflicts = $state<LintResult | null>(null);
  let deleted = $state<DeletedStack[]>([]);
  let restoring = $state<string | null>(null);

  $effect(() => {
    untrack(() => {
      api.deletedStacks().then((d) => (deleted = d)).catch(() => (deleted = []));
    });
  });

  async function restoreDeleted(d: DeletedStack, deploy: boolean) {
    restoring = d.name;
    error = '';
    try {
      const r = await api.restoreDeletedStack(d.name, d.hash, deploy);
      if (r?.deployError) {
        error = `${d.name} is restored, but it couldn't be started: ${r.deployError}`;
        return;
      }
      onCreated(d.name);
    } catch (err) {
      error = (err as Error).message;
    } finally {
      restoring = null;
    }
  }

  // convert dialog
  let dlg: HTMLDialogElement;
  let runCmd = $state('');
  let convError = $state('');
  let convWarnings = $state<string[]>([]);
  let converted = $state('');
  let convService = $state('');

  const nameOk = $derived(/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name));

  // Start from an image: Moorage looks for a template, the compose file of the image's
  // source repository, and generates one from the image's metadata.
  let image = $state('');
  let finding = $state(false);
  let found = $state<ImageLookup | null>(null);
  let findError = $state('');
  let picked = $state<ImageCandidate | null>(null);
  const sourceLabel: Record<ImageCandidate['source'], string> = {
    template: 'Template',
    repo: 'Source repository',
    generated: 'Generated',
  };
  async function find() {
    if (!image.trim()) return;
    finding = true;
    findError = '';
    found = null;
    picked = null;
    try {
      found = await api.imageLookup(image.trim());
    } catch (err) {
      findError = (err as Error).message;
    } finally {
      finding = false;
    }
  }
  function use(c: ImageCandidate) {
    compose = c.compose ?? compose;
    env = c.env ?? '';
    picked = c;
    if (!name && found && /^[a-z0-9][a-z0-9_-]{0,62}$/.test(found.name)) name = found.name;
  }
  const templateHref = (c: ImageCandidate) =>
    link(`templates/${encodeURIComponent(c.templateSource ?? '')}/${encodeURIComponent(c.templateId ?? '')}`);

  $effect(() => {
    const c = compose;
    const e = env;
    const n = nameOk ? name : 'stack';
    const t = setTimeout(async () => {
      try {
        const r = await api.validate(n, c, e);
        validation = r.ok ? '' : (r.error ?? '');
        issues = r.issues ?? [];
      } catch {
        /* ignore */
      }
    }, 600);
    return () => clearTimeout(t);
  });

  async function create(e: SubmitEvent | null, overwrite?: boolean) {
    e?.preventDefault();
    error = '';
    busy = true;
    try {
      // literal environment values move into .env (conflicts wait for an answer)
      const moved = await api.extractEnv(name, compose, env, !!overwrite);
      if (moved.changed) {
        compose = moved.compose;
        env = moved.env;
      }
      if (overwrite === undefined && moved.conflicts?.length) {
        envConflicts = moved;
        return;
      }
      envConflicts = null;
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
      <a class="btn" class:btn-primary={userPrefs().newStackDefault === 'templates'} href={link('templates')}><Icon name="template" /> From template</a>
      <button type="button" class="btn" onclick={() => { converted = ''; convError = ''; dlg.showModal(); }}>
        <Icon name="wand" /> Convert docker run
      </button>
    </div>
  </div>

  {#if userPrefs().newStackDefault === 'templates'}
    <SetupHint
      title="Want a pre-configured app?"
      detail="Your profile prefers starting from app templates. Browse ready-to-deploy Compose templates or continue editing below."
      actionLabel="Browse templates"
      actionHref={link('templates')}
    />
  {/if}

  <section class="from-image" aria-label="Start from an image">
    <label class="label" for="simage">Start from an image <span class="meta">optional</span></label>
    <div class="find-row">
      <input id="simage" class="field mono" bind:value={image} placeholder="vaultwarden/server or ghcr.io/owner/app:1.2"
        spellcheck="false" autocomplete="off" onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); void find(); } }} />
      <button type="button" class="btn" onclick={find} disabled={!image.trim() || finding} aria-busy={finding}><Icon name="search" /> Find compose</button>
    </div>
    <p class="hint">Moorage looks for a template, the compose file in the image's source repository, and builds one from the image's ports and volumes. You review everything before the stack is created.</p>
    {#if findError}<p class="error-text" role="alert">{findError}</p>{/if}
    {#if found}
      {#if found.problems?.length}
        <ul class="problems meta">{#each found.problems as p (p)}<li>{p}</li>{/each}</ul>
      {/if}
      <ul class="cands">
        {#each found.candidates as c, i (i)}
          <li class="cand" class:on={picked === c}>
            <div class="cand-head">
              <span class="tag">{sourceLabel[c.source]}</span>
              <strong>{c.title}</strong>
            </div>
            {#if c.detail}<p class="meta">{c.detail}{#if c.url} · <a href={c.url} target="_blank" rel="noopener noreferrer">open</a>{/if}</p>{/if}
            {#if c.warnings?.length}
              <ul class="warns">{#each c.warnings as w (w)}<li><Icon name="alert" /> {w}</li>{/each}</ul>
            {/if}
            {#if c.source === 'template'}
              <a class="btn" href={templateHref(c)}>Open template</a>
            {:else}
              <button type="button" class="btn" onclick={() => use(c)}>{picked === c ? 'In the editor below' : 'Use this'}</button>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  {#if deleted.length > 0}
    <section class="card card-pad find">
      <h2 class="section-title">Restore deleted stack</h2>
      <ul class="cands">
        {#each deleted as d (d.name)}
          <li class="cand">
            <div>
              <span class="mono">{d.name}</span>
              <span class="meta"> · deleted {new Date(d.deletedAt).toLocaleString()}</span>
            </div>
            <div style="display:flex;gap:6px">
              <button type="button" class="btn" disabled={restoring !== null} onclick={() => restoreDeleted(d, false)}>
                {restoring === d.name ? 'Restoring…' : 'Restore'}
              </button>
              <button type="button" class="btn" disabled={restoring !== null} onclick={() => restoreDeleted(d, true)}>
                Restore &amp; deploy
              </button>
            </div>
          </li>
        {/each}
      </ul>
    </section>
  {/if}

  <div class="name">
    <label class="label" for="sname">Name</label>
    <input id="sname" class="field" bind:value={name} placeholder="paperless" spellcheck="false" autocomplete="off" required />
    <p class="hint" class:error-text={name !== '' && !nameOk}>
      Folder and compose project name: lowercase letters, digits, dash or underscore.
    </p>
  </div>

  <div>
    <ComposeEditor bind:value={compose} bind:env name={nameOk ? name : ''} {issues} title="compose.yaml" />
    {#if validation}
      <div class="invalid" role="alert"><Icon name="alert" /><span class="mono">{validation}</span></div>
    {/if}
  </div>

  <div>
    <span class="label">.env <span class="meta">(optional)</span></span>
    <EnvEditor bind:value={env} {compose} />
  </div>

  {#if envConflicts?.conflicts?.length}
    <div class="invalid" role="alert">
      <Icon name="alert" />
      <div>
        <p>Moving values into .env: {envConflicts.conflicts.length === 1 ? 'this key is' : 'these keys are'} already in .env with another value.</p>
        <ul class="mono">
          {#each envConflicts.conflicts as c (c.key + c.service)}<li>{c.key} ({c.service}): .env {c.envValue}, compose {c.composeValue}</li>{/each}
        </ul>
        <button type="button" class="btn btn-primary" onclick={() => create(null, true)}>Overwrite .env and create</button>
        <button type="button" class="btn" onclick={() => create(null, false)}>Keep .env and create</button>
      </div>
    </div>
  {/if}
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
  .from-image {
    display: grid;
    gap: 8px;
    padding-bottom: 16px;
    border-bottom: 1px solid var(--rope);
  }
  .find-row {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .find-row .field {
    flex: 1;
    min-width: 220px;
  }
  .problems {
    margin: 0;
    padding-left: 18px;
  }
  .cands {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    gap: 8px;
  }
  .cand {
    display: grid;
    gap: 6px;
    align-content: start;
    justify-items: start;
    padding: 12px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .cand.on {
    box-shadow: inset 0 0 0 2px var(--buoy);
  }
  .tag {
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .cand-head {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .cand .meta {
    overflow-wrap: anywhere;
  }
  .warns {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
    font-size: 12px;
    color: var(--adrift-text);
  }
  .warns li {
    display: flex;
    gap: 6px;
  }
</style>
