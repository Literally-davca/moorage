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
    untrack(() => void load(false));
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
</script>

<div class="page">
  <section class="card card-pad head">
    <div class="head-row">
      <div>
        <h2 class="section-title">Add-ons &amp; integrations</h2>
        <p class="meta">
          Moorage keeps its core universal. Optional integrations and sidecar add-ons live in
          <a href="https://github.com/Literally-davca/moorage-addons" target="_blank" rel="noopener noreferrer">moorage-addons</a>
          with estimated RAM and CPU usage.
        </p>
      </div>
      <button type="button" class="btn" onclick={() => load(true, catalog !== null)} disabled={loadingCatalog}>
        <Icon name="refresh" /> {catalog === null ? 'Browse catalog' : loadingCatalog ? 'Refreshing…' : 'Refresh catalog'}
      </button>
    </div>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if addons === null && !error}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:45%"></div></section>
  {:else if addons}
    <div class="list">
      {#each addons as a (a.id)}
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

  {#if catalogError}
    <p class="error-text" role="alert">Catalog: {catalogError}</p>
  {/if}

  {#if catalog !== null}
    <section class="card card-pad catalog-head">
      <div class="head-row">
        <div>
          <h2 class="section-title">Available add-ons</h2>
          <p class="meta">Sidecar add-ons are deployed as isolated stacks (<code>addon-&lt;id&gt;</code>) on the chosen host.</p>
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

    {#if available.length === 0}
      <section class="card card-pad"><p class="meta">Every add-on from the catalog is already installed.</p></section>
    {:else}
      <div class="list">
        {#each available as c (c.id)}
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
    align-items: flex-start;
    gap: 12px;
    flex-wrap: wrap;
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
