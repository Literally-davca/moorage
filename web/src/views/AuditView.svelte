<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type AuditEntry } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import { auditGroups } from '../lib/audit';
  import AuditList from '../lib/AuditList.svelte';

  const pageSize = 100;

  let entries = $state<AuditEntry[] | null>(null);
  let more = $state(false);
  let error = $state('');
  let loadingMore = $state(false);

  // Filters. Text fields apply on submit, the selects right away.
  let actor = $state('');
  let stack = $state('');
  let group = $state('');
  let hostId = $state('');

  const filter = () => ({
    host: hostId,
    stack: stack.trim(),
    actor: actor.trim(),
    action: group,
    limit: pageSize,
  });

  async function load() {
    error = '';
    try {
      const r = await api.audit(filter());
      entries = r.entries;
      more = r.more;
    } catch (e) {
      error = (e as Error).message;
      entries = entries ?? [];
    }
  }

  async function loadMore() {
    const last = entries?.at(-1);
    if (!last) return;
    loadingMore = true;
    try {
      const r = await api.audit({ ...filter(), before: last.id });
      entries = [...(entries ?? []), ...r.entries];
      more = r.more;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loadingMore = false;
    }
  }

  function clear() {
    actor = stack = group = hostId = '';
    load();
  }

  $effect(() => {
    untrack(load);
  });

  const filtered = $derived(!!(actor.trim() || stack.trim() || group || hostId));
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Audit log</h1>
      <p class="meta">
        Every change made through Moorage: who, when, from which IP and whether it worked. Kept for 1 year. Request
        contents such as passwords or compose files are never stored.
      </p>
    </div>
  </section>

  <form class="card card-pad filters" onsubmit={(e) => { e.preventDefault(); load(); }} aria-label="Filter the audit log">
    <label>
      <span class="label">Who</span>
      <input class="field" bind:value={actor} placeholder="Username or system" spellcheck="false" autocomplete="off" />
    </label>
    <label>
      <span class="label">Stack</span>
      <input class="field" bind:value={stack} placeholder="Any stack" spellcheck="false" autocomplete="off" />
    </label>
    {#if app.hosts.length > 1}
      <label>
        <span class="label">Host</span>
        <select class="field" bind:value={hostId} onchange={load}>
          <option value="">All hosts</option>
          {#each app.hosts as h (h.id)}<option value={h.id}>{h.name}</option>{/each}
        </select>
      </label>
    {/if}
    <label>
      <span class="label">Action</span>
      <select class="field" bind:value={group} onchange={load}>
        <option value="">All actions</option>
        {#each auditGroups as g (g.prefix)}<option value={g.prefix}>{g.name}</option>{/each}
      </select>
    </label>
    <div class="fbtns">
      <button class="btn">Filter</button>
      {#if filtered}<button type="button" class="btn btn-ghost" onclick={clear}>Clear</button>{/if}
    </div>
  </form>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if entries === null}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:50%"></div></section>
  {:else if entries.length === 0}
    <section class="card card-pad">
      <p class="meta">{filtered ? 'No entries match these filters.' : 'Nothing recorded yet.'}</p>
    </section>
  {:else}
    <section class="card list" aria-label="Audit entries">
      <AuditList {entries} />
      {#if more}
        <div class="more">
          <button class="btn" onclick={loadMore} disabled={loadingMore} aria-busy={loadingMore}>Load more</button>
        </div>
      {/if}
    </section>
  {/if}
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
  .head .meta {
    max-width: 70ch;
    margin-top: 4px;
  }
  .filters {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
    gap: 12px;
    align-items: end;
  }
  .fbtns {
    display: flex;
    gap: 8px;
  }
  .list {
    padding: 4px 0;
    min-width: 0;
  }
  .more {
    display: flex;
    justify-content: center;
    padding: 12px;
    border-top: 1px solid var(--rope);
  }
</style>
