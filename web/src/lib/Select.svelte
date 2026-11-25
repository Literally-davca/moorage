<script lang="ts" module>
  export type SelectItem = {
    value: string;
    label: string;
    meta?: string;
    disabled?: string;
  };
</script>

<script lang="ts">
  import { dismiss } from './dismiss';
  import Icon from './Icon.svelte';

  let {
    items,
    value = $bindable(''),
    placeholder = 'Select…',
    label,
    disabled = false,
    allowCustom = false,
    customPlaceholder = 'Type a custom value…',
    compact = false,
    onchange,
  }: {
    items: SelectItem[];
    value?: string;
    placeholder?: string;
    label: string;
    disabled?: boolean;
    allowCustom?: boolean;
    customPlaceholder?: string;
    compact?: boolean;
    onchange?: (next: string) => void;
  } = $props();

  let open = $state(false);
  let query = $state('');
  let hi = $state(0);
  let searchEl = $state<HTMLInputElement | null>(null);
  const id = `sel-${Math.random().toString(36).slice(2, 8)}`;

  const selectedItem = $derived(items.find((i) => i.value === value));
  const summary = $derived(selectedItem ? selectedItem.label : value ? value : placeholder);
  const isEmpty = $derived(!selectedItem && !value);
  const searchable = $derived(allowCustom || items.length >= 7);

  const visible = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (!q) return items;
    return items.filter((i) => (i.label + ' ' + i.value + ' ' + (i.meta ?? '')).toLowerCase().includes(q));
  });

  const canUseCustom = $derived.by(() => {
    if (!allowCustom) return false;
    const q = query.trim();
    if (!q) return false;
    return !items.some((i) => i.value === q || i.label.toLowerCase() === q.toLowerCase());
  });

  function pick(v: string) {
    value = v;
    open = false;
    query = '';
    onchange?.(v);
  }

  function toggleOpen() {
    if (disabled) return;
    open = !open;
    if (open) {
      query = '';
      const idx = items.findIndex((i) => i.value === value);
      hi = idx >= 0 ? idx : 0;
      queueMicrotask(() => searchEl?.focus());
    }
  }

  function onKey(e: KeyboardEvent) {
    if (!open) {
      if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        toggleOpen();
      }
      return;
    }
    const max = visible.length - 1;
    if (e.key === 'Escape') {
      e.preventDefault();
      open = false;
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      hi = max >= 0 ? (hi + 1) % (max + 1) : 0;
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      hi = max >= 0 ? (hi - 1 + (max + 1)) % (max + 1) : 0;
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const target = visible[hi];
      if (target && !target.disabled) {
        pick(target.value);
      } else if (canUseCustom) {
        pick(query.trim());
      }
    }
  }
</script>

<div class="sel" class:compact use:dismiss={() => (open = false)}>
  <button
    type="button"
    class="field trigger"
    class:empty={isEmpty}
    onclick={toggleOpen}
    onkeydown={onKey}
    {disabled}
    aria-haspopup="listbox"
    aria-expanded={open}
    aria-controls={id}
    aria-label="{label}: {summary}"
  >
    <span class="sum">
      <span class="slabel">{summary}</span>
      {#if selectedItem?.meta}<span class="smeta">{selectedItem.meta}</span>{/if}
    </span>
    <Icon name="chevronDown" />
  </button>
  {#if open}
    <div class="panel card" {id} role="listbox" aria-label={label} onkeydown={onKey} tabindex="-1">
      {#if searchable}
        <input
          class="field search"
          bind:this={searchEl}
          bind:value={query}
          placeholder={allowCustom ? customPlaceholder : 'Search…'}
          aria-label="Search {label}"
          spellcheck="false"
        />
      {/if}
      <div class="opts">
        {#if canUseCustom}
          <button type="button" class="opt custom-opt" onclick={() => pick(query.trim())}>
            <Icon name="plus" />
            <span class="olabel">Use "{query.trim()}"</span>
          </button>
        {/if}
        {#each visible as it, idx (it.value)}
          {@const on = it.value === value}
          <button
            type="button"
            class="opt"
            class:selected={on}
            class:hi={idx === hi}
            class:off={!!it.disabled}
            disabled={!!it.disabled}
            role="option"
            aria-selected={on}
            onclick={() => pick(it.value)}
            onmouseenter={() => (hi = idx)}
          >
            <span class="check-col">
              {#if on}<Icon name="check" />{/if}
            </span>
            <span class="olabel">{it.label}</span>
            {#if it.meta}<span class="ometa">{it.meta}</span>{/if}
            {#if it.disabled}<span class="why">{it.disabled}</span>{/if}
          </button>
        {:else}
          {#if !canUseCustom}
            <p class="meta none">{items.length ? 'Nothing matches.' : 'Nothing to choose from.'}</p>
          {/if}
        {/each}
      </div>
    </div>
  {/if}
</div>

<style>
  .sel {
    position: relative;
    min-width: 0;
    width: 100%;
  }
  .sel.compact {
    width: auto;
    min-width: 120px;
  }
  .trigger {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    text-align: left;
    cursor: pointer;
  }
  .trigger:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
  .trigger.empty .slabel {
    color: var(--haze);
  }
  .sum {
    flex: 1;
    min-width: 0;
    display: flex;
    align-items: baseline;
    gap: 8px;
    overflow: hidden;
  }
  .slabel {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .smeta {
    color: var(--haze);
    font-size: 12px;
    white-space: nowrap;
    flex-shrink: 0;
  }
  .panel {
    position: absolute;
    z-index: 40;
    left: 0;
    right: 0;
    top: calc(100% + 4px);
    min-width: 220px;
    padding: 6px;
    background: var(--pier-raised);
    box-shadow: var(--lift-raised);
    display: grid;
    gap: 4px;
  }
  .search {
    height: 32px;
  }
  .opts {
    max-height: 260px;
    overflow-y: auto;
    display: grid;
    gap: 1px;
  }
  .opt {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    min-height: 34px;
    padding: 5px 8px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--mist);
    font: 400 13px var(--font);
    text-align: left;
    cursor: pointer;
  }
  .opt:hover,
  .opt.hi {
    background: var(--rope);
    color: var(--foam);
  }
  .opt.selected {
    color: var(--foam);
    font-weight: 500;
  }
  .opt.off {
    opacity: 0.48;
    cursor: not-allowed;
  }
  .check-col {
    width: 14px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--buoy-text, var(--buoy));
    flex-shrink: 0;
  }
  .olabel {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ometa {
    color: var(--haze);
    font-size: 12px;
    flex-shrink: 0;
  }
  .why {
    color: var(--haze);
    font-size: 11px;
    flex-shrink: 0;
  }
  .custom-opt {
    color: var(--buoy-text, var(--buoy));
  }
  .none {
    margin: 6px 8px;
    font-size: 12px;
  }
</style>
