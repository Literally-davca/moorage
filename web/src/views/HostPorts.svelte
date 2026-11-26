<script lang="ts">
  // Port map of one host (M17B, moved from M15): every taken host port – stacks, standalone
  // containers, ports reserved by stopped stacks and, in native / host-network mode, the
  // host's own listening sockets. Read on request, no background polling.
  import { untrack } from 'svelte';
  import Icon from '../lib/Icon.svelte';
  import { hostnet, netModeNote, portOwner, type PortEntry, type PortReport, type PortSource } from '../lib/hostnet';

  let { hostId, hostName, onClose }: { hostId: string; hostName: string; onClose: () => void } = $props();

  let report = $state<PortReport | null>(null);
  let loading = $state(false);
  let error = $state('');
  let query = $state('');
  let source = $state<PortSource | 'all'>('all');

  const sources: { k: PortSource | 'all'; label: string }[] = [
    { k: 'all', label: 'All' },
    { k: 'stack', label: 'Stacks' },
    { k: 'container', label: 'Containers' },
    { k: 'compose', label: 'Reserved' },
    { k: 'host', label: 'Host' },
  ];

  async function load(fresh = false) {
    loading = true;
    error = '';
    try {
      report = await hostnet.ports(hostId, fresh);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void hostId;
    untrack(() => load());
  });

  const counts = $derived.by(() => {
    const c: Record<string, number> = { all: report?.ports.length ?? 0 };
    for (const p of report?.ports ?? []) c[p.source] = (c[p.source] ?? 0) + 1;
    return c;
  });

  const rows = $derived.by(() => {
    const q = query.trim().toLowerCase();
    return (report?.ports ?? []).filter((p) => {
      if (source !== 'all' && p.source !== source) return false;
      if (!q) return true;
      return [String(p.port), p.proto, p.ip ?? '', p.stack ?? '', p.service ?? '', p.container ?? ''].join(' ').toLowerCase().includes(q);
    });
  });

  function sourceLabel(p: PortEntry): string {
    switch (p.source) {
      case 'stack':
        return 'Stack';
      case 'container':
        return 'Container';
      case 'compose':
        return 'compose.yaml';
      default:
        return 'Host';
    }
  }

  function stateLabel(p: PortEntry): string {
    if (p.state === 'reserved') return 'Reserved (stopped)';
    if (p.state === 'listening') return 'Listening';
    if (p.state === 'running') return 'Running';
    return p.state.charAt(0).toUpperCase() + p.state.slice(1);
  }
</script>

<section class="card card-pad ports" aria-label={`Ports of ${hostName}`}>
  <div class="head-bar">
    <h2 class="section-title">Ports · {hostName}</h2>
    <span class="grow"></span>
    <button type="button" class="btn btn-ghost" onclick={() => load(true)} disabled={loading} aria-busy={loading}>
      <Icon name="refresh" /> Refresh
    </button>
    <button type="button" class="btn btn-ghost" onclick={onClose}><Icon name="x" /> Close</button>
  </div>

  {#if report}
    <p class="meta note">
      <Icon name="info" />
      <span>
        {netModeNote(report.netMode)}
        {#if report.hostNetwork?.length}
          Containers on the host network ({report.hostNetwork.join(', ')}) have no port mappings{report.native ? '; their sockets show up as Host.' : '.'}
        {/if}
      </span>
    </p>

    <div class="tools">
      <div class="chips" role="radiogroup" aria-label="Filter by source">
        {#each sources as s (s.k)}
          {#if s.k === 'all' || counts[s.k]}
            <button type="button" role="radio" aria-checked={source === s.k} class="chip" class:on={source === s.k} onclick={() => (source = s.k)}>
              {s.label} <span class="n">{counts[s.k] ?? 0}</span>
            </button>
          {/if}
        {/each}
      </div>
      <input class="field search" type="search" placeholder="Filter by port, stack or container" aria-label="Filter ports" bind:value={query} />
    </div>

    {#if rows.length === 0}
      <p class="meta empty">{report.ports.length ? 'No port matches the filter.' : 'No published ports on this host.'}</p>
    {:else}
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th class="num">Port</th>
              <th>Bind address</th>
              <th>Used by</th>
              <th>Source</th>
              <th>State</th>
            </tr>
          </thead>
          <tbody>
            {#each rows as p (p.source + p.proto + (p.ip ?? '') + p.port + (p.stack ?? '') + (p.container ?? p.service ?? ''))}
              <tr class:reserved={p.state === 'reserved'}>
                <td class="num mono">
                  {p.port}<span class="proto">/{p.proto}</span>
                  {#if p.target && p.target !== p.port}<span class="target">→ {p.target}</span>{/if}
                </td>
                <td class="mono">{p.ip || 'All interfaces'}</td>
                <td>
                  {#if p.stack}
                    <a href={'#/h/' + encodeURIComponent(hostId) + '/stacks/' + encodeURIComponent(p.stack)}>{portOwner(p)}</a>
                  {:else}
                    {portOwner(p)}
                  {/if}
                </td>
                <td class="meta">{sourceLabel(p)}</td>
                <td><span class="state {p.state}">{stateLabel(p)}</span></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  {:else if loading}
    <div class="skeleton" style="height:14px;width:45%"></div>
  {/if}
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
</section>

<style>
  .ports {
    display: grid;
    gap: 12px;
  }
  .head-bar {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .grow {
    flex: 1;
  }
  .note {
    display: flex;
    gap: 8px;
    align-items: flex-start;
    text-wrap: pretty;
  }
  .tools {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    align-items: center;
  }
  .search {
    max-width: 280px;
    height: 32px;
  }
  .chips {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .chip {
    height: 28px;
    padding: 0 10px;
    border: 0;
    border-radius: 999px;
    background: transparent;
    box-shadow: inset 0 0 0 1px var(--rope-stronger);
    color: var(--mist);
    font: 500 12px var(--font);
    cursor: pointer;
  }
  .chip:hover {
    background: var(--rope);
    color: var(--foam);
  }
  .chip.on {
    background: var(--buoy-wash);
    box-shadow: inset 0 0 0 1px var(--buoy);
    color: var(--foam);
  }
  .chip:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: 2px;
  }
  .chip .n {
    color: var(--haze);
    font-variant-numeric: tabular-nums;
    margin-left: 2px;
  }
  .table-wrap {
    overflow-x: auto;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 13px;
  }
  th {
    text-align: left;
    font-size: 11px;
    font-weight: 500;
    color: var(--haze);
    padding: 6px 10px;
    box-shadow: inset 0 -1px 0 var(--rope-strong);
  }
  td {
    padding: 7px 10px;
    color: var(--mist);
    box-shadow: inset 0 -1px 0 var(--rope);
    vertical-align: top;
  }
  td a {
    color: var(--foam);
  }
  .num {
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .proto {
    color: var(--haze);
  }
  .target {
    color: var(--murk);
    margin-left: 6px;
  }
  tr.reserved td {
    color: var(--haze);
  }
  .state {
    font-size: 12px;
  }
  .state.running {
    color: var(--afloat-text);
  }
  .state.listening {
    color: var(--info-text);
  }
  .state.reserved {
    color: var(--signal-text);
  }
  .empty {
    padding: 8px 0;
  }
</style>
