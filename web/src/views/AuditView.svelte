<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type AuditEntry, type TerminalAuditRow } from '../lib/api';
  import { app, stackLabel } from '../lib/state.svelte';
  import { auditGroups } from '../lib/audit';
  import AuditList from '../lib/AuditList.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';

  const pageSize = 100;

  let entries = $state<AuditEntry[] | null>(null);
  let termSessions = $state<TerminalAuditRow[]>([]);
  let more = $state(false);
  let error = $state('');
  let loadingMore = $state(false);

  // Filters. Text fields apply on submit, the selects right away.
  let actor = $state('');
  let stack = $state('');
  let groups = $state<string[]>([]);
  let hostIds = $state<string[]>([]);

  const filter = () => ({
    host: hostIds.join(','),
    stack: stack.trim(),
    actor: actor.trim(),
    action: groups.join(','),
    limit: pageSize,
  });

  async function load() {
    error = '';
    try {
      const [r, ts] = await Promise.all([
        api.audit(filter()),
        api.terminalSessions().catch(() => []),
      ]);
      entries = r.entries;
      more = r.more;
      termSessions = ts ?? [];
    } catch (e) {
      error = (e as Error).message;
      entries = entries ?? [];
    }
  }

  function fmtTermDuration(row: TerminalAuditRow): string {
    if (!row.endedAt || row.endedAt.startsWith('0001-')) return 'active';
    const sec = Math.max(0, Math.round((new Date(row.endedAt).getTime() - new Date(row.startedAt).getTime()) / 1000));
    if (sec < 60) return `${sec} s`;
    if (sec < 3600) return `${Math.floor(sec / 60)} m ${sec % 60} s`;
    return `${Math.floor(sec / 3600)} h ${Math.floor((sec % 3600) / 60)} m`;
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
    actor = stack = '';
    groups = [];
    hostIds = [];
    load();
  }

  $effect(() => {
    untrack(load);
  });

  const filtered = $derived(!!(actor.trim() || stack.trim() || groups.length || hostIds.length));
</script>

<div class="page">
  <section class="card card-pad head">
    <h1 class="title">Audit log</h1>
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
      <div>
        <span class="label">Hosts</span>
        <MultiSelect label="Hosts" placeholder="All hosts" bind:selected={hostIds} onchange={load}
          items={app.hosts.map((h) => ({ value: h.id, label: h.name }))} />
      </div>
    {/if}
    <div>
      <span class="label">Actions</span>
      <MultiSelect label="Actions" placeholder="All actions" bind:selected={groups} onchange={load}
        items={auditGroups.map((g) => ({ value: g.prefix, label: g.name }))} />
    </div>
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

  {#if termSessions.length > 0}
    <section class="card card-pad term-audit" aria-label="Terminal sessions">
      <h2 class="section-title">Terminal sessions</h2>
      <div class="term-table-wrap">
        <table class="term-table">
          <thead>
            <tr>
              <th>Started</th>
              <th>User · IP</th>
              <th>Host · Stack</th>
              <th>Container · Shell</th>
              <th>Duration</th>
              <th>Ended</th>
            </tr>
          </thead>
          <tbody>
            {#each termSessions as ts (ts.id)}
              <tr>
                <td class="mono">{new Date(ts.startedAt).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</td>
                <td>
                  <span class="foam">{ts.user}</span>
                  <span class="meta mono"> · {ts.ip}</span>
                </td>
                <td>
                  <a href={`#/h/${encodeURIComponent(ts.hostId)}/stacks/${encodeURIComponent(ts.stack)}`}>
                    {ts.hostName || ts.hostId} / {stackLabel(ts.hostId, ts.stack)}
                  </a>
                </td>
                <td class="mono">{ts.containerName} · {ts.shell}</td>
                <td class="mono">{fmtTermDuration(ts)}</td>
                <td>
                  <span class="tag {ts.reason === 'revoked' || ts.reason === 'error' ? 'ro' : ''}">
                    {ts.reason || 'open'}{ts.exitCode !== undefined ? ` (${ts.exitCode})` : ''}
                  </span>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
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
  .term-table-wrap {
    overflow-x: auto;
    margin-top: 8px;
  }
  .term-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12px;
  }
  .term-table th,
  .term-table td {
    text-align: left;
    padding: 7px 8px;
    border-bottom: 1px solid var(--rope);
    white-space: nowrap;
  }
  .term-table th {
    color: var(--haze);
    font-weight: 500;
  }
</style>
