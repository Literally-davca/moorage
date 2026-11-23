<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type RemoteTemplateSource } from '../lib/api';
  import { app, isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';

  let sources = $state<RemoteTemplateSource[] | null>(null);
  let dir = $state('');
  let error = $state('');

  let url = $state('');
  let allowLocal = $state(false);
  let adding = $state(false);
  let addError = $state('');

  let removing = $state<RemoteTemplateSource | null>(null);
  let confirmOpen = $state(false);
  let rowError = $state<Record<number, string>>({});

  async function load() {
    try {
      const r = await api.templateSources();
      sources = r.sources;
      dir = r.dir;
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  // Poll faster while an index is being fetched in the background.
  const anyRefreshing = $derived((sources ?? []).some((s) => s.refreshing));
  $effect(() => {
    const fast = anyRefreshing;
    untrack(() => void load());
    const t = setInterval(() => !document.hidden && void load(), fast ? 2000 : 30000);
    return () => clearInterval(t);
  });

  async function add(e: SubmitEvent) {
    e.preventDefault();
    adding = true;
    addError = '';
    try {
      await api.addTemplateSource(url.trim(), allowLocal);
      url = '';
      allowLocal = false;
      await load();
    } catch (err) {
      addError = (err as Error).message;
    } finally {
      adding = false;
    }
  }

  async function refresh(s: RemoteTemplateSource) {
    rowError = { ...rowError, [s.id]: '' };
    try {
      await api.refreshTemplateSource(s.id);
      await load();
    } catch (err) {
      rowError = { ...rowError, [s.id]: (err as Error).message };
    }
  }

  async function toggleLocal(s: RemoteTemplateSource) {
    rowError = { ...rowError, [s.id]: '' };
    try {
      await api.saveTemplateSource(s.id, s.url, !s.allowLocal);
      await load();
    } catch (err) {
      rowError = { ...rowError, [s.id]: (err as Error).message };
    }
  }

  async function remove() {
    const s = removing;
    if (!s) return;
    try {
      await api.deleteTemplateSource(s.id);
      await load();
    } catch (err) {
      rowError = { ...rowError, [s.id]: (err as Error).message };
    }
  }

  const when = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  const fmt = (iso?: string) => (iso ? when.format(new Date(iso)) : '');

  function status(s: RemoteTemplateSource): { cls: string; text: string } {
    if (s.refreshing) return { cls: 'degraded', text: 'Fetching…' };
    if (s.lastError) return { cls: 'down', text: s.lastOk ? 'Last refresh failed' : 'Failed' };
    if (!s.lastAttempt) return { cls: '', text: 'Not fetched yet' };
    return { cls: 'up', text: `${s.count} template${s.count === 1 ? '' : 's'}` };
  }
</script>

<div class="page">
  <section class="card card-pad head">
    <h1 class="title">Template sources</h1>
    <p class="meta">
      Moorage has no central catalogue. Templates come from the bundled examples, from a local folder you control, and from
      indexes you add here by HTTPS URL. A template is only a compose file: you always review it before a stack is created.
    </p>
    <a class="meta" href={'#/h/' + encodeURIComponent(app.host) + '/templates'}>Open the catalogue</a>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  <section class="card card-pad grid1" aria-label="Local folder">
    <p class="section-title">Local folder</p>
    <p class="meta">
      One subfolder per template with <code>template.yaml</code>, <code>compose.yaml</code> and an optional <code>.env</code>.
      Copy or <code>git clone</code> a template repository there yourself; Moorage only reads it. Set the path with
      <code>MOORAGE_TEMPLATES_DIR</code>.
    </p>
    {#if dir}<p class="mono path">{dir}</p>{/if}
  </section>

  <section class="card card-pad grid1" aria-label="Remote indexes">
    <p class="section-title">Remote indexes</p>
    {#if sources === null}
      <span class="skeleton" style="height:12px;width:220px;display:block"></span>
    {:else if sources.length === 0}
      <p class="meta">No index added yet.</p>
    {:else}
      <ul class="list">
        {#each sources as s (s.id)}
          {@const st = status(s)}
          <li class="item">
            <div class="item-main">
              <p class="name">{s.name || s.host}</p>
              <p class="mono url">{s.url}</p>
              <p class="meta">
                <span class="badge {st.cls}">{st.text}</span>
                {#if s.lastOk} · last good copy {fmt(s.lastOk)}{/if}
                {#if s.allowLocal} · local network allowed{/if}
              </p>
              {#if s.lastError}<p class="error-text">{s.lastError}{s.lastOk ? ' The last good copy is still used.' : ''}</p>{/if}
              {#if rowError[s.id]}<p class="error-text" role="alert">{rowError[s.id]}</p>{/if}
            </div>
            {#if isAdmin()}
              <div class="item-actions">
                <button type="button" class="btn" onclick={() => refresh(s)} disabled={s.refreshing}><Icon name="refresh" /> Refresh</button>
                <button type="button" class="btn btn-ghost" onclick={() => toggleLocal(s)}>
                  {s.allowLocal ? 'Block local network' : 'Allow local network'}
                </button>
                <button type="button" class="btn btn-ghost btn-danger btn-icon" aria-label="Remove index" title="Remove index"
                  onclick={() => { removing = s; confirmOpen = true; }}><Icon name="trash" /></button>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}

    {#if isAdmin()}
      <form class="add" onsubmit={add}>
        <label class="label" for="srcurl">Add an index</label>
        <input id="srcurl" class="field mono" bind:value={url} placeholder="https://example.com/moorage-templates/index.json"
          spellcheck="false" autocomplete="off" required />
        <label class="checkbox">
          <input type="checkbox" bind:checked={allowLocal} />
          <span>Allow local network: the index may live on a private or loopback address (a server in your LAN).</span>
        </label>
        <p class="hint">
          HTTPS only, at most 200 templates and 1 MiB per file. Indexes are refreshed daily; a failing index keeps its last good copy.
        </p>
        {#if addError}<p class="error-text" role="alert">{addError}</p>{/if}
        <div class="row">
          <button class="btn btn-primary" disabled={adding || !url.trim()} aria-busy={adding}><Icon name="plus" /> Add index</button>
        </div>
      </form>
    {/if}
  </section>
</div>

<Confirm bind:open={confirmOpen} title="Remove this index?" danger confirmLabel="Remove"
  body={`Its templates disappear from the catalogue. Stacks already created from them are not changed.`}
  onConfirm={remove} />

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 920px;
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
  .path,
  .url {
    font-size: 12px;
    color: var(--mist);
    overflow-wrap: anywhere;
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
  .item-main .error-text {
    overflow-wrap: anywhere;
  }
  .name {
    font-weight: 600;
    font-size: 14px;
    overflow-wrap: anywhere;
  }
  .item-actions {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
    align-items: flex-start;
  }
  .add {
    display: grid;
    gap: 8px;
    padding-top: 12px;
    border-top: 1px solid var(--rope-strong);
  }
  .row {
    display: flex;
    justify-content: flex-end;
  }
  .btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
</style>
