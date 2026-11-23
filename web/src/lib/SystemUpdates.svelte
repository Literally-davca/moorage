<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type NodeUpdateStatus, type SystemUpdatesResponse } from './api';
  import { startHubRestart } from './state.svelte';
  import Confirm from './Confirm.svelte';
  import Icon from './Icon.svelte';

  let {
    primaryBusy = false,
    onUpdateAvail,
  }: {
    primaryBusy?: boolean;
    onUpdateAvail?: (avail: boolean) => void;
  } = $props();

  let data = $state<SystemUpdatesResponse | null>(null);
  let loading = $state(true);
  let checking = $state(false);
  let applying = $state(false);
  let error = $state('');
  let copiedId = $state('');
  let confirmAction = $state<{
    kind: 'apply' | 'rollback';
    title: string;
    body: string;
    label: string;
    danger?: boolean;
    targets?: string[];
    nodeId?: string;
    hubFrom?: string;
    hubTo?: string;
    restartsHub: boolean;
  } | null>(null);
  let confirmOpen = $state(false);

  async function load() {
    try {
      data = await api.systemUpdates();
      error = '';
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  const anyActive = $derived(
    (data?.nodes ?? []).some((n) => n.job && (n.job.status === 'running' || n.job.status === 'restarting')),
  );

  const hubNode = $derived((data?.nodes ?? []).find((n) => n.id === 'hub') ?? null);
  const updatableNodes = $derived((data?.nodes ?? []).filter((n) => n.canUpdate));
  const needsHubFirst = $derived(
    (data?.nodes ?? []).some((n) => n.id !== 'hub' && n.compatibility === 'update-hub-first'),
  );
  const anyComposeContainer = $derived((data?.nodes ?? []).some((n) => n.mode === 'container' && Boolean(n.composeProject)));

  $effect(() => {
    onUpdateAvail?.(updatableNodes.length > 0 && !primaryBusy);
  });

  $effect(() => {
    untrack(() => {
      void load();
    });
  });

  $effect(() => {
    const active = anyActive;
    const ms = active ? 2000 : 15000;
    const t = setInterval(() => {
      if (!document.hidden) void load();
    }, ms);
    return () => clearInterval(t);
  });

  async function checkNow() {
    checking = true;
    error = '';
    try {
      data = await api.checkSystemUpdates();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      checking = false;
    }
  }

  function promptUpdateAll() {
    if (!data?.release) return;
    if (needsHubFirst && hubNode?.canUpdate) {
      promptApplyNodes([hubNode], `Update hub to ${data.release.version} first`);
      return;
    }
    if (updatableNodes.length === 0) return;
    promptApplyNodes(updatableNodes, `Update ${updatableNodes.length} node(s) to ${data.release.version}`);
  }

  function promptApplyNodes(nodes: NodeUpdateStatus[], title: string) {
    if (!data?.release || nodes.length === 0) return;
    const restartsHub = nodes.some((n) => n.id === 'hub');
    const hub = nodes.find((n) => n.id === 'hub');
    const names = nodes.map((n) => n.name).join(', ');
    const ver = data.release.version;
    let body = `Install signed release ${ver} on ${names}.`;
    if (restartsHub) {
      body += ' Moorage will restart briefly while applying the hub update.';
    }
    confirmAction = {
      kind: 'apply',
      title,
      body,
      label: restartsHub && nodes.length === 1 ? 'Update hub' : nodes.length > 1 ? 'Update all' : 'Update',
      targets: nodes.length === updatableNodes.length && nodes.length > 1 ? ['all'] : nodes.map((n) => n.id),
      restartsHub,
      hubFrom: hub?.version,
      hubTo: ver,
    };
    confirmOpen = true;
  }

  function promptRollback(n: NodeUpdateStatus) {
    if (!n.prevVersion) return;
    const restartsHub = n.id === 'hub';
    confirmAction = {
      kind: 'rollback',
      title: `Roll back ${n.name}?`,
      body: `Restore ${n.name} from ${n.version} back to the kept ${n.prevVersion}.` +
        (restartsHub ? ' Moorage will restart briefly while restoring the hub.' : ''),
      label: 'Roll back',
      danger: true,
      nodeId: n.id,
      restartsHub,
      hubFrom: n.version,
      hubTo: n.prevVersion,
    };
    confirmOpen = true;
  }

  async function runConfirmed() {
    const act = confirmAction;
    if (!act) return;
    applying = true;
    error = '';
    try {
      if (act.kind === 'apply' && act.targets) {
        const res = await api.applySystemUpdates(act.targets);
        const hubJob = res.jobs.find((j) => j.nodeId === 'hub');
        if (res.restartingHub && hubJob && act.hubFrom && act.hubTo) {
          startHubRestart(act.hubFrom, act.hubTo, `Updating hub to ${act.hubTo}…`, hubJob.id);
        }
      } else if (act.kind === 'rollback' && act.nodeId) {
        const res = await api.rollbackSystemUpdate(act.nodeId);
        if (res.restartingHub && act.hubFrom && act.hubTo) {
          startHubRestart(act.hubFrom, act.hubTo, `Rolling back hub to ${act.hubTo}…`, res.job.id);
        }
      }
      await load();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      applying = false;
    }
  }

  async function copyCmd(id: string, cmd: string) {
    try {
      await navigator.clipboard.writeText(cmd);
      copiedId = id;
      setTimeout(() => (copiedId = ''), 2000);
    } catch {
      copiedId = 'manual';
    }
  }

  function nodeState(n: NodeUpdateStatus): { tone: 'ok' | 'warn' | 'bad' | 'neutral'; label: string } {
    if (n.job && (n.job.status === 'running' || n.job.status === 'restarting')) {
      return { tone: 'warn', label: n.job.status === 'restarting' ? 'Restarting' : 'Updating' };
    }
    if (!n.connected) return { tone: 'bad', label: 'Offline' };
    if (n.olderAgent) return { tone: 'warn', label: 'Update manually' };
    if (!n.selfUpdate) return { tone: 'neutral', label: 'Disabled' };
    if (!n.supported) return { tone: 'neutral', label: 'Unsupported' };
    if (n.compatibility === 'update-hub-first') return { tone: 'warn', label: 'Update hub first' };
    if (n.compatibility === 'agent-too-old') return { tone: 'bad', label: 'Agent too old' };
    if (n.compatibility === 'dev-build') return { tone: 'neutral', label: 'Custom build' };
    if (n.updateAvailable) return { tone: 'warn', label: `Update available (${n.latest})` };
    if (n.job?.status === 'rolled-back' || n.lastRollback) return { tone: 'bad', label: 'Rolled back' };
    if (n.job?.status === 'failed') return { tone: 'bad', label: 'Last update failed' };
    if (data?.release) return { tone: 'ok', label: 'Up to date' };
    return { tone: 'neutral', label: 'Ready' };
  }

  function fmtTime(iso?: string): string {
    if (!iso || iso.startsWith('0001-')) return 'Never';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return 'Never';
    return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  }
</script>

<section class="card card-pad sysupd" aria-label="Moorage updates">
  <div class="su-head">
    <div>
      <h2 class="section-title">Moorage updates</h2>
      <p class="meta">
        Signed self-updates for this hub and connected agents. Each node verifies the Ed25519 signature and checksum or
        image digest locally before installing.
      </p>
    </div>
    <div class="su-head-actions">
      <button type="button" class="btn" onclick={checkNow} disabled={checking} aria-busy={checking}>
        <Icon name="refresh" /> <span>{checking ? 'Checking…' : 'Check now'}</span>
      </button>
      {#if data?.release && (updatableNodes.length > 0 || (needsHubFirst && hubNode?.canUpdate))}
        <button
          type="button"
          class="btn {!primaryBusy ? 'btn-primary' : ''}"
          onclick={promptUpdateAll}
          disabled={applying || anyActive}
          aria-busy={applying}
        >
          <Icon name="cloudDown" />
          <span>{needsHubFirst && hubNode?.canUpdate ? 'Update hub first' : 'Update all'}</span>
        </button>
      {/if}
    </div>
  </div>

  {#if data}
    <div class="su-meta-bar meta">
      <span>Channel: <strong class="mono">{data.channel}</strong></span>
      <span>Last checked: {fmtTime(data.lastChecked)}</span>
      {#if !data.keyReady}
        <span class="su-warn-txt">Release signing key is not embedded in this build.</span>
      {/if}
    </div>
  {/if}

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
  {#if data?.checkError}
    <div class="su-notice warn" role="status">
      <Icon name="alert" />
      <span>Release check note: {data.checkError}{#if data.release} (showing last verified release {data.release.version}){/if}</span>
    </div>
  {/if}

  {#if data?.release}
    <div class="su-release">
      <div class="su-rel-top">
        <div class="su-rel-title">
          <span class="badge warn">Latest: {data.release.version}</span>
          {#if data.release.prerelease}<span class="badge neutral">Pre-release</span>{/if}
          {#if data.release.minHub}<span class="meta mono">min hub {data.release.minHub}</span>{/if}
          {#if data.release.minAgent}<span class="meta mono">min agent {data.release.minAgent}</span>{/if}
        </div>
        {#if data.release.htmlUrl}
          <a class="su-link" href={data.release.htmlUrl} target="_blank" rel="noopener noreferrer">
            <span>Release notes</span>
            <Icon name="external" />
          </a>
        {/if}
      </div>
      {#if data.release.notes}
        <p class="su-notes">{data.release.notes}</p>
      {/if}
    </div>
  {:else if !loading}
    <p class="meta">No verified release cached yet. Click Check now once a signed release is published.</p>
  {/if}

  {#if loading}
    <div class="skeleton" style="height:36px;width:100%"></div>
  {:else if data}
    <div class="su-list">
      {#each data.nodes as n (n.id)}
        {@const st = nodeState(n)}
        <div class="su-row">
          <div class="su-main">
            <div class="su-title">
              <span class="su-name">{n.name}</span>
              <span class="badge neutral mono">{n.version || 'unknown'}</span>
              <span class="badge neutral">{n.mode}</span>
              {#if n.composeProject}<span class="badge neutral">compose: {n.composeProject}</span>{/if}
              <span class="badge {st.tone}">{st.label}</span>
            </div>

            {#if n.compatNote && n.compatibility !== 'ok'}
              <p class="su-detail warn-txt">{n.compatNote}</p>
            {:else if !n.supported && n.reason && !n.olderAgent}
              <p class="su-detail meta">{n.reason}</p>
            {/if}

            {#if n.lastRollback && (!n.job || n.job.status !== 'done')}
              <p class="su-detail bad-txt">{n.lastRollback}</p>
            {/if}

            {#if n.job && (n.job.status === 'running' || n.job.status === 'restarting' || n.job.message)}
              <p class="su-detail {n.job.status === 'failed' || n.job.status === 'rolled-back' ? 'bad-txt' : 'meta'}">
                {#if n.job.status === 'running' || n.job.status === 'restarting'}
                  {n.job.action === 'rollback' ? 'Rolling back' : 'Updating'} ({n.job.fromVersion} → {n.job.toVersion}):
                {/if}
                {n.job.message}
              </p>
            {/if}

            {#if n.olderAgent && n.manualCommand}
              <div class="su-manual">
                <span class="meta">Update manually:</span>
                <code class="mono su-cmd">{n.manualCommand}</code>
                <button type="button" class="btn btn-ghost" onclick={() => copyCmd(n.id, n.manualCommand!)}>
                  <Icon name="copy" /> <span>{copiedId === n.id ? 'Copied' : 'Copy'}</span>
                </button>
              </div>
            {/if}
          </div>

          <div class="su-actions">
            {#if n.prevVersion}
              <button
                type="button"
                class="btn btn-ghost"
                disabled={applying || !n.connected || !n.selfUpdate || !n.supported || n.job?.status === 'running' || n.job?.status === 'restarting'}
                onclick={() => promptRollback(n)}
              >
                <Icon name="rollback" /> <span>Roll back to {n.prevVersion}</span>
              </button>
            {/if}
            {#if n.canUpdate}
              <button
                type="button"
                class="btn"
                disabled={applying || n.job?.status === 'running' || n.job?.status === 'restarting'}
                onclick={() => promptApplyNodes([n], `Update ${n.name} to ${n.latest}`)}
              >
                Update
              </button>
            {/if}
          </div>
        </div>
      {/each}
    </div>
  {/if}

  {#if anyComposeContainer}
    <p class="meta su-compose-hint">
      Containerized Moorage keeps its <code class="mono">com.docker.compose.*</code> labels across self-updates. The next
      <code class="mono">docker compose up</code> recreates the container from the image tag in your compose file—pin
      <code class="mono">:latest</code> or update the tag there too.
    </p>
  {/if}
</section>

{#if confirmAction}
  <Confirm
    bind:open={confirmOpen}
    title={confirmAction.title}
    body={confirmAction.body}
    confirmLabel={confirmAction.label}
    danger={Boolean(confirmAction.danger)}
    onConfirm={runConfirmed}
  />
{/if}

<style>
  .sysupd {
    display: grid;
    gap: 12px;
  }
  .su-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
    flex-wrap: wrap;
  }
  .su-head .meta {
    max-width: 68ch;
    margin-top: 4px;
  }
  .su-head-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .su-meta-bar {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 14px;
    font-size: 12px;
  }
  .su-warn-txt {
    color: var(--signal-text);
  }
  .su-notice {
    display: flex;
    align-items: flex-start;
    gap: 8px;
    padding: 8px 12px;
    border-radius: var(--r-md);
    font-size: 13px;
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .su-release {
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    display: grid;
    gap: 6px;
  }
  .su-rel-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    flex-wrap: wrap;
  }
  .su-rel-title {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .su-link {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 13px;
  }
  .su-notes {
    font-size: 13px;
    color: var(--mist);
    white-space: pre-line;
    overflow-wrap: anywhere;
  }
  .su-list {
    display: grid;
    border-top: 1px solid var(--rope);
  }
  .su-row {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
    padding: 12px 0;
  }
  .su-row + .su-row {
    border-top: 1px solid var(--rope);
  }
  .su-main {
    flex: 1;
    min-width: 0;
    display: grid;
    gap: 5px;
  }
  .su-title {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .su-name {
    font-weight: 500;
    color: var(--foam);
  }
  .su-detail {
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  .warn-txt {
    color: var(--signal-text);
  }
  .bad-txt {
    color: var(--adrift-text);
  }
  .su-manual {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    margin-top: 2px;
  }
  .su-cmd {
    font-size: 11px;
    padding: 4px 8px;
    border-radius: var(--r-sm);
    background: var(--well);
    color: var(--mist);
    overflow-wrap: anywhere;
    white-space: pre-wrap;
  }
  .su-actions {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
    flex-shrink: 0;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
  }
  .badge.ok {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .badge.warn {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .badge.bad {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .badge.neutral {
    background: var(--rope);
    color: var(--mist);
  }
  .su-compose-hint {
    border-top: 1px solid var(--rope);
    padding-top: 10px;
    font-size: 12px;
  }

  @media (max-width: 767px) {
    .su-row {
      flex-direction: column;
      align-items: stretch;
    }
    .su-actions {
      justify-content: flex-start;
    }
  }
</style>
