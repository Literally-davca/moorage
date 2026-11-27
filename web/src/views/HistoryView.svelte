<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Op, type TimelineEntry, type TimelineKind, type VersionDetail } from '../lib/api';
  import { auditLabel } from '../lib/audit';
  import { currentHost, isAdmin, link } from '../lib/state.svelte';
  import Icon, { type IconName } from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import { chat as assistantChat, ask as askAssistant } from '../lib/assistantStore.svelte';

  let {
    name,
    coolifyUuid = '',
    at,
    editable,
    dirty,
    onRestored,
    refresh = 0,
  }: {
    name: string;
    coolifyUuid?: string;
    at?: number;
    editable: boolean;
    dirty: boolean;
    onRestored: (op: Op | null) => void;
    /** Bumped by the stack page when an operation finished, so its entries show up. */
    refresh?: number;
  } = $props();

  // M19I: Explain this alert on an incident row – same question as on the check page,
  // filled in the assistant panel; nothing is sent until Enter.
  const assistantOn = $derived(!!assistantChat.info?.enabled && !!assistantChat.info?.configured);
  function explainIncident(e: TimelineEntry) {
    const inc = e.incident;
    if (!inc) return;
    const host = currentHost();
    askAssistant(
      `Explain this alert: check "${inc.checkName}" of stack ${name} on host ${host?.name ?? host?.id ?? ''} went down at ` +
        `${new Date(inc.startedAt).toLocaleString()} (incident #${e.ref.incidentId ?? '?'}, check #${e.ref.checkId ?? '?'}, cause: ${inc.cause}). ` +
        `What failed, why, what changed before it, and how do I fix it?`,
    );
  }

  let coolifyDeploys = $state<{ uuid: string; status: string; commit?: string; logs?: string; createdAt?: string }[]>([]);
  let expandedDeploy = $state('');

  $effect(() => {
    const cuuid = coolifyUuid;
    if (!cuuid) {
      coolifyDeploys = [];
      return;
    }
    api.coolifyDeployments(cuuid)
      .then((r) => (coolifyDeploys = r.deployments ?? []))
      .catch(() => (coolifyDeploys = []));
  });

  // One merged timeline: versions, stack events, incidents, image updates and, for admins,
  // audit rows and terminal sessions. The server leaves the admin-only kinds out for viewers.
  const kindInfo: Record<TimelineKind, { label: string; plural: string; icon: IconName }> = {
    version: { label: 'Version', plural: 'Versions', icon: 'history' },
    event: { label: 'Event', plural: 'Events', icon: 'play' },
    incident: { label: 'Incident', plural: 'Incidents', icon: 'alert' },
    update: { label: 'Image update', plural: 'Image updates', icon: 'cloudDown' },
    audit: { label: 'Audit', plural: 'Audit', icon: 'clipboardList' },
    terminal: { label: 'Terminal', plural: 'Terminal', icon: 'terminal' },
  };
  const allKinds = $derived<TimelineKind[]>(
    isAdmin() ? ['version', 'event', 'incident', 'update', 'audit', 'terminal'] : ['version', 'event', 'incident', 'update'],
  );

  const WINDOW = 2 * 3600; // "what changed before": the 2 h before an incident started
  let hidden = $state<TimelineKind[]>([]);
  // the incident start (unix s) the timeline is narrowed to, or 0 for everything
  let windowAt = $state(untrack(() => at ?? 0));
  let entries = $state<TimelineEntry[] | null>(null);
  let next = $state<string | undefined>(undefined);
  let more = $state(false);
  let loadingMore = $state(false);
  let versionsError = $state('');
  let listError = $state('');
  let selected = $state<string | null>(null);
  let detail = $state<VersionDetail | null>(null);
  let detailError = $state('');
  let confirmOpen = $state(false);
  let deployAfter = $state(true);
  let restoreError = $state('');
  let restoring = $state(false);
  let logbook = $state<HTMLElement | null>(null);

  const shownKinds = $derived(allKinds.filter((k) => !hidden.includes(k)));
  // derived primitive keys: the parent re-creates its stack object on every poll
  const stackName = $derived(name);
  const kindsKey = $derived(shownKinds.join(','));
  const routeAt = $derived(at ?? 0);

  // A new link from a check page moves the window there.
  $effect(() => {
    const a = routeAt;
    if (a) untrack(() => (windowAt = a));
  });

  function query(before?: string) {
    return {
      before,
      until: !before && windowAt ? windowAt : undefined,
      since: windowAt ? windowAt - WINDOW : undefined,
      limit: 100,
      kinds: shownKinds,
    };
  }

  async function load(select?: string) {
    try {
      const p = await api.timeline(name, query());
      entries = p.entries;
      next = p.next;
      more = p.more;
      versionsError = p.versionsError ?? '';
      listError = '';
      const want = select ?? selected;
      const versions = p.entries.filter((e) => e.kind === 'version');
      selected = versions.some((e) => e.ref.hash === want) ? (want ?? null) : (versions[0]?.ref.hash ?? null);
    } catch (e) {
      listError = (e as Error).message;
    }
  }

  async function loadMore() {
    if (!next || loadingMore) return;
    loadingMore = true;
    try {
      const p = await api.timeline(name, query(next));
      const seen = new Set(entries?.map((e) => e.key));
      entries = [...(entries ?? []), ...p.entries.filter((e) => !seen.has(e.key))];
      next = p.next;
      more = p.more;
      if (!selected) selected = entries.find((e) => e.kind === 'version')?.ref.hash ?? null;
    } catch (e) {
      listError = (e as Error).message;
    } finally {
      loadingMore = false;
    }
  }

  $effect(() => {
    void stackName;
    void kindsKey;
    void windowAt;
    untrack(() => {
      entries = null;
      load();
    });
  });

  // An operation on the stack page finished: reload the first page in place (no flash).
  // Only the counter is read, so the 5 s stack poll never reloads the timeline.
  const refreshKey = $derived(refresh);
  $effect(() => {
    if (refreshKey > 0)
      untrack(() => {
        if (!windowAt) load();
      });
  });

  // load the diff of the selected version
  $effect(() => {
    const h = selected;
    const n = stackName;
    detail = null;
    detailError = '';
    if (!h) return;
    let gone = false;
    api
      .version(n, h)
      .then((d) => !gone && (detail = d))
      .catch((e) => !gone && (detailError = (e as Error).message));
    return () => (gone = true);
  });

  function toggleKind(k: TimelineKind) {
    if (hidden.includes(k)) hidden = hidden.filter((x) => x !== k);
    else if (shownKinds.length > 1) hidden = [...hidden, k];
  }

  function showBefore(e: TimelineEntry) {
    const start = new Date(e.incident?.startedAt ?? e.at);
    windowAt = Math.floor(start.getTime() / 1000);
    logbook?.scrollTo({ top: 0 });
  }

  function showAll() {
    windowAt = 0;
    // drop ?at= from the address, so a reload shows everything too
    history.replaceState(null, '', link('stacks/' + encodeURIComponent(name)) + '/history');
  }

  async function restore() {
    if (!detail) return;
    restoring = true;
    restoreError = '';
    try {
      const r = await api.restore(name, detail.hash, deployAfter);
      if (r.deployError) restoreError = `Restored, but the deploy didn't start: ${r.deployError}`;
      onRestored(r.op ?? null);
      await load(r.version?.hash);
    } catch (e) {
      restoreError = (e as Error).message;
    } finally {
      restoring = false;
    }
  }

  async function rollbackImgUpdate(id: number) {
    try {
      const op = await api.rollbackStackImages(name, id);
      onRestored(op);
      await load();
    } catch (e) {
      restoreError = (e as Error).message;
    }
  }

  const dayFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric' });
  const timeFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
  const fullFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });

  // entries grouped by calendar day, newest first
  const days = $derived.by(() => {
    const out: { day: string; items: TimelineEntry[] }[] = [];
    for (const e of entries ?? []) {
      const day = dayFmt.format(new Date(e.at));
      if (out.at(-1)?.day !== day) out.push({ day, items: [] });
      out.at(-1)!.items.push(e);
    }
    return out;
  });

  // the incident the window was opened for (its start is the window's end)
  const windowIncident = $derived(
    windowAt
      ? entries?.find((e) => e.incident?.phase === 'open' && Math.floor(new Date(e.at).getTime() / 1000) === windowAt)
      : undefined,
  );

  /** A change inside the "before" window: everything but incidents. */
  const isChange = (e: TimelineEntry) => !!windowAt && e.kind !== 'incident';

  function title(e: TimelineEntry): string {
    if (e.kind === 'audit') return auditLabel(e.title) + (e.outcome === 'failed' ? ' · failed' : '');
    let t = e.title;
    if (e.version?.current) t += ' · current';
    if (e.outcome === 'failed' && e.kind !== 'update') t += ' · failed';
    return t;
  }

  function byLine(e: TimelineEntry): string {
    const parts: string[] = [];
    if (e.kind === 'version' && !e.actor) parts.push('Detected by Moorage');
    else if (e.actor) parts.push(e.ip ? `${e.actor} (${e.ip})` : e.actor);
    if (e.kind === 'incident') parts.push(e.incident?.checkName ?? '');
    if (e.version) parts.push(e.version.short);
    if (e.merged?.includes('audit') && e.action) parts.push(`audit: ${auditLabel(e.action)}`);
    return parts.filter(Boolean).join(' · ');
  }

  function kindClass(e: TimelineEntry): string {
    if (e.kind === 'version') return e.version?.kind ?? '';
    if (e.kind === 'incident') return e.incident?.phase === 'open' ? 'delete' : 'up';
    if (e.kind === 'update') return e.update?.result === 'rolled-back' ? 'restore' : e.update?.result === 'ok' ? '' : 'delete';
    if (e.eventKind === 'maintenance') return 'maint';
    return e.outcome === 'failed' ? 'delete' : '';
  }

  function ago(iso: string): string {
    const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 60) return 'just now';
    if (s < 3600) return `${Math.round(s / 60)} min ago`;
    if (s < 86400) return `${Math.round(s / 3600)} h ago`;
    return `${Math.round(s / 86400)} d ago`;
  }
