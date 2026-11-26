<script lang="ts">
  import { SvelteMap, SvelteSet } from 'svelte/reactivity';
  import {
    api,
    type ImportResult,
    type MonitorImportPreview,
    type MonitorImportSource,
    type PortainerStackPreview,
  } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';

  const DOCS = 'https://github.com/Literally-davca/moorage/blob/main/docs/migration.md';
  const stackNameRe = /^[a-z0-9][a-z0-9_-]{0,62}$/;
  const MAX_UPLOAD = 5 * 1024 * 1024;

  // ---- Portainer ----
  const writableHosts = $derived(app.hosts.filter((h) => h.connected && !h.readOnly));
  let pUrl = $state('');
  let pToken = $state('');
  let pInsecure = $state(false);
  let pHost = $state('');
  let pLoading = $state(false);
  let pApplying = $state(false);
  let pErr = $state('');
  let pStacks = $state<PortainerStackPreview[] | null>(null);
  /** Selected Portainer stack id → Moorage stack name. */
  const pSel = new SvelteMap<number, string>();
  let pResults = $state<ImportResult[] | null>(null);

  $effect(() => {
    if (!pHost && writableHosts.length) {
      pHost = writableHosts.some((h) => h.id === app.host) ? app.host : writableHosts[0].id;
    }
  });

  const conn = () => ({ url: pUrl.trim(), token: pToken, insecure: pInsecure, hostId: pHost });
  const selectable = (st: PortainerStackPreview) =>
    st.status === 'ready' || st.status === 'exists' || (st.status === 'invalid' && !st.target);

  async function loadPortainer(e: SubmitEvent) {
    e.preventDefault();
    pErr = '';
    pResults = null;
    pLoading = true;
    try {
      const r = await api.portainerPreview(conn());
      pStacks = r.stacks;
      pSel.clear();
      for (const st of r.stacks) if (st.status === 'ready') pSel.set(st.id, st.target);
    } catch (err) {
      pErr = (err as Error).message;
      pStacks = null;
    } finally {
      pLoading = false;
    }
  }

  function togglePortainer(st: PortainerStackPreview, on: boolean) {
    if (on) pSel.set(st.id, st.target);
    else pSel.delete(st.id);
  }

  const pBadName = $derived([...pSel.values()].some((n) => !stackNameRe.test(n)));

  async function importPortainer() {
    pErr = '';
    pApplying = true;
    try {
      const r = await api.portainerApply(conn(), [...pSel.entries()].map(([id, name]) => ({ id, name: name.trim() })));
      pResults = r.results;
      if (r.error) pErr = r.error;
      if (r.created > 0) {
        // The token isn't needed any more; don't keep it in the page.
        pToken = '';
        pStacks = null;
        pSel.clear();
      }
    } catch (err) {
      pErr = (err as Error).message;
    } finally {
      pApplying = false;
    }
  }

  // ---- Uptime Kuma / Gatus ----
  let mSource = $state<MonitorImportSource>('uptime-kuma');
  let mFile = $state<File | null>(null);
  let mContent = '';
  let mLoading = $state(false);
  let mApplying = $state(false);
  let mErr = $state('');
  let mPreview = $state<MonitorImportPreview | null>(null);
  const mSel = new SvelteSet<string>();
  let mPause = $state(false);
  let mResults = $state<ImportResult[] | null>(null);

  async function previewMonitors(e: SubmitEvent) {
    e.preventDefault();
    if (!mFile) return;
    mErr = '';
    mResults = null;
    if (mFile.size > MAX_UPLOAD) {
      mErr = 'The file is larger than 5 MB.';
      return;
    }
    mLoading = true;
    try {
      mContent = await mFile.text();
      const p = await api.monitorImportPreview(mSource, mContent);
      mPreview = p;
      mSel.clear();
      for (const c of p.channels) if (!c.exists && !c.error) mSel.add(c.key);
      for (const c of p.checks) if (!c.exists && !c.error) mSel.add(c.key);
      for (const pg of p.pages) if (!pg.exists) mSel.add(pg.key);
    } catch (err) {
      mErr = (err as Error).message;
      mPreview = null;
    } finally {
      mLoading = false;
    }
  }

  function toggle(key: string, on: boolean) {
    if (on) mSel.add(key);
    else mSel.delete(key);
  }

  async function importMonitors() {
    if (!mPreview) return;
    mErr = '';
    mApplying = true;
    try {
      const r = await api.monitorImportApply(mPreview.source, mContent, [...mSel], mPause);
      mResults = r.results;
      if (r.error) mErr = r.error;
      if (Object.values(r.created).some((n) => n > 0)) {
        mPreview = null;
        mContent = '';
        mFile = null;
        mSel.clear();
      }
    } catch (err) {
      mErr = (err as Error).message;
    } finally {
      mApplying = false;
    }
  }

  const channelName = (key: string) => mPreview?.channels.find((c) => c.key === key)?.name ?? key;
  const kindLabel: Record<string, string> = { http: 'HTTP', tcp: 'TCP', ping: 'Ping', dns: 'DNS' };
