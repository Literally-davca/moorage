<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type AddonCatalogEntry, type AddonInfo } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';

  let addons = $state<AddonInfo[] | null>(null);
  let catalog = $state<AddonCatalogEntry[] | null>(null);
  let catalogError = $state('');
  let loadingCatalog = $state(false);
  let error = $state('');
  let busy = $state<Record<string, boolean>>({});
  let rowError = $state<Record<string, string>>({});
  let targetHost = $state<string>('');
  let removing = $state<AddonInfo | null>(null);
  let confirmOpen = $state(false);

  // filters
  let statusFilter = $state<'all' | 'installed' | 'enabled' | 'disabled' | 'not_installed'>('all');
  let kindFilter = $state<'all' | 'builtin' | 'sidecar'>('all');
  let categoryFilter = $state<string>('all');
  let search = $state('');

  const writableHosts = $derived(app.hosts.filter((h) => h.connected && !h.readOnly));

  $effect(() => {
    if (!targetHost && writableHosts.length) {
      targetHost = writableHosts.find((h) => h.id === app.host)?.id ?? writableHosts[0].id;
    }
  });

  async function load(withCatalog = false, refresh = false) {
    if (withCatalog) loadingCatalog = true;
    try {
      const r = await api.addons({ catalog: withCatalog || catalog !== null, refresh });
      addons = r.addons;
      if (r.catalog) catalog = r.catalog;
      catalogError = r.catalogError ?? '';
      error = '';
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loadingCatalog = false;
    }
  }

  $effect(() => {
    untrack(() => void load(true));
  });

  async function toggle(a: AddonInfo) {
    if (!a.toggleable) return;
    busy = { ...busy, [a.id]: true };
    rowError = { ...rowError, [a.id]: '' };
    try {
      await api.setAddon(a.id, !a.enabled);
      await load();
    } catch (e) {
      rowError = { ...rowError, [a.id]: (e as Error).message };
    } finally {
      busy = { ...busy, [a.id]: false };
    }
  }

  async function install(entry: AddonCatalogEntry) {
    busy = { ...busy, [entry.id]: true };
    rowError = { ...rowError, [entry.id]: '' };
    try {
      await api.installAddon(entry.id, entry.kind === 'sidecar' ? targetHost : undefined);
      await load();
    } catch (e) {
      rowError = { ...rowError, [entry.id]: (e as Error).message };
    } finally {
      busy = { ...busy, [entry.id]: false };
    }
  }

  async function uninstall() {
    const a = removing;
    if (!a) return;
    busy = { ...busy, [a.id]: true };
    rowError = { ...rowError, [a.id]: '' };
    try {
      await api.uninstallAddon(a.id);
      await load();
    } catch (e) {
      rowError = { ...rowError, [a.id]: (e as Error).message };
    } finally {
      busy = { ...busy, [a.id]: false };
      removing = null;
    }
  }

  const installedIds = $derived(new Set((addons ?? []).map((a) => a.id)));
  const available = $derived((catalog ?? []).filter((c) => c.kind === 'sidecar' && !installedIds.has(c.id)));
  const categories = $derived.by(() => {
    const s = new Set<string>();
    for (const a of addons ?? []) if (a.category) s.add(a.category);
    for (const c of catalog ?? []) if (c.category) s.add(c.category);
    return [...s].sort();
  });

  const q = $derived(search.trim().toLowerCase());
  const filteredInstalled = $derived(
    (addons ?? []).filter((a) => {
      if (statusFilter === 'not_installed') return false;
      if (statusFilter === 'enabled' && !a.enabled) return false;
      if (statusFilter === 'disabled' && a.enabled) return false;
      if (kindFilter !== 'all' && a.kind !== kindFilter) return false;
      if (categoryFilter !== 'all' && a.category !== categoryFilter) return false;
      if (q && !`${a.name} ${a.description} ${a.category}`.toLowerCase().includes(q)) return false;
      return true;
    }),
  );
  const filteredAvailable = $derived(
    available.filter((c) => {
      if (statusFilter === 'installed' || statusFilter === 'enabled' || statusFilter === 'disabled') return false;
      if (kindFilter !== 'all' && c.kind !== kindFilter) return false;
      if (categoryFilter !== 'all' && c.category !== categoryFilter) return false;
      if (q && !`${c.name} ${c.description} ${c.category}`.toLowerCase().includes(q)) return false;
      return true;
    }),
  );
  const hasFilters = $derived(statusFilter !== 'all' || kindFilter !== 'all' || categoryFilter !== 'all' || q !== '');
  function resetFilters() {
    statusFilter = 'all';
    kindFilter = 'all';
    categoryFilter = 'all';
    search = '';
  }
