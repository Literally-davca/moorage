<script lang="ts">
  import { api } from '../lib/api';
  import { ago } from '../lib/checks';
  import { chat, ask } from '../lib/assistantStore.svelte';

  // Home → Problems (M19K): problems across hosts, worst first. Problems the admin
  // ignored don't count; an offline host shows as unknown, not as zero.
  type Item = {
    rule: string;
    severity: string;
    container: string;
    service?: string;
    title: string;
    since?: string;
    hostId: string;
    hostName: string;
    stack?: string;
    containerId?: string;
  };
  let data = $state<{ problems: Item[]; more: number; offline: string[] } | null>(null);

  async function load() {
    try {
      data = (await api.problemsSummary()) as { problems: Item[]; more: number; offline: string[] };
    } catch {
      data = null;
    }
  }
  $effect(() => {
    void load();
    const t = setInterval(load, 60_000);
    return () => clearInterval(t);
  });
  const assistantOn = $derived(!!chat.info?.enabled && !!chat.info?.configured);

  function href(p: Item) {
    if (p.stack) return `#/h/${encodeURIComponent(p.hostId)}/stacks/${encodeURIComponent(p.stack)}`;
    return `#/h/${encodeURIComponent(p.hostId)}`;
  }
</script>

{#if data && (data.problems.length || data.offline.length)}
  <section class="card card-pad" aria-label="Problems">
    <div class="sec-head">
      <h2 class="section-title">Problems</h2>
      <span class="meta">Across all hosts, worst first</span>
    </div>
    <ul class="rows">
      {#each data.problems as p (p.hostId + (p.stack ?? p.containerId) + p.container + p.rule)}
        <li class="prow">
          <span class="dot {p.severity === 'error' ? 'down' : p.severity === 'warning' ? 'degraded' : ''}"></span>
          <a href={href(p)} class="ptitle">{p.title}</a>
          <span class="meta">{p.service || p.container} · {p.stack ?? 'standalone'} · {p.hostName}{p.since ? ' · ' + ago(p.since) : ''}</span>
          {#if assistantOn}
            <button type="button" class="btn btn-ghost btn-sm" onclick={() => ask(`Why does ${p.service || p.container}${p.stack ? ' in ' + p.stack : ''} on ${p.hostName} have "${p.title}" and how do I fix it?`)}>Ask</button>
          {/if}
        </li>
      {/each}
      {#if data.more}<li class="meta">+{data.more} more</li>{/if}
      {#each data.offline as h (h)}
        <li class="meta">Host {h} offline – problems unknown</li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .sec-head {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 12px;
    margin-bottom: 8px;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .prow {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .ptitle {
    font-weight: 500;
    color: var(--foam);
  }
</style>
