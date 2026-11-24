<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type ContainerInfo } from '../lib/api';
  import { app, currentHost, isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import LogViewer from './LogViewer.svelte';
  import TerminalPanel from './TerminalPanel.svelte';

  let containers = $state<ContainerInfo[]>([]);
  let loading = $state(true);
  let error = $state('');
  let splitView = $state(false);
  let termTarget = $state<{ id: string; name: string; service: string } | null>(null);
  let selectedTermId = $state('');

  const curHost = $derived(currentHost());
  const canContainerTerm = $derived(isAdmin() && !!curHost?.terminal && !curHost?.readOnly);
  const runningContainers = $derived(containers.filter((c) => c.state === 'running'));

  async function loadContainers() {
    try {
      containers = await api.containers();
      error = '';
      if (!selectedTermId && runningContainers.length > 0) {
        selectedTermId = runningContainers[0].id;
      } else if (selectedTermId && !runningContainers.some((c) => c.id === selectedTermId)) {
        selectedTermId = runningContainers[0]?.id ?? '';
      }
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void app.host;
    loading = true;
    termTarget = null;
    untrack(() => {
      void loadContainers();
    });
    const t = setInterval(() => {
      if (!document.hidden) void loadContainers();
    }, 15000);
    return () => clearInterval(t);
  });

  function openSelectedTerminal() {
    const c = runningContainers.find((x) => x.id === selectedTermId);
    if (!c) return;
    termTarget = {
      id: c.id,
      name: c.name,
      service: c.project ? `${c.project}/${c.service || c.name}` : c.name,
    };
  }
</script>

<div class="logs-page">
  <section class="card card-pad head">
    <div class="head-row">
      <div>
        <h1 class="title">Logs</h1>
        <p class="meta">
          {curHost?.name ?? app.host} · {containers.length} {containers.length === 1 ? 'container' : 'containers'} ({runningContainers.length} running)
        </p>
      </div>

      <div class="actions">
        {#if isAdmin() && runningContainers.length > 0}
          <div class="term-launch">
            <select
              class="term-sel"
              bind:value={selectedTermId}
              aria-label="Select running container for terminal"
              disabled={!canContainerTerm}
            >
              {#each runningContainers as c (c.id)}
                <option value={c.id}>{c.project ? `${c.project} / ${c.service || c.name}` : c.name}</option>
              {/each}
            </select>
            <button
              type="button"
              class="btn"
              disabled={!canContainerTerm || !selectedTermId}
              title={
                canContainerTerm
                  ? 'Open interactive shell in selected container'
                  : curHost?.readOnly
                    ? 'Host is read-only (MOORAGE_READ_ONLY=true)'
                    : 'Set MOORAGE_TERMINAL=true on this host to enable container terminal'
              }
              onclick={openSelectedTerminal}
            >
              <Icon name={canContainerTerm ? 'terminal' : 'lock'} /> Terminal
            </button>
          </div>
        {/if}

        <button
          type="button"
          class="btn"
          class:active={splitView}
          aria-pressed={splitView}
          onclick={() => (splitView = !splitView)}
        >
          <Icon name="columns" /> {splitView ? 'Single view' : 'Split view'}
        </button>
      </div>
    </div>
  </section>

  {#if error}
    <p class="error-text" role="alert">{error}</p>
  {/if}

  {#if termTarget}
    {#key termTarget.id}
      <TerminalPanel
        mode="container"
        containerId={termTarget.id}
        containerName={termTarget.name}
        service={termTarget.service}
        onClose={() => (termTarget = null)}
      />
    {/key}
  {/if}

  {#if loading && containers.length === 0}
    <section class="card card-pad" aria-busy="true">
      <div class="skeleton" style="height:20px;width:240px"></div>
      <div class="skeleton" style="height:360px;width:100%;margin-top:12px"></div>
    </section>
  {:else}
    <div class="panes" class:split={splitView}>
      <section class="card card-pad pane">
        <LogViewer stack="" hostContainers={containers} paneLabel={splitView ? 'Left' : ''} />
      </section>
      {#if splitView}
        <section class="card card-pad pane">
          <LogViewer stack="" hostContainers={containers} paneLabel="Right" />
        </section>
      {/if}
    </div>
  {/if}
</div>

<style>
  .logs-page {
    display: grid;
    gap: 12px;
  }
  .head-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .term-launch {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .term-sel {
    height: 36px;
    max-width: 240px;
    padding: 0 10px;
    border-radius: var(--r-md);
    border: 0;
    background: var(--well);
    color: var(--foam);
    box-shadow: inset 0 0 0 1px var(--rope);
    font: 500 13px var(--font);
  }
  .term-sel option {
    background: var(--hull);
    color: var(--foam);
  }
  .btn.active {
    background: var(--buoy-wash);
    color: var(--buoy-text);
    box-shadow: inset 0 0 0 1px var(--buoy);
  }
  .panes {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .panes.split {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .pane {
    min-width: 0;
  }

  @media (max-width: 1024px) {
    .panes.split {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
