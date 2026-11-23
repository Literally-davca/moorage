<script lang="ts">
  import { engineLabel, statusLabel, type HostInfo, type StackSummary } from '../lib/api';
  import { canEdit, link } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';

  let { stacks, host }: { stacks: StackSummary[] | null; host: HostInfo | null } = $props();

  const attention = $derived((stacks ?? []).filter((s) => s.status === 'down' || s.status === 'degraded'));
</script>

{#if stacks !== null && stacks.length === 0}
  <section class="card empty">
    <span class="mark"><Icon name="anchor" /></span>
    <h1>Add your first stack</h1>
    <p>
      A stack is a folder with a <code>compose.yaml</code> in
      <code>{host?.stacksDir ?? '/opt/stacks'}</code>. Create one here, or copy existing folders there and they show up on
      their own.
    </p>
    {#if canEdit()}<a class="btn btn-primary" href={link('new')}><Icon name="plus" /> Create stack</a>{/if}
  </section>
{:else if stacks !== null}
  <section class="card card-pad">
    <h1 class="title">{host?.name ?? 'Host'}</h1>
    <p class="meta">{[`${stacks.length} stacks`, engineLabel(host), host?.stacksDir].filter(Boolean).join(' · ')}</p>

    {#if attention.length}
      <h2 class="section-title gap">Needs attention</h2>
      <ul class="rows">
        {#each attention as s (s.name)}
          <li>
            <a href={link('stacks/' + encodeURIComponent(s.name))}>
              <span class="dot {s.status}"></span>
              <span class="n">{s.name}</span>
              <span class="badge {s.status}">{statusLabel[s.status]}</span>
              <span class="meta num">{s.running}/{s.total} running</span>
            </a>
          </li>
        {/each}
      </ul>
    {:else}
      <p class="calm"><Icon name="check" /> Every stack is where it should be.</p>
    {/if}
  </section>
{/if}

<style>
  .empty {
    display: grid;
    justify-items: start;
    gap: 12px;
    padding: 40px;
    max-width: 640px;
  }
  .mark {
    width: 40px;
    height: 40px;
    border-radius: 12px;
    background: var(--buoy-wash);
    color: var(--buoy-text);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 22px;
  }
  .empty h1,
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .empty p {
    color: var(--mist);
    max-width: 52ch;
  }
  .empty .btn {
    margin-top: 8px;
  }
  .gap {
    margin: 24px 0 8px;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .rows a {
    display: flex;
    align-items: center;
    gap: 12px;
    height: 44px;
    padding: 0 12px;
    margin: 0 -12px;
    border-radius: var(--r-md);
    color: var(--foam);
    text-decoration: none;
  }
  .rows a:hover {
    background: var(--rope);
  }
  .n {
    font-weight: 500;
    flex: 1;
  }
  .calm {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 20px;
    color: var(--afloat-text);
  }
</style>
