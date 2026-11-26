<script lang="ts">
  // The other addresses of a service link (M17B): a small "+N" button next to the primary
  // link opens a list of the same ports on the host's other addresses (Wi-Fi, VPN, …).
  import Icon from './Icon.svelte';
  import type { AltUrl } from './hostnet';
  import { kindLabel } from './hostnet';

  let { urls = [], compact = false }: { urls?: AltUrl[]; compact?: boolean } = $props();

  let open = $state(false);
  let root = $state<HTMLElement | null>(null);

  const groups = $derived.by(() => {
    const m = new Map<string, AltUrl[]>();
    for (const u of urls) {
      const k = `${u.label}\u0000${u.address}`;
      m.set(k, [...(m.get(k) ?? []), u]);
    }
    return [...m.values()];
  });

  function onDoc(e: MouseEvent) {
    if (open && root && !root.contains(e.target as Node)) open = false;
  }
  function onKey(e: KeyboardEvent) {
    if (open && e.key === 'Escape') open = false;
  }
</script>

<svelte:document onclick={onDoc} onkeydown={onKey} />

{#if urls.length}
  <span class="alt" bind:this={root}>
    <button
      type="button"
      class="more"
      class:compact
      aria-haspopup="true"
      aria-expanded={open}
      title="Open on another address of this host"
      onclick={() => (open = !open)}
    >
      +{groups.length} {groups.length === 1 ? 'address' : 'addresses'}
    </button>
    {#if open}
      <div class="pop" role="menu">
        {#each groups as g (g[0].label + g[0].address)}
          <div class="grp">
            <div class="glabel">
              <span>{g[0].label}</span>
              <span class="kind">{kindLabel(g[0].kind)}</span>
            </div>
            {#each g as u (u.url)}
              <a class="item mono" role="menuitem" href={u.url} target="_blank" rel="noopener noreferrer" onclick={() => (open = false)}>
                <Icon name="external" />
                <span>{u.url.replace(/^https?:\/\//, '')}</span>
              </a>
            {/each}
          </div>
        {/each}
      </div>
    {/if}
  </span>
{/if}

<style>
  .alt {
    position: relative;
    display: inline-flex;
  }
  .more {
    height: 24px;
    padding: 0 8px;
    border: 0;
    border-radius: var(--r-sm, 6px);
    background: transparent;
    box-shadow: inset 0 0 0 1px var(--rope-strong);
    color: var(--haze);
    font: 500 12px var(--font);
    cursor: pointer;
    white-space: nowrap;
  }
  .more.compact {
    height: 20px;
    font-size: 11px;
    padding: 0 6px;
  }
  .more:hover,
  .more[aria-expanded='true'] {
    color: var(--foam);
    background: var(--rope);
  }
  .more:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: 2px;
  }
  .pop {
    position: absolute;
    z-index: 30;
    top: calc(100% + 6px);
    left: 0;
    min-width: 240px;
    max-width: min(420px, calc(100vw - 32px));
    padding: 6px;
    border-radius: var(--r-card, 12px);
    background: var(--pier-raised);
    box-shadow: 0 0 0 1px var(--rope-stronger), var(--lift);
    transform-origin: top left;
    animation: pop 160ms cubic-bezier(0.23, 1, 0.32, 1);
  }
  @keyframes pop {
    from {
      opacity: 0;
      transform: scale(0.95);
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .pop {
      animation: none;
    }
  }
  .grp + .grp {
    margin-top: 4px;
    padding-top: 4px;
    box-shadow: 0 -1px 0 var(--rope-strong);
  }
  .glabel {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    padding: 4px 8px;
    font-size: 12px;
    font-weight: 600;
    color: var(--mist);
  }
  .kind {
    font-weight: 400;
    color: var(--haze);
  }
  .item {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 8px;
    border-radius: var(--r-md, 8px);
    color: var(--foam);
    font-size: 12px;
    text-decoration: none;
    overflow-wrap: anywhere;
  }
  .item:hover,
  .item:focus-visible {
    background: var(--rope);
    outline: none;
  }
</style>
