<script lang="ts" module>
  export type MultiItem = {
    value: string;
    label: string;
    /** Small grey text after the label (e.g. a host name). */
    meta?: string;
    /** Why the item can't be picked; a picked item stays removable. */
    disabled?: string;
  };
</script>

<script lang="ts">
  import { dismiss } from './dismiss';
  import Icon from './Icon.svelte';

  let {
    items,
    selected = $bindable([]),
    placeholder = 'None',
    label,
    disabled = false,
    onchange,
  }: {
    items: MultiItem[];
    selected?: string[];
    placeholder?: string;
    /** Accessible name of the trigger button. */
    label: string;
    disabled?: boolean;
    onchange?: (next: string[]) => void;
  } = $props();

  let open = $state(false);
  let query = $state('');
  let searchEl = $state<HTMLInputElement | null>(null);
  const id = `ms-${Math.random().toString(36).slice(2, 8)}`;

  const byValue = $derived(new Map(items.map((i) => [i.value, i])));
  const picked = $derived(selected.filter((v) => byValue.has(v)));
  const summary = $derived.by(() => {
    if (!picked.length) return placeholder;
    const names = picked.map((v) => byValue.get(v)!.label);
    if (names.length <= 2) return names.join(', ');
    return `${names.slice(0, 2).join(', ')} +${names.length - 2}`;
  });
  const searchable = $derived(items.length >= 8);
  const visible = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (!q) return items;
    return items.filter((i) => (i.label + ' ' + (i.meta ?? '')).toLowerCase().includes(q));
  });
  const pickable = $derived(visible.filter((i) => !i.disabled));

  function set(next: string[]) {
    selected = next;
    onchange?.(next);
  }
  function toggle(v: string, on: boolean) {
    set(on ? [...selected.filter((x) => x !== v), v] : selected.filter((x) => x !== v));
  }
  function selectAll() {
    set([...new Set([...selected, ...pickable.map((i) => i.value)])]);
  }
  function clear() {
    const shown = new Set(visible.map((i) => i.value));
    set(selected.filter((v) => !shown.has(v)));
  }
  function toggleOpen() {
    open = !open;
    if (open) {
      query = '';
      queueMicrotask(() => searchEl?.focus());
    }
  }
</script>

<div class="ms" use:dismiss={() => (open = false)}>
  <button type="button" class="field trigger" class:empty={!picked.length} onclick={toggleOpen} {disabled}
    aria-haspopup="true" aria-expanded={open} aria-controls={id} aria-label="{label}: {summary}">
    <span class="sum">{summary}</span>
    {#if picked.length > 2}<span class="count">{picked.length}</span>{/if}
    <Icon name="chevronDown" />
  </button>
  {#if open}
    <div class="panel card" {id}>
      {#if searchable}
        <input class="field search" bind:this={searchEl} bind:value={query} placeholder="Search" aria-label="Search {label}" spellcheck="false" />
      {/if}
      <div class="tools">
        <button type="button" class="link" onclick={selectAll} disabled={!pickable.length}>Select all</button>
        <button type="button" class="link" onclick={clear} disabled={!picked.length}>Clear</button>
        <span class="meta">{picked.length} selected</span>
      </div>
      <div class="opts" role="group" aria-label={label}>
        {#each visible as it (it.value)}
          {@const on = selected.includes(it.value)}
          <label class="opt" class:off={!!it.disabled && !on}>
            <input type="checkbox" checked={on} disabled={!!it.disabled && !on} onchange={(e) => toggle(it.value, e.currentTarget.checked)} />
            <span class="otext">
              <span class="olabel">{it.label}</span>
              {#if it.meta}<span class="ometa">{it.meta}</span>{/if}
              {#if it.disabled}<span class="why">{it.disabled}</span>{/if}
            </span>
          </label>
        {:else}
          <p class="meta none">{items.length ? 'Nothing matches.' : 'Nothing to choose from.'}</p>
        {/each}
      </div>
    </div>
  {/if}
</div>

<style>
  .ms {
    position: relative;
    min-width: 0;
  }
  .trigger {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    text-align: left;
    cursor: pointer;
  }
  .trigger.empty .sum {
    color: var(--haze);
  }
  .sum {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .count {
    min-width: 20px;
    height: 20px;
    padding: 0 6px;
    border-radius: 999px;
    background: var(--buoy-wash);
    color: var(--foam);
    font-size: 11px;
    font-weight: 600;
    display: inline-flex;
    align-items: center;
    justify-content: center;
  }
  .panel {
    position: absolute;
    z-index: 30;
    left: 0;
    top: calc(100% + 4px);
    /* Wider than the trigger when names are long, never wider than the screen. */
    min-width: max(100%, 280px);
    width: max-content;
    max-width: min(420px, calc(100vw - 32px));
    padding: 6px;
    background: var(--pier-raised);
    box-shadow: var(--lift-raised);
    display: grid;
    gap: 4px;
  }
  .search {
    height: 32px;
  }
  .tools {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 2px 6px;
  }
  .tools .meta {
    margin-left: auto;
    font-size: 12px;
  }
  .link {
    border: 0;
    background: none;
    padding: 0;
    color: var(--buoy-text, var(--buoy));
    font: 500 12px var(--font);
    cursor: pointer;
  }
  .link:disabled {
    color: var(--haze);
    cursor: default;
  }
  .opts {
    max-height: 260px;
    overflow-y: auto;
    /* A flex column, not a grid: Chromium collapsed grid rows when labels wrapped. */
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
  .opt {
    flex-shrink: 0;
    display: flex;
    align-items: flex-start;
    gap: 10px;
    min-height: 34px;
    padding: 4px 8px;
    border-radius: var(--r-sm);
    color: var(--mist);
    font-size: 13px;
    cursor: pointer;
  }
  .opt:hover {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .opt.off {
    cursor: default;
    color: var(--haze);
  }
  .opt.off:hover {
    background: transparent;
  }
  .opt input {
    flex-shrink: 0;
    margin: 3px 0 0;
  }
  .otext {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: 2px 0;
  }
  .olabel {
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .ometa {
    font-size: 12px;
    color: var(--haze);
  }
  .why {
    font-size: 12px;
    color: var(--haze);
  }
  .none {
    padding: 6px 8px;
  }
</style>
