<script lang="ts">
  import { api, type Op, type SnapshotManifest, type SnapshotPlan, type SnapshotSettings } from '../lib/api';
  import { isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import OpPanel from './OpPanel.svelte';

  // Snapshots tab (M18F): local restore points of the stack's data and configuration,
  // kept on the host as .tar.zst files. Long-term backups stay with restic-backup.
  let { name, editable, onChanged }: { name: string; editable: boolean; onChanged: () => void } = $props();

  let list = $state<SnapshotManifest[]>([]);
  let settings = $state<SnapshotSettings | null>(null);
  let error = $state('');
  let unsupported = $state('');
  let op = $state<Op | null>(null);
  let busy = $state(false);

  // take dialog
  let plan = $state<SnapshotPlan | null>(null);
  let mode = $state<'pause' | 'stop' | 'live'>('pause');
  let liveOk = $state(false);
  let extra = $state<string[]>([]);
  let note = $state('');

  // restore dialog
  let restoring = $state<SnapshotManifest | null>(null);
  let scope = $state<'all' | 'data' | 'config'>('all');
  let preRestore = $state(true);
  let configDiff = $state<{ changed: boolean; compose: string; current: string } | null>(null);

  async function load() {
    try {
      const r = await api.snapshots(name);
      list = r.snapshots;
      settings = r.settings;
      error = '';
      unsupported = '';
    } catch (e) {
      const msg = (e as Error).message;
      if (msg.includes('Update the agent') || msg.includes("doesn't support")) unsupported = msg;
      else error = msg;
    }
  }
  $effect(() => {
    void name;
    load();
  });

  const size = (n: number) =>
    n < 0 ? 'unknown' : n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + ' GiB' : n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + ' MiB' : Math.max(1, Math.round(n / 1024)) + ' KiB';

  async function openTake() {
    error = '';
    try {
      plan = await api.snapshotPlan(name);
      extra = [...(settings?.extraBinds ?? [])].filter((b) => plan!.sources.some((s) => s.external && s.source === b));
      mode = 'pause';
      liveOk = false;
      note = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  const estimate = $derived(
    plan ? plan.sources.filter((s) => !s.external || extra.includes(s.source)).reduce((a, s) => a + Math.max(0, s.size), 0) : 0,
  );
  const tooFull = $derived(
    !!plan && plan.freeBytes >= 0 && plan.freePct > 0 && settings !== null &&
      ((plan.freeBytes - estimate) / (plan.freeBytes / (plan.freePct / 100))) * 100 < settings.minFreePct,
  );
  // a long copy means a long pause: suggest Stop / Live above ~5 minutes at ~100 MB/s
  const longPause = $derived(estimate > 30 * (1 << 30));

  async function take() {
    busy = true;
    error = '';
    try {
      op = await api.createSnapshot(name, { mode, extraBinds: extra, note });
      if (settings) settings = await api.saveSnapshotSettings(name, { ...settings, extraBinds: extra });
      plan = null;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function openRestore(m: SnapshotManifest) {
    restoring = m;
    scope = m.service ? 'data' : 'all';
    preRestore = true;
    configDiff = null;
    try {
      const p = await api.snapshotPreview(name, m.id);
      configDiff = { changed: p.compose !== p.currentCompose || p.env !== p.currentEnv, compose: p.compose, current: p.currentCompose };
    } catch {
      configDiff = null;
    }
  }

  async function restore() {
    if (!restoring) return;
    busy = true;
    error = '';
    try {
      op = await api.restoreSnapshot(name, restoring.id, scope, preRestore);
      restoring = null;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function pin(m: SnapshotManifest) {
    try {
      await api.pinSnapshot(name, m.id, !m.pinned);
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function remove(m: SnapshotManifest) {
    if (!confirm(`Delete snapshot ${m.id}? This can't be undone.`)) return;
    try {
      await api.deleteSnapshot(name, m.id);
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function saveSettings() {
    if (!settings) return;
    try {
      settings = await api.saveSnapshotSettings(name, settings);
    } catch (e) {
      error = (e as Error).message;
    }
  }

  function diffSummary(a: string, b: string) {
    const al = new Set(a.split('\n'));
    const bl = new Set(b.split('\n'));
    const removed = b.split('\n').filter((l) => !al.has(l) && l.trim());
    const added = a.split('\n').filter((l) => !bl.has(l) && l.trim());
    return { added, removed };
  }
</script>

<section class="card card-pad snaps">
  {#if unsupported}
    <p class="meta">{unsupported}</p>
  {:else}
    {#if error}<p class="error-text" role="alert">{error}</p>{/if}
    <header class="shead">
      <div>
        <h3 class="section-title">Snapshots</h3>
        <p class="meta">Local restore points of this stack's volumes and configuration, kept on the host. For long-term backups off this server use the restic-backup add-on.</p>
      </div>
      {#if editable && isAdmin()}
        <button type="button" class="btn btn-primary" onclick={openTake} disabled={busy || !!op}><Icon name="plus" /> Take snapshot</button>
      {/if}
    </header>

    {#if op}
      <OpPanel {op} onDone={() => { load(); onChanged(); }} onClose={() => (op = null)} />
    {/if}

    {#if plan}
      <div class="dialog" role="dialog" aria-label="Take snapshot">
        <p class="section-title">What goes into the snapshot</p>
        {#if plan.sources.length === 0}<p class="meta">No volumes or bind mounts: only the configuration is saved.</p>{/if}
        <ul class="srcs">
          {#each plan.sources as s (s.kind + s.source)}
            <li>
              <label class="check">
                {#if s.external}
                  <input type="checkbox" checked={extra.includes(s.source)} onchange={() => (extra = extra.includes(s.source) ? extra.filter((x) => x !== s.source) : [...extra, s.source])} />
                {:else}
                  <input type="checkbox" checked disabled />
                {/if}
                <span class="mono">{s.kind === 'volume' ? s.source : s.source}</span>
                <span class="meta">→ {s.service}:{s.target} · {size(s.size)}</span>
                {#if s.users?.length}<span class="meta">(also used by {s.users.join(', ')})</span>{/if}
                {#if s.external}<span class="badge">outside the stack folder</span>{/if}
              </label>
            </li>
          {/each}
          <li><label class="check"><input type="checkbox" checked disabled /> compose.yaml + .env</label></li>
        </ul>
        <p class="meta">
          About {size(estimate)} before compression{#if plan.freeBytes >= 0} · {size(plan.freeBytes)} free ({plan.freePct.toFixed(0)} %){/if}
        </p>
        <fieldset class="modes">
          <legend class="meta">Consistency</legend>
          <label class="check"><input type="radio" bind:group={mode} value="pause" /> Pause during copy (recommended) – like a power cut, databases recover; requests wait</label>
          <label class="check"><input type="radio" bind:group={mode} value="stop" /> Stop, copy, start</label>
          <label class="check"><input type="radio" bind:group={mode} value="live" /> Live (no pause)</label>
        </fieldset>
        {#if longPause && mode === 'pause'}
          <p class="warn-text">This copy may take several minutes and the containers stay paused for all of it. Consider Stop or Live.</p>
        {/if}
        {#if mode === 'live'}
          <label class="check warn-text">
            <input type="checkbox" bind:checked={liveOk} />
            I understand that a database copied while it writes may not restore.
          </label>
        {/if}
        <input class="field" bind:value={note} placeholder="Note (optional)" maxlength="200" aria-label="Note" />
        {#if tooFull}<p class="error-text">Not enough free space: at least {settings?.minFreePct} % of the disk must stay free.</p>{/if}
        <div class="actions">
          <button type="button" class="btn btn-primary" onclick={take} disabled={busy || tooFull || (mode === 'live' && !liveOk)}>Take snapshot</button>
          <button type="button" class="btn btn-ghost" onclick={() => (plan = null)}>Cancel</button>
        </div>
      </div>
    {/if}

    {#if restoring}
      <div class="dialog" role="dialog" aria-label="Restore snapshot">
        <p class="section-title">Restore snapshot {restoring.id}</p>
        <p class="meta">
          Overwrites {restoring.parts.filter((p) => p.kind !== 'config').length} data parts ({size(restoring.uncompressed)}). The stack is stopped, restored and started again.
        </p>
        {#if restoring.service}
          <p class="meta">This is a snapshot of the service {restoring.service}: only its data comes back.</p>
        {/if}
        <fieldset class="modes">
          <legend class="meta">Restore</legend>
          <label class="check"><input type="radio" bind:group={scope} value="all" disabled={!!restoring.service} /> Data + configuration</label>
          <label class="check"><input type="radio" bind:group={scope} value="data" /> Data only</label>
          <label class="check"><input type="radio" bind:group={scope} value="config" disabled={!!restoring.service} /> Configuration only</label>
        </fieldset>
        {#if configDiff}
          {#if configDiff.changed}
            {@const d = diffSummary(configDiff.compose, configDiff.current)}
            <div class="diff mono">
              {#each d.removed as l, i (i)}<div class="del">- {l}</div>{/each}
              {#each d.added as l, i (i)}<div class="add">+ {l}</div>{/each}
            </div>
          {:else}
            <p class="meta">The configuration is the same as now.</p>
          {/if}
        {/if}
        <label class="check"><input type="checkbox" bind:checked={preRestore} /> Take a pre-restore snapshot first</label>
        <div class="actions">
          <button type="button" class="btn btn-danger" onclick={restore} disabled={busy}>Restore</button>
          <button type="button" class="btn btn-ghost" onclick={() => (restoring = null)}>Cancel</button>
        </div>
      </div>
    {/if}

    {#if list.length === 0}
      <p class="meta">No snapshots yet.</p>
    {:else}
      <ul class="rows">
        {#each list as m (m.id)}
          <li class="row">
            <div>
              <strong>{new Date(m.time).toLocaleString()}</strong>
              <span class="badge">{m.kind}</span>
              {#if m.pinned}<span class="badge info">pinned</span>{/if}
              {#if m.service}<span class="badge">service {m.service}</span>{/if}
              <div class="meta">
                {size(m.size)} · {m.mode}{#if m.commit} · compose {m.commit}{/if}{#if m.note} · {m.note}{/if}
              </div>
            </div>
            {#if isAdmin()}
              <div class="actions">
                {#if editable}<button type="button" class="btn btn-sm" onclick={() => openRestore(m)} disabled={!!op}>Restore</button>{/if}
                <a
                  class="btn btn-sm"
                  href={api.snapshotDownloadUrl(name, m.id)}
                  download
                  title="Contains .env and application data, unencrypted"><Icon name="download" /> Download</a>
                {#if editable}
                  <button type="button" class="btn btn-ghost btn-sm" onclick={() => pin(m)}>{m.pinned ? 'Unpin' : 'Pin'}</button>
                  <button type="button" class="btn btn-ghost btn-sm" onclick={() => remove(m)} aria-label="Delete snapshot"><Icon name="trash" /></button>
                {/if}
              </div>
            {/if}
          </li>
        {/each}
      </ul>
      <p class="meta">Downloads contain .env and application data, unencrypted – keep them safe.</p>
    {/if}

    {#if settings && editable && isAdmin()}
      <div class="settings">
        <label class="check">
          <input type="checkbox" bind:checked={settings.beforeUpdates} onchange={saveSettings} />
          Snapshot before updates (an update waits if the snapshot fails)
        </label>
        <label class="num">
          <span class="meta">Keep automatic snapshots</span>
          <input class="field" type="number" min="1" max="100" bind:value={settings.retainAuto} onchange={saveSettings} />
        </label>
        <label class="num">
          <span class="meta">Keep free disk (%)</span>
          <input class="field" type="number" min="0" max="90" bind:value={settings.minFreePct} onchange={saveSettings} />
        </label>
      </div>
    {/if}
  {/if}
</section>

<style>
  .snaps {
    display: grid;
    gap: 12px;
  }
  .shead {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
    flex-wrap: wrap;
  }
  .shead h3,
  .shead p {
    margin: 0;
  }
  .dialog {
    display: grid;
    gap: 8px;
    padding: 12px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .srcs,
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    padding: 8px 10px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .modes {
    border: 0;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .actions {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
    align-items: center;
  }
  .settings {
    display: flex;
    gap: 16px;
    flex-wrap: wrap;
    align-items: end;
  }
  .num {
    display: grid;
    gap: 4px;
    max-width: 180px;
  }
  .diff {
    font-size: 12px;
    max-height: 200px;
    overflow: auto;
    white-space: pre;
  }
  .del {
    color: var(--danger);
  }
  .add {
    color: var(--afloat-text);
  }
  .warn-text {
    color: var(--degraded-text, var(--warning));
  }
</style>
