<script lang="ts">
  import { dismiss } from '../lib/dismiss';
  import {
    api,
    formatBytes,
    statusLabel,
    type Anchor,
    type AutoRollback,
    type Check,
    type ImageCheckRow,
    type ImagePolicy,
    type ImageStatus,
    type ImageUpdate,
    type Op,
    type Stack,
    type StackImages,
    type Stat,
  } from '../lib/api';
  import { app, canEdit, currentHost, go, isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import CodeEditor from '../lib/CodeEditor.svelte';
  import EnvEditor from '../lib/EnvEditor.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import LogViewer from './LogViewer.svelte';
  import OpPanel from './OpPanel.svelte';
  import TerminalPanel from './TerminalPanel.svelte';
  import HistoryView from './HistoryView.svelte';
  import StackDeps from './StackDeps.svelte';
  import Strip from '../lib/Strip.svelte';
  import { checkStatusLabel, pct } from '../lib/checks';
  import { untilText } from '../lib/maintenance';
  import type { ActiveMaintenance } from '../lib/api';
  import { untrack } from 'svelte';

  let { name, onChanged }: { name: string; onChanged: () => void } = $props();

  let stack = $state<Stack | null>(null);
  let loadError = $state('');
  let autoRollbacks = $state<AutoRollback[]>([]);

  $effect(() => {
    const h = app.host;
    const st = name;
    api.autoRollbacks()
      .then((list) => {
        autoRollbacks = list.filter((r) => r.hostId === h && r.stack === st);
      })
      .catch(() => (autoRollbacks = []));
  });

  async function ackAutoRollback(id: number) {
    try {
      await api.ackAutoRollback(id);
      autoRollbacks = autoRollbacks.filter((r) => r.id !== id);
    } catch {
      /* ignore */
    }
  }
  // A link to the timeline (#/h/<host>/stacks/<name>/history?at=…) opens the History tab.
  const routeTab = $derived(app.route.page === 'stack' ? app.route.tab : undefined);
  const routeAt = $derived(app.route.page === 'stack' ? app.route.at : undefined);
  let tab = $state<'overview' | 'compose' | 'logs' | 'deps' | 'history'>(
    app.route.page === 'stack' && app.route.tab === 'history' ? 'history' : 'overview',
  );
  $effect(() => {
    // primitives only: the effect re-runs when the link changes, never on a poll
    if (routeTab === 'history' || routeAt) untrack(() => (tab = 'history'));
  });
  let stats = $state<Record<string, Stat>>({});
  // One line under the table says why some values show "–" (the same reason for every row on a host).
  const statNote = $derived(
    [...new Set(Object.values(stats).flatMap((s) => [s.cpuNote, s.memNote]).filter(Boolean))].join(' '),
  );
  let op = $state<Op | null>(null);
  let actionError = $state('');
  let menuOpen = $state(false);

  // editor drafts are separate from the polled stack so polling never overwrites typing
  let draftCompose = $state('');
  let draftEnv = $state('');
  let savedCompose = $state('');
  let savedEnv = $state('');
  let saving = $state(false);
  let saveError = $state('');
  let saveOk = $state(false);
  let validation = $state('');

  let health = $state<{ checks: Check[]; anchors: Anchor[] } | null>(null);
  let imgInfo = $state<StackImages | null>(null);
  let checkingImgs = $state(false);
  let updatingImgs = $state(false);
  let rollingBack = $state(false);
  let changingTag = $state('');
  let imgMsg = $state<{ ok: boolean; text: string } | null>(null);
  let confirmDown = $state(false);
  let confirmDelete = $state(false);
  let historyRefresh = $state(0); // bumped when an operation ends, so History shows it
  let activeTerm = $state<{ containerName: string; service: string } | null>(null);

  const dirty = $derived(draftCompose !== savedCompose || draftEnv !== savedEnv);
  const editable = $derived(!!stack?.managed && canEdit());
  const canTerminal = $derived(isAdmin() && !!currentHost()?.terminal && !currentHost()?.readOnly);
  const exitInfo = $derived.by(() => {
    const failed = stack?.containers.find((c) => c.exitCode !== undefined && c.exitCode !== 0);
    return failed ? `exit ${failed.exitCode}` : '';
  });

  async function load(first = false) {
    try {
      const s = await api.stack(name);
      stack = s;
      loadError = '';
      if (first || (!dirty && (s.compose !== savedCompose || s.env !== savedEnv))) {
        draftCompose = savedCompose = s.compose;
        draftEnv = savedEnv = s.env;
      }
    } catch (e) {
      loadError = (e as Error).message;
    }
  }

  async function loadStats() {
    if (!stack || stack.running === 0) {
      stats = {};
      return;
    }
    try {
      const list = await api.stats(name);
      stats = Object.fromEntries(list.map((s) => [s.id, s]));
    } catch {
      /* stats are best effort */
    }
  }

  let maint = $state<ActiveMaintenance | null>(null);
  async function loadMaint() {
    try {
      maint = (await api.stackMaintenance(name)).active ?? null;
    } catch {
      maint = null; // older hub API or offline: no pill
    }
  }

  async function loadHealth() {
    try {
      health = await api.health(name);
    } catch {
      health = null;
    }
  }

  async function loadImages() {
    try {
      imgInfo = await api.stackImages(name);
    } catch {
      /* best effort */
    }
  }

  async function setImgPolicy(policy: ImagePolicy) {
    if (!imgInfo) return;
    imgMsg = null;
    try {
      await api.saveStackImagePolicy(name, policy);
      imgInfo.policy = policy;
    } catch (e) {
      imgMsg = { ok: false, text: (e as Error).message };
    }
  }

  async function checkImagesNow() {
    checkingImgs = true;
    imgMsg = null;
    try {
      await api.checkStackImages(name);
      await loadImages();
    } catch (e) {
      imgMsg = { ok: false, text: (e as Error).message };
    } finally {
      checkingImgs = false;
    }
  }

  async function updateImagesNow() {
    updatingImgs = true;
    imgMsg = null;
    try {
      // The hub returns the operation at once; its output shows in the op panel, and the
      // update record + re-check land a moment after it ends (see the op panel's onDone).
      op = await api.updateStackImages(name);
      onChanged();
    } catch (e) {
      imgMsg = { ok: false, text: (e as Error).message };
    } finally {
      updatingImgs = false;
    }
  }

  async function changeTagTo(svc: ImageCheckRow, newTag: string) {
    changingTag = `${svc.service}:${newTag}`;
    imgMsg = null;
    try {
      const res = await api.changeStackImageTag(name, svc.service, svc.ref, newTag);
      await load(true);
      await loadImages();
      if (res.op) {
        op = res.op;
      }
      onChanged();
    } catch (e) {
      const msg = (e as Error).message;
      imgMsg = { ok: false, text: msg };
      if (msg.includes('Edit compose.yaml manually')) {
        tab = 'compose';
        saveError = msg;
      }
    } finally {
      changingTag = '';
    }
  }

  async function rollbackUpdate(upd: ImageUpdate) {
    rollingBack = true;
    imgMsg = null;
    try {
      op = await api.rollbackStackImages(name, upd.id);
      await loadImages();
      onChanged();
    } catch (e) {
      imgMsg = { ok: false, text: (e as Error).message };
    } finally {
      rollingBack = false;
    }
  }

  function imgStatusLabel(s: ImageCheckRow): string {
    switch (s.status) {
      case 'up-to-date':
        return 'Up to date';
      case 'update-available':
        return 'Update available';
      case 'pulled-not-applied':
        return 'Pulled, not applied';
      case 'pinned':
        return 'Pinned digest';
      case 'built':
        return 'Built locally';
      case 'local-only':
        return 'Local image only';
      case 'unsupported':
        return s.error || 'Update the agent to check images';
      case 'unreachable':
        return s.error ? `Unreachable (${s.error})` : 'Unreachable';
      default:
        return s.status || 'Not checked';
    }
  }

  function imgBadgeClass(st: ImageStatus): string {
    switch (st) {
      case 'up-to-date':
        return 'ok';
      case 'update-available':
      case 'pulled-not-applied':
        return 'warn';
      case 'unreachable':
      case 'unsupported':
        return 'bad';
      default:
        return 'neutral';
    }
  }

  // Services whose image changed; an update lists every service of the stack.
  function changedServices(u: ImageUpdate) {
    return (u.services ?? []).filter((s) => (s.oldDigest || s.oldId) !== (s.newDigest || s.newId));
  }

  function shortDigest(d?: string): string {
    if (!d) return '';
    return d.replace(/^sha256:/, '').slice(0, 7);
  }

  function shortUrlLabel(raw: string): string {
    try {
      const u = new URL(raw);
      const path = u.pathname === '/' ? '' : u.pathname.replace(/\/+$/, '');
      return u.host + path;
    } catch {
      return raw.replace(/^https?:\/\//, '');
    }
  }

  $effect(() => {
    load(true).then(loadStats);
    loadHealth();
    loadImages();
    loadMaint();
    let n = 0;
    const t = setInterval(() => {
      if (document.hidden) return;
      load();
      if (tab === 'overview') loadStats();
      if (++n % 3 === 0) {
        loadHealth();
        loadImages();
        loadMaint();
      }
    }, 5000);
    return () => clearInterval(t);
  });

  // live validation while editing (debounced)
  $effect(() => {
    const c = draftCompose;
    const e = draftEnv;
    if (!editable || (c === savedCompose && e === savedEnv)) {
      validation = '';
      return;
    }
    const t = setTimeout(async () => {
      try {
        const r = await api.validate(name, c, e);
        validation = r.ok ? '' : (r.error ?? '');
      } catch {
        /* ignore while typing */
      }
    }, 600);
    return () => clearTimeout(t);
  });

  async function run(action: string) {
    actionError = '';
    menuOpen = false;
    try {
      op = await api.action(name, action);
      onChanged();
    } catch (e) {
      actionError = (e as Error).message;
    }
  }

  // Moorage's own stack: Restart and Apply & restart take this node down for a moment.
  // On the hub's own host the page waits for Moorage to answer again and reloads.
  let selfMsg = $state('');
  const ownHub = $derived(currentHost()?.kind === 'local');
  async function waitForMoorage() {
    selfMsg = 'Moorage is restarting… this page reloads when it is back.';
    let sawDown = false;
    const until = Date.now() + 3 * 60 * 1000;
    while (Date.now() < until) {
      await new Promise((r) => setTimeout(r, 2000));
      try {
        await api.session();
        if (sawDown) {
          location.reload();
          return;
        }
      } catch {
        sawDown = true;
      }
    }
    selfMsg = sawDown
      ? 'Moorage did not come back within 3 minutes. Check the host; a failed change is rolled back automatically.'
      : 'Moorage kept running: nothing needed a restart.';
  }
  async function selfRestart() {
    await run('restart');
    if (!actionError && ownHub) void waitForMoorage();
    else if (!actionError) selfMsg = 'The agent restarts and reconnects within a minute.';
  }
  async function selfApply() {
    actionError = '';
    saveError = '';
    try {
      await api.selfApply(name, draftCompose, draftEnv);
      savedCompose = draftCompose;
      savedEnv = draftEnv;
      if (ownHub) void waitForMoorage();
      else selfMsg = 'Applying: the agent restarts and reconnects within a minute. A change that breaks it is rolled back.';
    } catch (e) {
      actionError = (e as Error).message;
    }
  }

  async function save() {
    saving = true;
    saveError = '';
    saveOk = false;
    try {
      await api.save(name, draftCompose, draftEnv);
      savedCompose = draftCompose;
      savedEnv = draftEnv;
      saveOk = true;
      setTimeout(() => (saveOk = false), 2500);
    } catch (e) {
      saveError = (e as Error).message;
    } finally {
      saving = false;
    }
  }

  async function del() {
    try {
      await api.remove(name);
      onChanged();
      go('');
    } catch (e) {
      actionError = (e as Error).message;
    }
  }

  function since(unix: number): string {
    const s = Math.max(0, Date.now() / 1000 - unix);
    if (s < 3600) return `${Math.round(s / 60)} min`;
    if (s < 86400) return `${Math.round(s / 3600)} h`;
    return `${Math.round(s / 86400)} d`;
  }
</script>

<svelte:window onkeydown={(e) => {
  if ((e.ctrlKey || e.metaKey) && e.key === 's' && tab === 'compose' && editable) { e.preventDefault(); if (dirty && !validation) save(); }
}} />

{#if loadError && !stack}
  <section class="card card-pad">
    <h1 class="title">{name}</h1>
    <p class="error-text" style="margin-top:8px">{loadError}</p>
  </section>
{:else if !stack}
  <section class="card card-pad" aria-busy="true">
    <div class="skeleton" style="height:22px;width:220px"></div>
    <div class="skeleton" style="height:12px;width:140px;margin-top:12px"></div>
  </section>
{:else}
  <div class="stackview">
    {#if autoRollbacks.length > 0}
      <section class="card card-pad" style="box-shadow:inset 3px 0 0 var(--signal)" role="alert">
        {#each autoRollbacks as rb (rb.id)}
          <div style="display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap">
            <div style="display:flex;align-items:flex-start;gap:10px">
              <Icon name="alert" />
              <div>
                <strong>Auto-rollback triggered ({new Date(rb.at).toLocaleString()})</strong>
                <div class="meta">Check <code>{rb.checkName}</code> failed after auto-update ({rb.reason}). Rolled back and switched update policy to notify.</div>
              </div>
            </div>
            {#if canEdit()}
              <button type="button" class="btn" onclick={() => ackAutoRollback(rb.id)}>Acknowledge</button>
            {/if}
          </div>
        {/each}
      </section>
    {/if}

    <section class="card card-pad head">
      <div class="head-row">
        <div class="ident">
          <h1 class="title">{stack.name}</h1>
          <div class="sub">
            <span class="badge {stack.status}">
              <span class="dot {stack.status}"></span>{statusLabel[stack.status]}{exitInfo && stack.status !== 'up' ? ` · ${exitInfo}` : ''}
            </span>
            <span class="meta num">{stack.running}/{stack.total} running</span>
            {#if maint}
              <a class="badge maintenance" href={`#/maintenance/${maint.id}`} title={maint.title}>
                <Icon name="tool" /> Maintenance until {untilText(maint.until)}
              </a>
            {/if}

            {#if !stack.managed}
              <span class="meta ro"><Icon name="lock" /> Read-only</span>
            {/if}
            {#if stack.urls?.length}
              {#each stack.urls.slice(0, 3) as u (u)}
                <a class="head-url" href={u} target="_blank" rel="noopener noreferrer" title={u}>
                  <Icon name="external" /> <span>{shortUrlLabel(u)}</span>
                </a>
              {/each}
            {/if}
          </div>
        </div>

        {#if editable && stack.self}
          <div class="actions">
            <button class="btn" onclick={selfRestart} disabled={stack.busy || stack.running === 0}><Icon name="refresh" /> Restart</button>
            <button class="btn btn-primary" onclick={selfApply} disabled={stack.busy || !!validation}
              title="Save the files and recreate Moorage; rolled back if it doesn't come back"><Icon name="play" /> Apply & restart</button>
            <div class="more" use:dismiss={() => (menuOpen = false)}>
              <button class="btn btn-icon" onclick={() => (menuOpen = !menuOpen)} aria-label="More actions" aria-expanded={menuOpen}><Icon name="dots" /></button>
              {#if menuOpen}
                <div class="menu card" role="menu">
                  <button role="menuitem" onclick={() => run('pull')}><Icon name="download" /> Pull images only</button>
                </div>
              {/if}
            </div>
          </div>
        {:else if editable}
          <div class="actions">
            <button class="btn" onclick={() => run('restart')} disabled={stack.busy || stack.running === 0}><Icon name="refresh" /> Restart</button>
            <button class="btn" onclick={() => run('stop')} disabled={stack.busy || stack.running === 0}><Icon name="stop" /> Stop</button>
            <button class="btn {dirty && tab === 'compose' ? '' : 'btn-primary'}" onclick={() => run('up')} disabled={stack.busy} title={dirty ? 'Save your changes first to deploy them' : ''}>
              <Icon name="play" /> Deploy
            </button>
            <div class="more" use:dismiss={() => (menuOpen = false)}>
              <button class="btn btn-icon" onclick={() => (menuOpen = !menuOpen)} aria-label="More actions" aria-expanded={menuOpen}><Icon name="dots" /></button>
              {#if menuOpen}
                <div class="menu card" role="menu">
                  <button role="menuitem" onclick={() => run('update')}><Icon name="cloudDown" /> Update images</button>
                  <button role="menuitem" onclick={() => run('pull')}><Icon name="download" /> Pull images only</button>
                  <hr />
                  <button role="menuitem" class="danger" onclick={() => { menuOpen = false; confirmDown = true; }}><Icon name="power" /> Down</button>
                  <button role="menuitem" class="danger" onclick={() => { menuOpen = false; confirmDelete = true; }}><Icon name="trash" /> Delete stack</button>
                </div>
              {/if}
            </div>
          </div>
        {/if}
      </div>
      {#if stack.self}
        <p class="note">
          Moorage runs from this stack, so it can't be stopped, taken down or deleted here. <strong>Apply & restart</strong> saves your
          changes and recreates Moorage from a helper container; if Moorage doesn't come back within 2 minutes, the previous files are
          put back. To update Moorage itself, use <a href="#/settings/hosts">Moorage updates</a>.
        </p>
        {#if selfMsg}<p class="note" role="status">{selfMsg}</p>{/if}
        {#if stack.selfResult && !stack.selfResult.ok}
          <p class="error-text" role="alert">Last Apply & restart ({new Date(stack.selfResult.at).toLocaleString()}): {stack.selfResult.message}</p>
        {/if}
      {/if}
      {#if !stack.managed}
        <p class="note">
          This stack runs from <code>{stack.location || 'another folder'}</code>, outside the Moorage stacks folder. You can
          watch its containers and logs. To manage it here, move its folder into the stacks folder.
        </p>
      {/if}
      {#if actionError}<p class="error-text" role="alert" style="margin-top:12px">{actionError}</p>{/if}

      <div class="tabs" role="tablist">
        <button class="tab" role="tab" aria-selected={tab === 'overview'} onclick={() => (tab = 'overview')}>Containers</button>
        {#if stack.managed}
          <button class="tab" role="tab" aria-selected={tab === 'compose'} onclick={() => (tab = 'compose')}>
            Compose{#if dirty}<span class="unsaved" title="Unsaved changes"></span>{/if}
          </button>
        {/if}
        <button class="tab" role="tab" aria-selected={tab === 'logs'} onclick={() => (tab = 'logs')}>Logs</button>
        <button class="tab" role="tab" aria-selected={tab === 'deps'} onclick={() => (tab = 'deps')}>Dependencies</button>
        {#if stack.managed}
          <button class="tab" role="tab" aria-selected={tab === 'history'} onclick={() => (tab = 'history')}>History</button>
        {/if}
      </div>
    </section>

    {#if op}
      {#key op.id}
        <OpPanel {op} onDone={() => { load(); loadImages(); setTimeout(loadImages, 2500); onChanged(); loadStats(); historyRefresh++; }} onClose={() => (op = null)} />
      {/key}
    {/if}

    {#if activeTerm && canTerminal}
      <TerminalPanel
        stack={stack.name}
        containerName={activeTerm.containerName}
        service={activeTerm.service}
        onClose={() => (activeTerm = null)}
      />
    {/if}

    {#if tab === 'overview' && stack.managed && health}
      <section class="card health" aria-label="Checks of this stack">
        {#if health.checks.length}
          {#each health.checks as c (c.id)}
            <a class="hrow" href={'#/checks/' + c.id}>
              <span class="dot {c.state.status}" title={checkStatusLabel[c.state.status]}></span>
              <span class="hname">{c.name}</span>
              <span class="hstrip"><Strip ticks={c.ticks} anchors={health.anchors} /></span>
              <span class="hup num">{pct(c.uptime24h)}</span>
            </a>
          {/each}
        {:else if editable}
          <p class="meta hempty">
            No check watches this stack yet.
            <a href={`#/checks/new?host=${encodeURIComponent(app.host)}&stack=${encodeURIComponent(stack.name)}`}>Add a check</a>
            to get alerts with its last log lines when it goes down.
          </p>
        {/if}
      </section>
    {/if}

    {#if tab === 'overview'}
      <section class="card">
        {#if stack.containers.length === 0}
          <div class="empty">
            <p class="section-title">Not deployed</p>
            <p class="meta">{editable ? 'This stack has no containers yet. Deploy it to start them.' : 'This stack has no containers yet.'}</p>
          </div>
        {:else}
          <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Container</th>
                <th>State</th>
                <th class="r">CPU</th>
                <th class="r">Memory</th>
                <th>Ports</th>
                {#if canTerminal}<th class="r"></th>{/if}
              </tr>
            </thead>
            <tbody>
              {#each stack.containers as c (c.id)}
                {@const st = stats[c.id]}
                <tr>
                  <td>
                    <div class="cname">{c.service || c.name}</div>
                    <div class="meta mono img" title={c.image}>{c.image}</div>
                    {#if c.urls?.length}
                      <div class="curls" class:muted={c.state !== 'running'}>
                        {#each c.urls as u (u)}
                          <a class="curl" href={u} target="_blank" rel="noopener noreferrer" title={u}>
                            <Icon name="external" />
                            <span>{shortUrlLabel(u)}</span>
                          </a>
                        {/each}
                      </div>
                    {/if}
                  </td>
                  <td>
                    <span class="state">
                      <span class="dot {c.state === 'running' ? (c.health === 'unhealthy' ? 'degraded' : 'up') : c.exitCode ? 'down' : ''}"></span>
                      <span>{c.status}</span>
                    </span>
                    {#if c.health && c.health !== 'none'}<div class="meta">{c.health}</div>{/if}
                  </td>
                  <td class="r mono">
                    {#if st?.cpuNote}<span title={st.cpuNote} aria-label="CPU not reported: {st.cpuNote}">–</span>{:else}{st ? st.cpu.toFixed(1) + '%' : '–'}{/if}
                  </td>
                  <td class="r mono">
                    {#if st?.memNote}<span title={st.memNote} aria-label="Memory not reported: {st.memNote}">–</span>
                    {:else if st}{formatBytes(st.memUsed)}{#if st.memLimit}<span class="lim"> / {formatBytes(st.memLimit)}</span>{/if}
                    {:else}–{/if}
                  </td>
                  <td class="mono ports">{c.ports?.join(', ') || '–'}</td>
                  {#if canTerminal}
                    <td class="r">
                      {#if c.state === 'running'}
                        <button
                          type="button"
                          class="btn btn-sm"
                          onclick={() => (activeTerm = { containerName: c.name || c.id, service: c.service || c.name })}
                        >
                          <Icon name="terminal" /> Terminal
                        </button>
                      {/if}
                    </td>
                  {/if}
                </tr>
              {/each}
            </tbody>
          </table>
          </div>
          {#if statNote}<p class="meta foot">– {statNote}</p>{/if}
          <p class="meta foot">Created {since(Math.min(...stack.containers.map((c) => c.created)))} ago · refreshes every 5 s</p>
        {/if}
      </section>

      {#if imgInfo}
        <section class="card card-pad images-card" aria-label="Images">
          <div class="img-head">
            <div class="img-title-row">
              <span class="section-title">Images</span>
              {#if imgInfo.checkedAt && !imgInfo.checkedAt.startsWith('0001-')}
                <span class="meta">Checked {new Date(imgInfo.checkedAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
              {/if}
            </div>

            <div class="img-controls">
              {#if isAdmin()}
                <div class="seg" role="group" aria-label="Image update policy">
                  <button type="button" class:active={imgInfo.policy === 'off'} onclick={() => setImgPolicy('off')}>Off</button>
                  <button type="button" class:active={imgInfo.policy === 'notify'} onclick={() => setImgPolicy('notify')}>Notify</button>
                  <button
                    type="button"
                    class:active={imgInfo.policy === 'auto'}
                    disabled={Boolean(imgInfo.autoReason)}
                    title={imgInfo.autoReason || 'Auto-apply same-tag digest updates inside the apply window'}
                    onclick={() => setImgPolicy('auto')}
                  >Auto</button>
                </div>
                <button
                  type="button"
                  class="btn"
                  onclick={checkImagesNow}
                  disabled={checkingImgs}
                  aria-busy={checkingImgs}
                >
                  <Icon name="refresh" /> {checkingImgs ? 'Checking…' : 'Check now'}
                </button>
                {#if editable}
                  <button
                    type="button"
                    class="btn"
                    onclick={updateImagesNow}
                    disabled={updatingImgs || stack.busy}
                    aria-busy={updatingImgs}
                  >
                    <Icon name="cloudDown" /> {updatingImgs ? 'Updating…' : 'Update now'}
                  </button>
                {/if}
              {:else}
                <span class="img-badge neutral">Policy: {imgInfo.policy}</span>
              {/if}
            </div>
          </div>

          {#if imgMsg}
            <div class="img-msg {imgMsg.ok ? 'ok' : 'bad'}" role="status">
              <Icon name={imgMsg.ok ? 'check' : 'alert'} />
              <span>{imgMsg.text}</span>
            </div>
          {/if}

          {#if imgInfo.services.length === 0}
            <p class="meta">Click <strong>Check now</strong> to inspect local and remote digests and version tags.</p>
          {:else}
            <div class="img-list">
              {#each imgInfo.services as svc (svc.service)}
                <div class="img-row">
                  <div class="img-ident">
                    <span class="svc-name mono">{svc.service}</span>
                    <span class="svc-ref mono">{svc.ref || '—'}</span>
                  </div>
                  <div class="img-badges">
                    <span class="img-badge {imgBadgeClass(svc.status)}">{imgStatusLabel(svc)}</span>
                    {#if svc.localDigest || svc.remoteDigest}
                      <span class="meta mono">
                        {#if svc.localDigest && svc.remoteDigest && svc.localDigest !== svc.remoteDigest}
                          {shortDigest(svc.localDigest)} → {shortDigest(svc.remoteDigest)}
                        {:else}
                          {shortDigest(svc.remoteDigest || svc.localDigest)}
                        {/if}
                      </span>
                    {/if}
                    {#if svc.newerTag}
                      {#if editable}
                        <button
                          type="button"
                          class="btn btn-sm"
                          disabled={changingTag !== '' || stack.busy}
                          onclick={() => changeTagTo(svc, svc.newerTag!)}
                        >
                          Change to <span class="mono">{svc.newerTag}</span>
                        </button>
                      {:else}
                        <span class="img-badge warn">New tag: <span class="mono">{svc.newerTag}</span></span>
                      {/if}
                    {/if}
                    {#if svc.newerMajor}
                      {#if editable}
                        <button
                          type="button"
                          class="btn btn-sm"
                          disabled={changingTag !== '' || stack.busy}
                          title="Major version upgrade"
                          onclick={() => changeTagTo(svc, svc.newerMajor!)}
                        >
                          Change to <span class="mono">{svc.newerMajor}</span> (major)
                        </button>
                      {:else}
                        <span class="img-badge warn">Major tag: <span class="mono">{svc.newerMajor}</span></span>
                      {/if}
                    {/if}
                  </div>
                </div>
              {/each}
            </div>
          {/if}

          {#if imgInfo.lastUpdate}
            {@const lu = imgInfo.lastUpdate}
            <div class="last-update">
              <div class="lu-info meta">
                <span>
                  Last update ({lu.result}) by <strong class="mono">{lu.actor}</strong> ·
                  {new Date(lu.startedAt).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}
                </span>
                {#if changedServices(lu).length}
                  <span class="mono">
                    {#each changedServices(lu) as us, idx (us.service)}
                      {#if idx > 0}, {/if}{us.service} ({shortDigest(us.oldDigest || us.oldId)} → {shortDigest(us.newDigest || us.newId)})
                    {/each}
                  </span>
                {/if}
              </div>
              {#if editable && (lu.result === 'ok' || lu.result === 'up-failed') && lu.services?.some((s) => s.oldId || s.oldDigest)}
                <button
                  type="button"
                  class="btn"
                  disabled={rollingBack || stack.busy}
                  aria-busy={rollingBack}
                  onclick={() => rollbackUpdate(lu)}
                >
                  <Icon name="rollback" /> {rollingBack ? 'Rolling back…' : 'Roll back update'}
                </button>
              {/if}
            </div>
            {#if lu.logTail && (lu.result === 'up-failed' || lu.result === 'pull-failed')}
              <pre class="lu-tail mono">{lu.logTail}</pre>
            {/if}
          {/if}
        </section>
      {/if}
    {:else if tab === 'compose'}
      <section class="card card-pad compose">
        <div class="ed-head">
          <span class="section-title">{stack.composeFile ?? 'compose.yaml'}</span>
          {#if editable}
            <div class="save">
              {#if saveOk}<span class="ok"><Icon name="check" /> Saved</span>{/if}
              {#if dirty}<button class="btn btn-ghost" onclick={() => { draftCompose = savedCompose; draftEnv = savedEnv; }}>Discard</button>{/if}
              <button class="btn {dirty ? 'btn-primary' : ''}" onclick={save} disabled={!dirty || saving || !!validation} aria-busy={saving}>Save</button>
            </div>
          {/if}
        </div>
        <CodeEditor bind:value={draftCompose} label="Compose file" readonly={!editable} minHeight={360} />
        {#if validation}
          <div class="invalid" role="alert"><Icon name="alert" /><span class="mono">{validation}</span></div>
        {/if}
        {#if saveError}<p class="error-text">{saveError}</p>{/if}

        <div class="ed-head env">
          <span class="section-title">.env</span>
          <span class="meta">Variables for <code>$&#123;VAR&#125;</code> in the compose file. Leave empty to remove the file.</span>
        </div>
        <EnvEditor bind:value={draftEnv} compose={draftCompose} readonly={!editable} />
        {#if dirty}<p class="meta">Save, then Deploy to apply the changes.</p>{/if}
      </section>
    {:else if tab === 'deps'}
      <StackDeps name={stack.name} />
    {:else if tab === 'history'}
      <HistoryView name={stack.name} at={routeAt} {editable} {dirty} refresh={historyRefresh} onRestored={(o) => { if (o) op = o; load(true); onChanged(); }} />
    {:else}
      <section class="card card-pad">
        <LogViewer stack={stack.name} containers={stack.containers} />
      </section>
    {/if}
  </div>

  <Confirm bind:open={confirmDown} title="Take {stack.name} down?" danger confirmLabel="Down"
    body="Stops and removes the containers and networks. Volumes and the compose file stay." onConfirm={() => run('down')} />
  <Confirm bind:open={confirmDelete} title="Delete {stack.name}?" danger confirmLabel="Delete stack"
    body="Removes the stack folder, including any data stored inside it. The compose and .env files stay in the version history. Take the stack down first."
    onConfirm={del} />
{/if}

<style>
  .stackview {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head {
    padding-bottom: 12px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 16px;
    flex-wrap: wrap;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
    word-break: break-word;
  }
  .sub {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-top: 8px;
    flex-wrap: wrap;
  }
  .badge .dot {
    width: 6px;
    height: 6px;
  }
  .ro {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .head-url {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 2px 9px;
    border-radius: 999px;
    background: var(--buoy-wash);
    color: var(--buoy-text);
    font-size: 12px;
    font-weight: 500;
    text-decoration: none;
    max-width: 240px;
  }
  .head-url span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .head-url:hover {
    text-decoration: underline;
  }
  .actions {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
    transform: none;
  }
  .more {
    position: relative;
  }
  .menu {
    position: absolute;
    right: 0;
    top: 38px;
    z-index: 10;
    min-width: 200px;
    padding: 4px;
    background: var(--pier-raised);
    box-shadow: var(--lift-raised);
    transform-origin: top right;
    animation: menu 160ms var(--ease-out);
  }
  @keyframes menu {
    from {
      opacity: 0;
      transform: scale(0.96);
    }
  }
  .menu button {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    height: 34px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--mist);
    font: 400 13px var(--font);
    cursor: pointer;
    text-align: left;
  }
  .menu button:hover {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .menu .danger {
    color: var(--adrift-text);
  }
  .menu hr {
    border: 0;
    border-top: 1px solid var(--rope);
    margin: 4px 0;
  }
  .note {
    margin-top: 14px;
    color: var(--mist);
    font-size: 13px;
    max-width: 70ch;
  }
  .tabs {
    margin-top: 18px;
    margin-left: -4px;
  }
  .unsaved {
    display: inline-block;
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--buoy);
    margin-left: 6px;
    vertical-align: middle;
  }
  table {
    width: 100%;
    border-collapse: collapse;
  }
  th {
    text-align: left;
    font-size: 11px;
    font-weight: 500;
    letter-spacing: 0.04em;
    color: var(--haze);
    padding: 14px 12px 8px;
  }
  td {
    padding: 10px 12px;
    border-top: 1px solid var(--rope);
    vertical-align: top;
  }
  th:first-child,
  td:first-child {
    padding-left: 20px;
  }
  th:last-child,
  td:last-child {
    padding-right: 20px;
  }
  .r {
    text-align: right;
  }
  td.mono {
    font-size: 12px;
    padding-top: 12px;
  }
  .cname {
    font-weight: 500;
  }
  .curls {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 10px;
    margin-top: 4px;
  }
  .curl {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 12px;
    color: var(--buoy-text);
    text-decoration: none;
    max-width: 280px;
  }
  .curl span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .curl:hover {
    text-decoration: underline;
  }
  .curls.muted .curl {
    color: var(--haze);
  }
  .img {
    max-width: 280px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .state {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    color: var(--mist);
  }
  .lim {
    color: var(--haze);
  }
  .ports {
    color: var(--mist);
  }
  .foot {
    padding: 10px 20px 14px;
    border-top: 1px solid var(--rope);
  }
  .empty {
    padding: 24px 20px;
    display: grid;
    gap: 4px;
  }
  .compose {
    display: grid;
    gap: 10px;
  }
  .ed-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    min-height: 32px;
  }
  .ed-head.env {
    margin-top: 10px;
    justify-content: flex-start;
  }
  .save {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .ok {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    color: var(--afloat-text);
    font-size: 13px;
  }
  .invalid {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    padding: 10px 14px;
    border-radius: var(--r-md);
    background: var(--adrift-wash);
    color: var(--adrift-text);
    font-size: 12px;
    white-space: pre-wrap;
  }
  .invalid .icon {
    margin-top: 2px;
  }

  .table-wrap {
    overflow-x: auto;
  }
  .health {
    padding: 6px 0;
  }
  .hrow {
    display: grid;
    grid-template-columns: 8px minmax(100px, 200px) minmax(0, 1fr) 64px;
    gap: 14px;
    align-items: center;
    padding: 8px 20px;
    color: inherit;
    text-decoration: none;
  }
  .hrow:hover {
    background: var(--rope);
    text-decoration: none;
  }
  .hname {
    font-weight: 500;
    color: var(--foam);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .hstrip {
    min-width: 0;
  }
  .hup {
    text-align: right;
    font-weight: 600;
    color: var(--foam);
  }
  .hempty {
    padding: 8px 20px;
  }
  td.mono {
    white-space: nowrap;
  }
  .images-card {
    display: grid;
    gap: 12px;
  }
  .img-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
  }
  .img-title-row {
    display: flex;
    align-items: baseline;
    gap: 10px;
  }
  .img-controls {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .seg {
    display: inline-flex;
    background: var(--well);
    border-radius: var(--r-md);
    padding: 2px;
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .seg button {
    border: 0;
    background: transparent;
    color: var(--mist);
    font: 500 12px var(--font);
    padding: 4px 10px;
    border-radius: var(--r-sm);
    cursor: pointer;
  }
  .seg button.active {
    background: var(--buoy-wash);
    color: var(--foam);
  }
  .seg button:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
  .img-msg {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    border-radius: var(--r-md);
    font-size: 13px;
  }
  .img-msg.ok {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .img-msg.bad {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .img-list {
    display: grid;
    gap: 8px;
  }
  .img-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 8px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    flex-wrap: wrap;
  }
  .img-ident {
    display: flex;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .svc-name {
    font-weight: 600;
    color: var(--foam);
  }
  .svc-ref {
    color: var(--mist);
    font-size: 13px;
  }
  .img-badges {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .img-badge {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
  }
  .img-badge.ok {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .img-badge.warn {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .img-badge.bad {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .img-badge.neutral {
    background: var(--rope);
    color: var(--mist);
  }
  .btn-sm {
    height: 26px;
    padding: 0 10px;
    font-size: 12px;
  }
  .last-update {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding-top: 10px;
    border-top: 1px solid var(--rope);
    flex-wrap: wrap;
  }
  .lu-info {
    display: grid;
    gap: 2px;
    font-size: 12px;
  }
  .lu-tail {
    margin: 0;
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    color: var(--adrift-text);
    font-size: 11px;
    overflow-x: auto;
    max-height: 180px;
  }

  @media (max-width: 767px) {
    th:nth-child(5),
    td:nth-child(5),
    .lim {
      display: none;
    }
    th,
    td {
      padding-left: 8px;
      padding-right: 8px;
    }
    .img {
      max-width: 140px;
    }
  }
</style>
