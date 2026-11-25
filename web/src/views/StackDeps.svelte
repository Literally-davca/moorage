<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type AutoRestartMode, type HostDeps, type StackDep } from '../lib/api';
  import { app, isAdmin, link, stackLabel } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Select, { type SelectItem } from '../lib/Select.svelte';
  import { autoRestartLabels, detectionText, edgeText, nodeClass, nodeText, refKey } from '../lib/deps';

  let { name }: { name: string } = $props();

  let data = $state<HostDeps | null>(null);
  let error = $state('');
  let busy = $state(false);
  let adding = $state(false);
  let addHost = $state(app.host);
  let addStack = $state('');
  let options = $state<SelectItem[]>([]);

  async function load() {
    try {
      data = await api.deps();
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => void load());
    const t = setInterval(() => !document.hidden && void load(), 15000);
    return () => clearInterval(t);
  });

  // stack names offered when adding: those of the chosen host (a primitive key)
  const chosenHost = $derived(addHost);
  $effect(() => {
    if (!adding) return;
    const h = chosenHost;
    untrack(() => {
      options = [];
      api
        .stacksOf(h)
        .then((r) => {
          if (h === addHost) {
            options = r.stacks
              .filter((s) => !(h === app.host && s.name === name))
              .map((s) => {
                const disp = stackLabel(h, s.name, s.displayName);
                return { value: s.name, label: disp, meta: disp !== s.name ? s.name : undefined };
              });
          }
        })
        .catch(() => {});
    });
  });

  const here = $derived(`${app.host}/${name}`);
  const nodes = $derived(new Map((data?.nodes ?? []).map((n) => [refKey(n), n])));
  const dependsOn = $derived((data?.edges ?? []).filter((e) => refKey(e.from) === here));
  const usedBy = $derived((data?.edges ?? []).filter((e) => refKey(e.to) === here));
  const inCycle = $derived(nodes.get(here)?.inCycle ?? false);

  async function act(fn: () => Promise<unknown>) {
    busy = true;
    error = '';
    try {
      await fn();
      await load();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  function add(ev: SubmitEvent) {
    ev.preventDefault();
    const stack = addStack.trim();
    if (!stack) return;
    void act(async () => {
      await api.addDep(name, { hostId: addHost, stack });
      addStack = '';
      adding = false;
    });
  }

  function otherHref(e: StackDep, other: 'from' | 'to'): string {
    const r = e[other];
    return `#/h/${encodeURIComponent(r.hostId)}/stacks/${encodeURIComponent(r.stack)}`;
  }

  function otherLabel(e: StackDep, other: 'from' | 'to'): string {
    const r = e[other];
    const n = nodes.get(refKey(r));
    const disp = stackLabel(r.hostId, r.stack, n?.displayName);
    return r.hostId === app.host ? disp : `${disp} (${n?.hostName || r.hostId})`;
  }
</script>

{#snippet edgeRow(e: StackDep, other: 'from' | 'to')}
  {@const n = nodes.get(refKey(e[other]))}
  <div class="row" class:dim={e.ignored}>
    <span class="dot {nodeClass(n)}"></span>
    <span class="who">
      <a href={otherHref(e, other)}>{otherLabel(e, other)}</a>
      <span class="meta sub">{nodeText(n)} · {edgeText(e)}{e.detail ? ` · ${e.detail}` : ''}</span>
    </span>
    {#if isAdmin()}
      {#if !e.ignored}
        <div class="auto-sel">
          <Select
            label="Auto-restart policy"
            compact={true}
            value={e.autoRestart ?? 'off'}
            disabled={busy}
            items={Object.entries(autoRestartLabels).map(([val, txt]) => ({ value: val, label: txt }))}
            onchange={(v) => act(() => api.setDepAutoRestart(name, e.id, v as AutoRestartMode))}
          />
        </div>
      {/if}
      {#if e.source === 'manual'}
        <button class="btn btn-sm btn-ghost" disabled={busy} onclick={() => act(() => api.removeDep(name, e.id))}>Delete</button>
      {:else if e.ignored}
        <button class="btn btn-sm btn-ghost" disabled={busy} onclick={() => act(() => api.ignoreDep(name, e.id, false))}>Unignore</button>
      {:else}
        <button class="btn btn-sm btn-ghost" disabled={busy} onclick={() => act(() => api.ignoreDep(name, e.id, true))} title="This dependency is wrong: don't use it for alert grouping">Ignore</button>
      {/if}
    {/if}
  </div>
{/snippet}

<section class="card card-pad deps" aria-label="Dependencies">
  <div class="dhead">
    <span class="section-title">Dependencies</span>
    <a class="meta" href={link('deps')}>Graph of this host</a>
  </div>

  {#if inCycle}
    <p class="warn meta" role="alert"><Icon name="alert" /> This stack is part of a dependency loop; its alerts are not grouped.</p>
  {/if}

  {#if data}
    <div class="cols">
      <div>
        <p class="meta lbl">Depends on</p>
        {#each dependsOn as e (e.id)}{@render edgeRow(e, 'to')}{:else}<p class="meta none">Nothing</p>{/each}
      </div>
      <div>
        <p class="meta lbl">Used by</p>
        {#each usedBy as e (e.id)}{@render edgeRow(e, 'from')}{:else}<p class="meta none">Nothing</p>{/each}
      </div>
    </div>
    {#if data.detection.unsupported || data.detection.error}
      <p class="meta warn"><Icon name="alert" /> {detectionText(data.detection)}</p>
    {/if}
  {:else if !error}
    <div class="skeleton" style="height:12px;width:40%"></div>
  {/if}

  {#if isAdmin()}
    {#if adding}
      <form class="add" onsubmit={add}>
        <div class="add-host">
          <span class="meta">Host</span>
          <Select
            label="Dependency host"
            value={addHost}
            items={app.hosts.map((h) => ({ value: h.id, label: h.name }))}
            onchange={(v) => (addHost = v)}
          />
        </div>
        <div class="grow">
          <span class="meta">{stackLabel(app.host, name)} depends on stack</span>
          <Select
            label="Upstream stack"
            items={options}
            bind:value={addStack}
            allowCustom={true}
            placeholder="Pick or type a stack name…"
            customPlaceholder="Filter stacks or type a custom name…"
          />
        </div>
        <button class="btn" type="submit" disabled={busy || !addStack.trim()}>Add</button>
        <button class="btn btn-ghost" type="button" onclick={() => (adding = false)}>Cancel</button>
      </form>
    {:else}
      <button class="btn btn-sm add-btn" onclick={() => { addHost = app.host; adding = true; }}><Icon name="plus" /> Add dependency</button>
    {/if}
  {/if}
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
</section>

<style>
  .deps {
    display: grid;
    gap: 12px;
  }
  .dhead {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 12px;
    flex-wrap: wrap;
  }
  .cols {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 8px 24px;
  }
  .lbl {
    font-size: 12px;
    margin-bottom: 4px;
  }
  .none {
    font-size: 13px;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 6px 0;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .row.dim {
    opacity: 0.7;
  }
  .who {
    flex: 1;
    min-width: 0;
    display: grid;
    gap: 2px;
    overflow-wrap: anywhere;
  }
  .sub {
    font-size: 12px;
  }
  .warn {
    display: flex;
    gap: 6px;
    align-items: baseline;
    color: var(--signal-text);
  }
  .add {
    display: flex;
    gap: 8px;
    align-items: end;
    flex-wrap: wrap;
  }
  .add .add-host,
  .add .grow {
    display: grid;
    gap: 4px;
  }
  .add .add-host {
    min-width: 150px;
  }
  .add .grow {
    flex: 1;
    min-width: 160px;
  }
  .add-btn {
    justify-self: start;
  }
  .auto-sel {
    width: auto;
    height: 28px;
    padding: 0 8px;
    font-size: 12px;
  }
  @media (max-width: 640px) {
    .cols {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