</script>

<div class="page">
  <section class="card card-pad head">
    <div class="head-row">
      <h2 class="section-title">Add-ons &amp; integrations</h2>
      <div class="head-actions">
        <a class="btn btn-ghost" href="https://github.com/Literally-davca/moorage-addons" target="_blank" rel="noopener noreferrer">
          Catalog <Icon name="external" />
        </a>
        <button type="button" class="btn" onclick={() => load(true, true)} disabled={loadingCatalog}>
          <Icon name="refresh" /> {loadingCatalog ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>
    </div>

    <div class="filters-bar">
      <input class="field search-in" bind:value={search} placeholder="Search add-ons…" aria-label="Search add-ons" />
      <select class="field" bind:value={statusFilter} aria-label="Filter by status">
        <option value="all">All statuses</option>
        <option value="installed">Installed</option>
        <option value="enabled">Enabled</option>
        <option value="disabled">Disabled</option>
        <option value="not_installed">Not installed</option>
      </select>
      <select class="field" bind:value={kindFilter} aria-label="Filter by type">
        <option value="all">All types</option>
        <option value="builtin">Built-in</option>
        <option value="sidecar">Sidecar</option>
      </select>
      <select class="field" bind:value={categoryFilter} aria-label="Filter by category">
        <option value="all">All categories</option>
        {#each categories as cat (cat)}
          <option value={cat}>{cat}</option>
        {/each}
      </select>
      {#if hasFilters}
        <button type="button" class="btn btn-ghost" onclick={resetFilters}>Reset</button>
      {/if}
    </div>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if addons === null && !error}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:45%"></div></section>
  {:else if addons}
    {#if filteredInstalled.length > 0}
      <div class="list">
        {#each filteredInstalled as a (a.id)}
        <section class="card card-pad addon">
          <div class="addon-row">
            <div class="addon-main">
              <div class="addon-title">
                <span class="name">{a.name}</span>
                <span class="badge">{a.category}</span>
                <span class="badge {a.enabled ? 'up' : ''}">{a.enabled ? 'Enabled' : 'Disabled'}</span>
              </div>
              <p class="meta">{a.description}</p>
              {#if a.ram || a.cpu}
                <p class="meta mono">
                  {#if a.ram}RAM: {a.ram}{/if}{#if a.ram && a.cpu} · {/if}{#if a.cpu}CPU: {a.cpu}{/if}
                </p>
              {/if}
              {#if a.detail}<p class="mono detail">{a.detail}</p>{/if}
              {#if a.linkHref && a.linkLabel}
                <p class="meta"><a href={a.linkHref}>{a.linkLabel}</a></p>
              {/if}
              {#if rowError[a.id]}<p class="error-text" role="alert">{rowError[a.id]}</p>{/if}
            </div>
            <div class="addon-actions">
              {#if a.toggleable}
                <button
                  type="button"
                  class="btn"
                  disabled={busy[a.id]}
                  aria-busy={busy[a.id]}
                  onclick={() => toggle(a)}
                >
                  {a.enabled ? 'Disable' : 'Enable'}
                </button>
              {/if}
              {#if a.removable}
                <button
                  type="button"
                  class="btn btn-ghost btn-danger"
                  disabled={busy[a.id]}
                  onclick={() => { removing = a; confirmOpen = true; }}
                >
                  <Icon name="trash" /> Uninstall
                </button>
              {/if}
            </div>
          </div>
        </section>
        {/each}
      </div>
    {/if}
  {/if}

  {#if catalogError}
    <p class="error-text" role="alert">Catalog: {catalogError}</p>
  {/if}

  {#if catalog !== null && filteredAvailable.length > 0}
    <section class="card card-pad catalog-head">
      <div class="head-row">
        <div>
          <h2 class="section-title">Available add-ons</h2>
          <p class="meta">Sidecar add-ons are added as services to the <code>add-ons</code> stack.</p>
        </div>
        {#if writableHosts.length > 1}
          <label class="host-pick">
            <span class="meta">Target host</span>
            <select class="field" bind:value={targetHost}>
              {#each writableHosts as h (h.id)}
                <option value={h.id}>{h.name}</option>
              {/each}
            </select>
          </label>
        {/if}
      </div>
    </section>

    <div class="list">
      {#each filteredAvailable as c (c.id)}
          <section class="card card-pad addon">
            <div class="addon-row">
              <div class="addon-main">
                <div class="addon-title">
                  <span class="name">{c.name}</span>
                  <span class="badge">{c.category}</span>
                  <span class="badge">{c.kind}</span>
                </div>
                <p class="meta">{c.description}</p>
                <p class="meta mono">RAM: {c.resources.ram} · CPU: {c.resources.cpu}</p>
                {#if c.readme}
                  <p class="meta">
                    <a href={`https://github.com/Literally-davca/moorage-addons/blob/main/${c.readme}`} target="_blank" rel="noopener noreferrer">
                      Read documentation <Icon name="external" />
                    </a>
                  </p>
                {/if}
                {#if rowError[c.id]}<p class="error-text" role="alert">{rowError[c.id]}</p>{/if}
              </div>
              <button
                type="button"
                class="btn"
                disabled={busy[c.id] || (c.kind === 'sidecar' && !targetHost)}
                aria-busy={busy[c.id]}
                onclick={() => install(c)}
              >
                <Icon name="plus" /> {busy[c.id] ? 'Installing…' : 'Install'}
              </button>
            </div>
          </section>
      {/each}
    </div>
  {/if}

  {#if addons && filteredInstalled.length === 0 && filteredAvailable.length === 0}
    <section class="card card-pad empty-row">
      <span class="meta">{hasFilters ? 'No add-ons match the selected filters.' : 'No add-ons available.'}</span>
      {#if hasFilters}
        <button type="button" class="btn" onclick={resetFilters}>Reset filters</button>
      {/if}
    </section>
  {/if}
</div>

<Confirm
  bind:open={confirmOpen}
  title={`Uninstall ${removing?.name ?? 'add-on'}?`}
  danger
  confirmLabel="Uninstall"
  body={removing?.kind === 'sidecar'
    ? `Takes down and deletes stack ${removing.stackName} on ${removing.hostId}.`
    : 'Removes this add-on configuration.'}
  onConfirm={uninstall}
/>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 920px;
  }
  .head,
  .catalog-head {
    display: grid;
    gap: 6px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .head-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .filters-bar {
    display: grid;
    grid-template-columns: minmax(160px, 1.4fr) repeat(3, minmax(120px, 1fr)) auto;
    gap: 8px;
    margin-top: 6px;
  }
  .empty-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  @media (max-width: 720px) {
    .filters-bar {
      grid-template-columns: 1fr 1fr;
    }
    .search-in {
      grid-column: 1 / -1;
    }
  }
  .head .meta {
    max-width: 70ch;
  }
  .host-pick {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .host-pick .field {
    width: auto;
    min-width: 150px;
    height: 32px;
  }
  .list {
    display: grid;
    gap: 10px;
  }
  .addon-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 16px;
    flex-wrap: wrap;
  }
  .addon-main {
    display: grid;
    gap: 6px;
    flex: 1 1 320px;
    min-width: 0;
  }
  .addon-title {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .addon-actions {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .name {
    font-weight: 600;
    font-size: 14px;
    color: var(--foam);
  }
  .detail {
    font-size: 12px;
    color: var(--mist);
    overflow-wrap: anywhere;
  }
</style>
