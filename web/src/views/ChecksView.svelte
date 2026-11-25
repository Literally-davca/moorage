<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Check } from '../lib/api';
  import { isAdmin, stackLabel } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Strip from '../lib/Strip.svelte';
  import { checkStatusLabel, kindName, pct, targetCount } from '../lib/checks';
  import { untilText } from '../lib/maintenance';

  let checks = $state<Check[] | null>(null);
  let error = $state('');

  async function load() {
    try {
      checks = await api.checks();
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(load);
    const t = setInterval(() => !document.hidden && load(), 10000);
    return () => clearInterval(t);
  });

  // problems first, then by name
  const order: Record<string, number> = { down: 0, degraded: 1, pending: 2, maintenance: 3, unknown: 4, up: 5, paused: 6 };
  const sorted = $derived(
    [...(checks ?? [])].sort((a, b) => (order[a.state.status] ?? 9) - (order[b.state.status] ?? 9) || a.name.localeCompare(b.name)),
  );
  const down = $derived((checks ?? []).filter((c) => c.state.status === 'down' || c.state.status === 'degraded').length);
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Checks</h1>
      <p class="meta">
        {#if checks}
          {checks.length} {checks.length === 1 ? 'check' : 'checks'}{#if down}<span class="bad">{` · ${down} failing`}</span>{:else if checks.length} · all fine{/if}
        {:else}Loading…{/if}
      </p>
    </div>
    {#if isAdmin()}
      <div class="head-actions">
        <a class="btn btn-ghost" href="#/settings/notifications">Notifications</a>
        <a class="btn btn-primary" href="#/checks/new"><Icon name="plus" /> Add check</a>
      </div>
    {/if}
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if checks && checks.length === 0}
    <section class="card card-pad empty">
      <p class="section-title">No checks yet</p>
      <p class="meta">
        Checks watch your services from the outside: a web page, a port, a DNS name, or whether a stack's containers keep
        running. When one fails, the alert carries the stack's last log lines.
      </p>
      {#if isAdmin()}<a class="btn btn-primary" href="#/checks/new"><Icon name="plus" /> Add the first check</a>{/if}
    </section>
  {:else if checks}
    <section class="card list" aria-label="Checks">
      {#each sorted as c (c.id)}
        <a class="row" href={'#/checks/' + c.id}>
          <span class="dot {c.state.status}" title={checkStatusLabel[c.state.status]}></span>
          <span class="who">
            <span class="name">{c.name}</span>
            <span class="target meta">
              <span class="kind">{kindName[c.kind]}</span>
              <span class="mono">{c.kind === 'log' ? c.settings.pattern : (c.kind === 'stack' ? (c.targetDisplayName || stackLabel(c.hostId, c.target)) : c.target)}</span>
              {#if targetCount(c) > 1}<span>+{targetCount(c) - 1} more</span>{/if}
              {#if c.kind === 'log' && c.target}<span>in <span class="mono">{c.target}</span></span>{/if}
              {#if c.stack && c.kind !== 'stack'}<span>· {stackLabel(c.hostId, c.stack, c.targetDisplayName)}{c.hostName ? ` on ${c.hostName}` : ''}</span>{/if}
              {#if c.kind === 'stack' && c.hostName}<span>· on {c.hostName}</span>{/if}
            </span>
            {#if c.state.status === 'down' || c.state.status === 'degraded' || c.state.status === 'pending'}
              <span class="why">{c.state.last?.msg}</span>
            {:else if c.maintenance}
              <span class="maint">Maintenance until {untilText(c.maintenance.until)} · {c.maintenance.title}</span>
            {/if}
          </span>
          <span class="strip"><Strip ticks={c.ticks} /></span>
          <span class="nums">
            <span class="up num">{pct(c.uptime24h)}</span>
            <span class="meta num">{c.state.last ? `${c.state.last.latencyMs} ms` : '–'}</span>
          </span>
        </a>
      {/each}
    </section>
  {:else}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:50%"></div></section>
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 16px;
    flex-wrap: wrap;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .head-actions {
    display: flex;
    gap: 8px;
  }
  .bad {
    color: var(--adrift-text);
  }
  .empty {
    display: grid;
    gap: 8px;
    justify-items: start;
  }
  .empty .meta {
    max-width: 62ch;
  }
  .list {
    padding: 4px 0;
  }
  .row {
    display: grid;
    grid-template-columns: 8px minmax(180px, 1fr) minmax(120px, 340px) 76px;
    gap: 16px;
    align-items: center;
    padding: 12px 20px;
    color: inherit;
    text-decoration: none;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .row:hover {
    background: var(--rope);
    text-decoration: none;
  }
  .who {
    display: grid;
    gap: 2px;
    min-width: 0;
  }
  .name {
    font-weight: 500;
    color: var(--foam);
  }
  .target {
    display: flex;
    gap: 6px;
    font-size: 12px;
    min-width: 0;
    white-space: nowrap;
    overflow: hidden;
  }
  .target .mono {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .kind {
    color: var(--mist);
    flex: none;
  }
  .why {
    font-size: 12px;
    color: var(--adrift-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .maint {
    font-size: 12px;
    color: var(--signal-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .strip {
    min-width: 0;
  }
  .nums {
    display: grid;
    justify-items: end;
    gap: 2px;
  }
  .up {
    font-weight: 600;
    color: var(--foam);
  }
  .nums .meta {
    font-size: 12px;
  }

  @media (max-width: 767px) {
    .row {
      grid-template-columns: 8px minmax(0, 1fr) 64px;
      gap: 8px 12px;
      padding: 12px 14px;
    }
    .strip {
      grid-column: 2 / 4;
      grid-row: 2;
    }
  }
</style>