</script>

{#if coolifyDeploys.length > 0}
  <section class="card card-pad" style="margin-bottom:12px" aria-label="Coolify deployments">
    <h3 class="section-title" style="margin-bottom:8px">Coolify deployments</h3>
    <div style="display:grid;gap:6px">
      {#each coolifyDeploys.slice(0, 10) as dep (dep.uuid)}
        <div style="padding:8px 10px;border-radius:var(--r-md);background:var(--well);box-shadow:inset 0 0 0 1px var(--rope)">
          <div style="display:flex;align-items:center;justify-content:space-between;gap:8px;flex-wrap:wrap">
            <div style="display:flex;align-items:center;gap:8px">
              <span class="badge {dep.status === 'finished' ? 'up' : dep.status === 'failed' ? 'down' : 'degraded'}">{dep.status}</span>
              {#if dep.commit}<span class="mono meta">{dep.commit.slice(0, 8)}</span>{/if}
              {#if dep.createdAt}<span class="meta">{new Date(dep.createdAt).toLocaleString()}</span>{/if}
            </div>
            {#if dep.logs}
              <button type="button" class="btn btn-sm" onclick={() => (expandedDeploy = expandedDeploy === dep.uuid ? '' : dep.uuid)}>
                {expandedDeploy === dep.uuid ? 'Hide build log' : 'View build log'}
              </button>
            {/if}
          </div>
          {#if expandedDeploy === dep.uuid && dep.logs}
            <pre class="mono" style="margin:8px 0 0;max-height:260px;overflow:auto;font-size:11px;white-space:pre-wrap">{dep.logs}</pre>
          {/if}
        </div>
      {/each}
    </div>
  </section>
{/if}

{#snippet entryBody(e: TimelineEntry)}
  <span class="time mono">{timeFmt.format(new Date(e.at))}</span>
  <span class="body">
    <span class="kind {kindClass(e)}">
      <Icon name={kindInfo[e.kind].icon} />
      <span class="klabel">{kindInfo[e.kind].label}:</span>
      {title(e)}
    </span>
    {#if e.detail}<span class="msg" class:mono={e.kind === 'update' || e.kind === 'terminal' || e.kind === 'audit'}>{e.detail}</span>{/if}
    {#if byLine(e)}<span class="by meta">{byLine(e)}</span>{/if}
    {#if windowIncident?.key === e.key}<span class="tag anchor-tag">This incident</span>
    {:else if isChange(e)}<span class="tag">Changed before the incident</span>{/if}
  </span>
{/snippet}

<div class="filters" role="group" aria-label="Show in the timeline">
  {#each allKinds as k (k)}
    <button type="button" class="chip" aria-pressed={!hidden.includes(k)} onclick={() => toggleKind(k)}>
      <Icon name={kindInfo[k].icon} />
      {kindInfo[k].plural}
    </button>
  {/each}
</div>

{#if windowAt}
  <section class="card window" aria-live="polite">
    <p>
      Changes in the 2 h before {windowIncident ? `${windowIncident.incident?.checkName} went down` : 'the incident'} at
      <time datetime={new Date(windowAt * 1000).toISOString()}>{fullFmt.format(new Date(windowAt * 1000))}</time>.
    </p>
    <button type="button" class="btn" onclick={showAll}>Show everything</button>
  </section>
{/if}

{#if listError && !entries}
  <section class="card card-pad"><p class="error-text" role="alert">{listError}</p></section>
{:else if !entries}
  <section class="card card-pad" aria-busy="true">
    <div class="skeleton" style="height:14px;width:60%"></div>
    <div class="skeleton" style="height:14px;width:40%;margin-top:12px"></div>
  </section>
{:else}
  <div class="history">
    <section class="card logbook" aria-label="Timeline" bind:this={logbook}>
      {#if versionsError}<p class="meta note" role="status">{versionsError}</p>{/if}
      {#if listError}<p class="error-text note" role="alert">{listError}</p>{/if}
      {#if entries.length === 0}
        <p class="meta note">
          {windowAt ? 'Nothing happened in these 2 hours.' : 'Nothing here yet. Saves, deploys, incidents and image updates show up here.'}
        </p>
      {/if}
      {#each days as d (d.day)}
        <h3 class="day">{d.day}</h3>
        <ul>
          {#each d.items as e (e.key)}
            <li class:change={isChange(e)} class:anchor={windowIncident?.key === e.key}>
              {#if e.kind === 'version'}
                <button class="entry" class:sel={e.ref.hash === selected} onclick={() => (selected = e.ref.hash ?? null)} aria-current={e.ref.hash === selected}>
                  {@render entryBody(e)}
                </button>
              {:else}
                <div class="entry static">{@render entryBody(e)}</div>
                {#if e.kind === 'incident' && e.incident?.phase === 'open'}
                  <div class="acts">
                    <button type="button" class="link" onclick={() => showBefore(e)}>Show what changed before</button>
                    <a class="link" href={`#/checks/${e.ref.checkId}`}>Open check</a>
                    {#if assistantOn}
                      <button type="button" class="link" onclick={() => explainIncident(e)}>Explain this alert</button>
                    {/if}
                    {#if e.incident.causedBy}
                      <a class="link" href={`#/h/${encodeURIComponent(e.incident.causedBy.hostId)}/stacks/${encodeURIComponent(e.incident.causedBy.stack)}/history`}>
                        Root cause: {e.incident.causedBy.stack}
                      </a>
                    {/if}
                  </div>
                {:else if e.kind === 'update' && editable && e.update?.canRollback && e.ref.updateId}
                  <div class="acts">
                    <button type="button" class="link" onclick={() => rollbackImgUpdate(e.ref.updateId!)}>Roll back update</button>
                  </div>
                {/if}
              {/if}
            </li>
          {/each}
        </ul>
      {/each}
      {#if more}
        <div class="more">
          <button type="button" class="btn" onclick={loadMore} disabled={loadingMore}>{loadingMore ? 'Loading…' : 'Load more'}</button>
        </div>
      {/if}
    </section>

    <section class="card diffcard" aria-label="Changes">
      {#if restoreError && !detail}<p class="error-text pad" role="alert">{restoreError}</p>{/if}
      {#if !selected}
        <p class="meta pad">Pick a version in the timeline to see what changed in the compose file and .env.</p>
      {:else if detailError}
        <p class="error-text pad" role="alert">{detailError}</p>
      {:else if !detail}
        <div class="pad" aria-busy="true"><div class="skeleton" style="height:14px;width:50%"></div></div>
      {:else}
        {@const cur = entries.find((x) => x.ref.hash === detail?.hash)}
        <header class="dhead">
          <div>
            <h2 class="dtitle">{detail.message}</h2>
            <p class="meta">
              {detail.author === 'moorage' ? 'Detected by Moorage' : detail.author} ·
              <time datetime={detail.time} title={new Date(detail.time).toLocaleString()}>{ago(detail.time)}</time> ·
              <span class="mono">{detail.short}</span>
              {#if detail.restored} · restored from <button class="link mono" onclick={() => (selected = entries?.find((x) => x.version?.short === detail?.restored)?.ref.hash ?? selected)}>{detail.restored}</button>{/if}
            </p>
          </div>
          {#if editable && cur && !cur.version?.current && detail.kind !== 'delete'}
            <button class="btn" onclick={() => { restoreError = ''; confirmOpen = true; }} disabled={dirty || restoring}
              title={dirty ? 'Save or discard your edits in the Compose tab first' : ''}>
              <Icon name="rollback" /> Roll back to this version
            </button>
          {/if}
        </header>
        {#if restoreError}<p class="error-text pad-x" role="alert">{restoreError}</p>{/if}

        {#each detail.diff as f (f.name)}
          <div class="file">
            <div class="fname">
              <span class="mono">{f.name}</span>
              <span class="fstatus {f.status}">{f.status}</span>
            </div>
            {#if f.binary}
              <p class="meta pad-x">Binary file, no preview.</p>
            {:else if !f.hunks?.length}
              <p class="meta pad-x">Empty file.</p>
            {:else}
              <div class="code" role="table" aria-label="Changes in {f.name}">
                {#each f.hunks as h, i (i)}
                  {#if i > 0}<div class="gap" aria-hidden="true">⋯</div>{/if}
                  {#each h.lines as l, j (j)}
                    <div class="ln {l.op === '+' ? 'add' : l.op === '-' ? 'del' : ''}" role="row">
                      <span class="no" role="cell">{l.old ?? ''}</span>
                      <span class="no" role="cell">{l.new ?? ''}</span>
                      <span class="op" role="cell" aria-label={l.op === '+' ? 'added' : l.op === '-' ? 'removed' : undefined}>{l.op}</span>
                      <span class="tx" role="cell">{l.text}</span>
                    </div>
                  {/each}
                {/each}
              </div>
            {/if}
          </div>
        {/each}
      {/if}
    </section>
  </div>

  <Confirm bind:open={confirmOpen} title="Roll back {name} to {detail?.short}?" confirmLabel={deployAfter ? 'Roll back and deploy' : 'Roll back'}
    body="The compose file and .env return to this version. The current files stay in the history, so you can come back to them."
    onConfirm={restore}>
    <label class="deploy">
      <input type="checkbox" bind:checked={deployAfter} />
      <span>Deploy right after, so the containers match</span>
    </label>
  </Confirm>
{/if}

<style>
  .filters {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin-bottom: 10px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 4px 10px;
    border: 1px solid var(--rope);
    border-radius: 999px;
    background: transparent;
    color: var(--haze);
    font: inherit;
    font-size: 12px;
    cursor: pointer;
  }
  .chip[aria-pressed='true'] {
    background: var(--buoy-wash);
    border-color: var(--buoy);
    color: var(--foam);
  }
  .chip:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: 1px;
  }
  .window {
    display: flex;
    align-items: center;
    justify-content: space-between;
    flex-wrap: wrap;
    gap: 10px;
    padding: 10px 16px;
    margin-bottom: 10px;
    font-size: 13px;
    color: var(--foam);
  }
  .note {
    padding: 8px 16px;
    font-size: 12px;
  }
  .entry.static {
    cursor: default;
  }
  .entry.static:hover {
    background: transparent;
  }
  .kind :global(.icon) {
    vertical-align: -2px;
  }
  .klabel {
    color: var(--haze);
  }
  .kind.up {
    color: var(--afloat-text);
  }
  li.change > .entry {
    box-shadow: inset 2px 0 0 var(--signal-text);
  }
  li.anchor > .entry {
    box-shadow: inset 2px 0 0 var(--adrift-text);
  }
  .tag {
    justify-self: start;
    margin-top: 2px;
    font-size: 11px;
    color: var(--signal-text);
  }
  .anchor-tag {
    color: var(--adrift-text);
  }
  .acts {
    display: flex;
    flex-wrap: wrap;
    gap: 12px;
    padding: 0 10px 6px 62px;
    font-size: 12px;
  }
  .more {
    padding: 8px 16px 4px;
  }
  .history {
    display: grid;
    grid-template-columns: minmax(240px, 320px) minmax(0, 1fr);
    gap: 12px;
    align-items: start;
  }
  .empty {
    display: grid;
    gap: 4px;
  }
  .logbook {
    padding: 8px 0 10px;
    max-height: calc(100vh - 260px);
    overflow-y: auto;
  }
  .day {
    font-size: 11px;
    font-weight: 500;
    letter-spacing: 0.04em;
    color: var(--haze);
    padding: 12px 16px 6px;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0 6px;
  }
  .entry {
    display: grid;
    grid-template-columns: 44px minmax(0, 1fr);
    gap: 8px;
    width: 100%;
    padding: 8px 10px;
    border: 0;
    border-radius: var(--r-md);
    background: transparent;
    color: inherit;
    font: inherit;
    text-align: left;
    cursor: pointer;
    line-height: 1.45;
  }
  .entry:hover {
    background: var(--rope);
  }
  .entry.sel {
    background: var(--buoy-wash);
  }
  .entry:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: -2px;
  }
  .time {
    font-size: 12px;
    color: var(--murk);
    padding-top: 1px;
  }
  .body {
    display: grid;
    min-width: 0;
  }
  .kind {
    font-size: 11px;
    font-weight: 500;
    letter-spacing: 0.02em;
    color: var(--mist);
  }
  .kind.restore {
    color: var(--buoy-text);
  }
  .kind.external {
    color: var(--signal-text);
  }
  .kind.delete {
    color: var(--adrift-text);
  }
  .kind.maint {
    color: var(--signal-text);
  }
  .msg {
    color: var(--foam);
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .by {
    font-size: 12px;
  }

  .diffcard {
    min-width: 0;
    padding-bottom: 8px;
  }
  .pad {
    padding: 20px;
  }
  .pad-x {
    padding: 0 20px 12px;
  }
  .dhead {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 16px;
    padding: 18px 20px 14px;
    flex-wrap: wrap;
  }
  .dtitle {
    font-size: 16px;
    font-weight: 600;
    letter-spacing: -0.01em;
    margin-bottom: 4px;
  }
  .link {
    border: 0;
    background: none;
    padding: 0;
    color: var(--buoy-text);
    cursor: pointer;
    font-size: inherit;
  }
  .link:hover {
    text-decoration: underline;
  }
  .btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
  .file {
    border-top: 1px solid var(--rope);
  }
  .fname {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 10px 20px;
    font-size: 13px;
  }
  .fstatus {
    font-size: 11px;
    color: var(--haze);
  }
  .fstatus.added {
    color: var(--afloat-text);
  }
  .fstatus.deleted {
    color: var(--adrift-text);
  }
  .code {
    margin: 0 12px 12px;
    background: var(--well);
    border-radius: var(--r-md);
    font: 12px/1.7 var(--mono);
    overflow-x: auto;
    padding: 6px 0;
  }
  .ln {
    display: grid;
    grid-template-columns: 36px 36px 16px minmax(0, 1fr);
    min-width: max-content;
  }
  .ln.add {
    background: var(--afloat-wash);
  }
  .ln.del {
    background: var(--adrift-wash);
  }
  .no {
    color: var(--murk);
    text-align: right;
    padding-right: 8px;
    user-select: none;
  }
  .op {
    color: var(--haze);
    user-select: none;
  }
  .ln.add .op {
    color: var(--afloat-text);
  }
  .ln.del .op {
    color: var(--adrift-text);
  }
  .tx {
    white-space: pre;
    padding-right: 16px;
    color: var(--mist);
  }
  .ln.add .tx,
  .ln.del .tx {
    color: var(--foam);
  }
  .gap {
    color: var(--murk);
    padding: 2px 0 2px 88px;
    user-select: none;
  }
  .deploy {
    display: flex;
    gap: 10px;
    align-items: center;
    font-size: 13px;
    color: var(--mist);
    cursor: pointer;
  }
  .deploy input {
    accent-color: var(--buoy);
    width: 16px;
    height: 16px;
  }

  @media (max-width: 899px) {
    .history {
      grid-template-columns: minmax(0, 1fr);
    }
    .logbook {
      max-height: 280px;
    }
  }
</style>