</script>

<div class="page">
  <section class="card card-pad">
    <h1 class="title">Import</h1>
    <p class="meta">
      Bring stacks, checks, notification channels and status pages over from other tools. Every import shows a preview first, and nothing is
      deployed or started: you deploy imported stacks yourself when you're ready.
    </p>
  </section>

  <!-- Portainer -->
  <section class="card card-pad block">
    <div>
      <h2 class="section-title">Portainer CE stacks</h2>
      <p class="meta">
        Reads your stacks through the Portainer API and saves the ones you pick as <code>compose.yaml</code> + <code>.env</code> in the stacks
        folder, each with its first version in the history. The access token is used for this request only and is never stored or logged.
      </p>
    </div>
    <form class="grid-form" onsubmit={loadPortainer}>
      <label class="field-wrap">
        <span>Portainer URL</span>
        <input class="field mono" bind:value={pUrl} placeholder="https://portainer.lan:9443" required autocomplete="off" />
      </label>
      <label class="field-wrap">
        <span>Access token</span>
        <input class="field mono" type="password" bind:value={pToken} placeholder="ptr_…" required autocomplete="off" />
      </label>
      <div class="field-wrap">
        <span>Save to host</span>
        <Select
          label="Save to host"
          value={pHost}
          placeholder={writableHosts.length ? 'Pick a host' : 'No writable host is connected'}
          items={writableHosts.map((h) => ({ value: h.id, label: h.name }))}
          onchange={(v) => (pHost = v)}
        />
      </div>
      <label class="check-row">
        <input type="checkbox" bind:checked={pInsecure} />
        <span>Accept a self-signed certificate (Portainer's default on port 9443)</span>
      </label>
      <div class="actions">
        <button type="submit" class="btn" disabled={pLoading || !pHost}><Icon name="search" /> {pLoading ? 'Loading…' : 'Load stacks'}</button>
        <span class="hint">Create the token in Portainer under My account → Access tokens. An admin's token sees every stack.</span>
      </div>
    </form>

    {#if pStacks}
      {#if !pStacks.length}
        <p class="meta">Portainer lists no stacks for this token.</p>
      {:else}
        <ul class="rows">
          {#each pStacks as st (st.id)}
            {@const on = pSel.has(st.id)}
            <li class="row" class:off={!selectable(st)}>
              <div class="row-head">
                <input
                  type="checkbox"
                  aria-label={`Import ${st.name}`}
                  checked={on}
                  disabled={!selectable(st)}
                  onchange={(e) => togglePortainer(st, e.currentTarget.checked)}
                />
                <b>{st.name}</b>
                <span class="pill">{st.type}</span>
                {#if st.endpoint}<span class="pill">{st.endpoint}</span>{/if}
                {#if st.git}<span class="pill">Git</span>{/if}
                {#if st.status === 'ready'}<span class="pill on">Ready</span>
                {:else if st.status === 'exists'}<span class="pill warn">Name taken</span>
                {:else if st.status === 'invalid'}<span class="pill bad-pill">Needs attention</span>
                {:else}<span class="pill bad-pill">Not supported</span>{/if}
              </div>
              {#if on}
                <label class="rename">
                  <span class="meta">Stack name in Moorage</span>
                  <input
                    class="field mono"
                    value={pSel.get(st.id)}
                    oninput={(e) => pSel.set(st.id, e.currentTarget.value)}
                    aria-invalid={!stackNameRe.test(pSel.get(st.id) ?? '')}
                  />
                </label>
              {/if}
              {#if st.reason}<p class="reason">{st.reason}</p>{/if}
              {#each st.warnings as w, i (i)}<p class="warn-line"><Icon name="alert" /> {w}</p>{/each}
              {#if st.envKeys.length}<p class="meta">.env: {st.envKeys.join(', ')}</p>{/if}
              {#if st.compose}
                <details>
                  <summary class="meta">compose.yaml</summary>
                  <pre class="code mono">{st.compose}</pre>
                </details>
              {/if}
            </li>
          {/each}
        </ul>
        {#if pBadName}<p class="error-text">Stack names use 1–63 lowercase letters, digits, - or _, starting with a letter or digit.</p>{/if}
        <div class="actions">
          <button type="button" class="btn btn-primary" disabled={pApplying || !pSel.size || pBadName} onclick={importPortainer}>
            <Icon name="download" /> {pApplying ? 'Importing…' : `Import ${pSel.size} ${pSel.size === 1 ? 'stack' : 'stacks'}`}
          </button>
          <span class="hint">Stop each stack in Portainer before you deploy it from Moorage, so two tools don't manage the same containers.</span>
        </div>
      {/if}
    {/if}
    {#if pResults}
      <ul class="results">
        {#each pResults as r, i (i)}
          <li class:ok={r.ok} class:fail={!r.ok}>
            <Icon name={r.ok ? 'check' : 'x'} /> <b>{r.name}</b>{#if r.ok}: saved, not deployed{:else}: {r.reason}{/if}
          </li>
        {/each}
      </ul>
    {/if}
    {#if pErr}<p class="error-text" role="alert">{pErr}</p>{/if}
  </section>

  <!-- Uptime Kuma / Gatus -->
  <section class="card card-pad block">
    <div>
      <h2 class="section-title">Uptime Kuma and Gatus monitors</h2>
      <p class="meta">
        Turns an Uptime Kuma JSON backup or a Gatus <code>config.yaml</code> into checks, notification channels and (from Gatus) a status page.
        Secrets from the file go into the channels, like when you add them by hand. Running the same import again only adds what's missing:
        checks and channels are matched by name, status pages by URL slug, and existing ones are never changed.
      </p>
    </div>
    <form class="grid-form" onsubmit={previewMonitors}>
      <div class="field-wrap">
        <span>Import from</span>
        <Select
          label="Import from"
          value={mSource}
          items={[
            { value: 'uptime-kuma', label: 'Uptime Kuma backup (.json)' },
            { value: 'gatus', label: 'Gatus config (.yaml)' },
          ]}
          onchange={(v) => {
            mSource = v as MonitorImportSource;
            mPreview = null;
          }}
        />
      </div>
      <label class="file-pick field" title={mFile ? mFile.name : 'Choose a file'}>
        <input
          type="file"
          class="sr-only"
          accept={mSource === 'gatus' ? '.yaml,.yml,text/yaml' : '.json,application/json'}
          required
          onchange={(e) => {
            mFile = e.currentTarget.files?.[0] ?? null;
            mPreview = null;
          }}
        />
        <span class="btn btn-ghost pick-btn"><Icon name="upload" /> Choose file</span>
        <span class="file-name" class:empty={!mFile}>{mFile ? mFile.name : 'No file selected'}</span>
      </label>
      <div class="actions">
        <button type="submit" class="btn" disabled={mLoading || !mFile}><Icon name="search" /> {mLoading ? 'Reading…' : 'Check & preview'}</button>
        <span class="hint">
          {mSource === 'gatus' ? 'The config.yaml Gatus runs with, at most 5 MB.' : 'In Uptime Kuma 1.x: Settings → Backup → Export. At most 5 MB.'}
        </span>
      </div>
    </form>

    {#if mPreview}
      {#each mPreview.notes as n, i (i)}<p class="meta"><Icon name="info" /> {n}</p>{/each}

      {#if mPreview.channels.length}
        <h3 class="sub">Notification channels</h3>
        <ul class="rows">
          {#each mPreview.channels as c (c.key)}
            <li class="row" class:off={c.exists || !!c.error}>
              <div class="row-head">
                <input
                  type="checkbox"
                  aria-label={`Import ${c.name}`}
                  checked={mSel.has(c.key)}
                  disabled={c.exists || !!c.error}
                  onchange={(e) => toggle(c.key, e.currentTarget.checked)}
                />
                <b>{c.name}</b>
                <span class="pill">{c.kind}</span>
                <span class="meta">{c.summary}</span>
                {#if c.exists && !c.error}<span class="pill">Exists, reused</span>{/if}
                {#if !c.enabled}<span class="pill">Off</span>{/if}
              </div>
              {#if c.error}<p class="reason">{c.error}</p>{/if}
              {#each c.warnings as w, i (i)}<p class="warn-line"><Icon name="alert" /> {w}</p>{/each}
            </li>
          {/each}
        </ul>
      {/if}

      {#if mPreview.checks.length}
        <h3 class="sub">Checks</h3>
        <ul class="rows">
          {#each mPreview.checks as c (c.key)}
            <li class="row" class:off={c.exists || !!c.error}>
              <div class="row-head">
                <input
                  type="checkbox"
                  aria-label={`Import ${c.name}`}
                  checked={mSel.has(c.key)}
                  disabled={c.exists || !!c.error}
                  onchange={(e) => toggle(c.key, e.currentTarget.checked)}
                />
                <b>{c.name}</b>
                <span class="pill">{kindLabel[c.kind] ?? c.kind}</span>
                <span class="meta mono target">{c.target}</span>
                <span class="meta">every {c.interval}s</span>
                {#if !c.enabled}<span class="pill">Paused</span>{/if}
                {#if c.exists}<span class="pill">Exists, skipped</span>{/if}
              </div>
              {#if c.channels.length}<p class="meta">Alerts: {c.channels.map(channelName).join(', ')}</p>{/if}
              {#if c.error}<p class="reason">{c.error}</p>{/if}
              {#each c.warnings as w, i (i)}<p class="warn-line"><Icon name="alert" /> {w}</p>{/each}
            </li>
          {/each}
        </ul>
      {/if}

      {#if mPreview.pages.length}
        <h3 class="sub">Status pages</h3>
        <ul class="rows">
          {#each mPreview.pages as pg (pg.key)}
            <li class="row" class:off={pg.exists}>
              <div class="row-head">
                <input
                  type="checkbox"
                  aria-label={`Import ${pg.title}`}
                  checked={mSel.has(pg.key)}
                  disabled={pg.exists}
                  onchange={(e) => toggle(pg.key, e.currentTarget.checked)}
                />
                <b>{pg.title}</b>
                <span class="meta mono">/status/{pg.slug}</span>
                <span class="meta">{pg.entries.length} items, created unpublished</span>
                {#if pg.exists}<span class="pill">Slug taken, skipped</span>{/if}
              </div>
            </li>
          {/each}
        </ul>
      {/if}

      {#if mPreview.skipped.length}
        <h3 class="sub">Not converted ({mPreview.skipped.length})</h3>
        <ul class="skipped">
          {#each mPreview.skipped as s, i (i)}
            <li><span class="pill">{s.kind}</span> <b>{s.name}</b>: <span class="meta">{s.reason}</span></li>
          {/each}
        </ul>
      {/if}

      <label class="check-row">
        <input type="checkbox" bind:checked={mPause} />
        <span>Create all checks paused (useful while the old tool still runs, so you don't get every alert twice)</span>
      </label>
      <div class="actions">
        <button type="button" class="btn btn-primary" disabled={mApplying || !mSel.size} onclick={importMonitors}>
          <Icon name="download" /> {mApplying ? 'Importing…' : `Import ${mSel.size} selected`}
        </button>
        <button type="button" class="btn" disabled={mApplying} onclick={() => (mPreview = null)}>Cancel</button>
      </div>
    {/if}
    {#if mResults}
      <ul class="results">
        {#each mResults as r, i (i)}
          <li class:ok={r.ok} class:fail={!r.ok}>
            <Icon name={r.ok ? 'check' : 'x'} /> <span class="pill">{r.kind}</span> <b>{r.name}</b>{#if r.ok}: created{:else}: {r.reason}{/if}
          </li>
        {/each}
      </ul>
    {/if}
    {#if mErr}<p class="error-text" role="alert">{mErr}</p>{/if}
  </section>

  <!-- Dockge -->
  <section class="card card-pad block">
    <div>
      <h2 class="section-title">Coming from Dockge?</h2>
      <p class="meta">
        Nothing to import: Moorage reads the same folder layout. Mount Dockge's stacks folder into Moorage at the <b>same path</b> it has on
        the host (usually <code>/opt/stacks:/opt/stacks</code>) and set <code>MOORAGE_STACKS_DIR</code> to it. Every folder with a compose
        file becomes a managed stack, running containers are recognised by their project name, and Moorage records a first version in its
        own history without changing any file. Dockge's <code>global.env</code> and <code>compose.override.yml</code> files aren't read.
      </p>
    </div>
    <div class="actions">
      <a class="btn btn-ghost" href={DOCS} target="_blank" rel="noopener noreferrer"><Icon name="external" /> Migration guide</a>
    </div>
  </section>
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .block {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    min-width: 0;
    gap: 14px;
  }
  .grid-form {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    gap: 12px 16px;
    align-items: end;
  }
  .grid-form .check-row,
  .grid-form .actions {
    grid-column: 1 / -1;
  }
  .field-wrap {
    display: grid;
    gap: 5px;
    font-size: 13px;
    color: var(--mist);
    align-content: start;
    min-width: 0;
  }
  .check-row {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    color: var(--foam);
    cursor: pointer;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .sub {
    font-size: 13px;
    font-weight: 600;
    margin: 4px 0 0;
    color: var(--foam);
  }
  .rows {
    display: grid;
    gap: 6px;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .row {
    display: grid;
    gap: 4px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--rope);
    min-width: 0;
  }
  .row.off {
    opacity: 0.75;
  }
  .row-head {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    min-width: 0;
    font-size: 13px;
  }
  .target {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 100%;
  }
  .rename {
    display: grid;
    grid-template-columns: auto minmax(0, 260px);
    gap: 8px;
    align-items: center;
    padding-left: 24px;
  }
  @media (max-width: 560px) {
    .rename {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .reason {
    margin: 0;
    padding-left: 24px;
    font-size: 12.5px;
    color: var(--adrift-text);
  }
  .warn-line {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    margin: 0;
    padding-left: 24px;
    font-size: 12.5px;
    color: var(--signal-text);
  }
  .row > .meta {
    margin: 0;
    padding-left: 24px;
  }
  details {
    padding-left: 24px;
  }
  .code {
    max-height: 260px;
    overflow: auto;
    margin: 6px 0 0;
    padding: 8px 10px;
    border-radius: var(--r-md);
    background: var(--well);
    font-size: 12px;
    white-space: pre;
  }
  .skipped,
  .results {
    display: grid;
    gap: 4px;
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: 13px;
  }
  .results li {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .results .ok {
    color: var(--afloat-text);
  }
  .results .fail {
    color: var(--adrift-text);
  }
  .file-pick {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 2px 10px 2px 3px;
    min-height: 36px;
    cursor: pointer;
    overflow: hidden;
  }
  .pick-btn {
    height: 28px;
    padding: 0 10px;
    font-size: 12.5px;
    flex-shrink: 0;
  }
  .file-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 13px;
    color: var(--foam);
  }
  .file-name.empty {
    color: var(--haze);
  }
  .pill {
    display: inline-flex;
    align-items: center;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .pill.on {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .pill.warn {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .pill.bad-pill {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
</style>
