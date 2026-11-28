<script lang="ts">
  import type { AssistantModelInfo } from './api';

  // A provider's models, collapsed to "312 models" (M20H). Opened: search, groups by
  // vendor (the part before "/") with counts, and ticks when ticking matters. The list
  // exists in the DOM only while open (M20E: hundreds of rows cost memory).
  let {
    models,
    fallbackGroup,
    ticked,
    tickable,
    ontoggle,
  }: {
    models: AssistantModelInfo[];
    fallbackGroup: string;
    ticked: string[];
    tickable: boolean;
    ontoggle: (id: string) => void;
  } = $props();

  let open = $state(false);
  let q = $state('');

  const groups = $derived.by(() => {
    if (!open) return [];
    const needle = q.trim().toLowerCase();
    const by = new Map<string, AssistantModelInfo[]>();
    for (const m of models) {
      if (needle && !m.id.toLowerCase().includes(needle)) continue;
      const g = m.id.includes('/') ? m.id.split('/')[0] : fallbackGroup;
      let l = by.get(g);
      if (!l) by.set(g, (l = []));
      l.push(m);
    }
    return [...by.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  });
</script>

<details class="mlist" bind:open>
  <summary>
    {models.length} {models.length === 1 ? 'model' : 'models'}{tickable ? ` · ${ticked.length} ticked` : ''}
  </summary>
  {#if open}
    <input class="field" type="search" bind:value={q} placeholder="Search models" aria-label="Search models" />
    {#each groups as [g, list] (g)}
      <div class="grp">
        <span class="meta">{g} ({list.length})</span>
        {#each list as m (m.id)}
          <label class="row">
            {#if tickable}<input type="checkbox" checked={ticked.includes(m.id)} onchange={() => ontoggle(m.id)} />{/if}
            <span class="mono">{m.id}</span>
            {#if m.tools === false}<span class="tag">advise only</span>{/if}
          </label>
        {/each}
      </div>
    {:else}
      <span class="meta">No model matches.</span>
    {/each}
  {/if}
</details>

<style>
  .mlist summary {
    cursor: pointer;
  }
  .mlist[open] {
    display: grid;
    gap: 6px;
    max-height: 320px;
    overflow: auto;
  }
  .grp {
    display: grid;
    gap: 2px;
  }
  .row {
    display: flex;
    gap: 6px;
    align-items: center;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .tag {
    font-size: 11px;
    padding: 0 6px;
    border-radius: var(--r-sm);
    background: var(--signal-wash);
    color: var(--signal-text);
  }
</style>
