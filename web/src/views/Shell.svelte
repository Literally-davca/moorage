<script lang="ts">
  import { untrack } from 'svelte';
  import { api, engineLabel, statusLabel, type HostEntry, type HostInfo, type StackSummary } from '../lib/api';
  import { app, canEdit, currentHost, go, isAdmin, link, setHost, toggleTheme } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import StackDetail from './StackDetail.svelte';
  import NewStack from './NewStack.svelte';
  import Home from './Home.svelte';
  import HostsView from './HostsView.svelte';
  import ChecksView from './ChecksView.svelte';
  import CheckDetail from './CheckDetail.svelte';
  import CheckForm from './CheckForm.svelte';
  import StatusPagesView from './StatusPagesView.svelte';
  import StatusPageForm from './StatusPageForm.svelte';
  import UpdatesView from './UpdatesView.svelte';
  import NotificationsView from './NotificationsView.svelte';
  import AuditView from './AuditView.svelte';
  import MaintenanceView from './MaintenanceView.svelte';
  import MaintenanceForm from './MaintenanceForm.svelte';
  import DepsView from './DepsView.svelte';
  import TemplatesView from './TemplatesView.svelte';
  import TemplateDeploy from './TemplateDeploy.svelte';
  import TemplateSources from './TemplateSources.svelte';

  let { onSignedOut }: { onSignedOut: () => void } = $props();

  let stacks = $state<StackSummary[] | null>(null);
  let listError = $state('');
  let host = $state<HostInfo | null>(null);
  let query = $state('');
  let navOpen = $state(false);
  let hostMenu = $state(false);
  let hostsLoaded = $state(false);

  const cur = $derived(currentHost());
  let checksDown = $state(0);
  const onChecks = $derived(['checks', 'check', 'checkForm'].includes(app.route.page));
  const onStatusPages = $derived(['statusPages', 'statusPageForm'].includes(app.route.page));
  const onMaintenance = $derived(['maintenance', 'maintenanceForm'].includes(app.route.page));

  async function loadCheckCount() {
    try {
      checksDown = (await api.checks()).filter((c) => c.state.status === 'down').length;
    } catch {
      /* not important */
    }
  }
  $effect(() => {
    untrack(loadCheckCount);
    const t = setInterval(() => !document.hidden && loadCheckCount(), 15000);
    return () => clearInterval(t);
  });

  const filtered = $derived(
    (stacks ?? []).filter((s) => s.name.includes(query.trim().toLowerCase())),
  );
  const counts = $derived({
    up: (stacks ?? []).filter((s) => s.status === 'up').length,
    issues: (stacks ?? []).filter((s) => s.status === 'down' || s.status === 'degraded').length,
  });
  const selected = $derived(app.route.page === 'stack' ? app.route.name : '');

  export async function refresh() {
    loadHosts();
    if (!app.hosts.length && hostsLoaded) {
      stacks = [];
      return;
    }
    const want = app.host;
    try {
      const r = await api.stacks();
      if (want !== app.host) return; // the user switched hosts meanwhile
      stacks = r.stacks;
      listError = r.error ?? '';
    } catch (e) {
      if (want !== app.host) return;
      stacks = [];
      listError = (e as Error).message;
    }
  }

  async function loadHosts() {
    try {
      const r = await api.hosts();
      app.hosts = r.hosts;
      hostsLoaded = true;
      // first start in hub mode, or a remembered host that was removed
      if (r.hosts.length && !r.hosts.some((h) => h.id === app.host)) setHost(r.hosts[0].id, app.route.page !== 'hosts');
    } catch {
      /* the stack list shows the error */
    }
  }

  async function loadHost() {
    try {
      host = await api.host();
    } catch {
      host = null;
    }
  }

  // reload everything when the host changes; poll every 5 s
  $effect(() => {
    void app.host;
    stacks = null;
    listError = '';
    host = null;
    // untracked: refresh reads app.hosts, which loadHosts replaces on every poll
    untrack(() => {
      refresh();
      loadHost();
    });
    const t = setInterval(() => {
      if (!document.hidden) refresh();
    }, 5000);
    return () => clearInterval(t);
  });

  function hostDot(h: HostEntry | undefined): string {
    if (!h) return '';
    return h.connected ? 'up' : 'offline';
  }

  function seen(iso?: string): string {
    if (!iso) return 'never connected';
    const s = (Date.now() - new Date(iso).getTime()) / 1000;
    if (s < 3600) return `seen ${Math.max(1, Math.round(s / 60))} min ago`;
    if (s < 86400) return `seen ${Math.round(s / 3600)} h ago`;
    return `seen ${Math.round(s / 86400)} d ago`;
  }

  // close the mobile nav after navigating
  $effect(() => {
    app.route;
    navOpen = false;
    hostMenu = false;
  });

  async function signOut() {
    await api.logout().catch(() => {});
    app.user = null;
    onSignedOut();
  }
</script>

<div class="shell" class:nav-open={navOpen}>
  <aside class="side card" aria-label="Stacks">
    <div class="side-head">
      <a class="brand" href={link('')} aria-label="Moorage home">
        <span class="logo"><Icon name="anchor" /></span>
        <span class="word">Moorage</span>
      </a>
      {#if canEdit() && cur?.connected}
        <a class="btn btn-ghost btn-icon" href={link('new')} aria-label="New stack" title="New stack"><Icon name="plus" /></a>
      {/if}
    </div>

    <div class="hostsw">
      <button class="host-btn" onclick={() => (hostMenu = !hostMenu)} aria-haspopup="menu" aria-expanded={hostMenu}>
        <span class="dot {hostDot(cur)}"></span>
        <span class="hname">{cur?.name ?? (hostsLoaded && !app.hosts.length ? 'No hosts yet' : 'Host')}</span>
        {#if cur?.readOnly}<span class="ro" title="Read-only host"><Icon name="lock" /></span>{/if}
        <Icon name="selector" />
      </button>
      {#if hostMenu}
        <div class="menu card" role="menu">
          {#each app.hosts as h (h.id)}
            <button role="menuitemradio" aria-checked={h.id === app.host} class:on={h.id === app.host}
              onclick={() => { hostMenu = false; setHost(h.id); }}>
              <span class="dot {hostDot(h)}"></span>
              <span class="mname">{h.name}</span>
              <span class="mmeta">{h.connected ? (h.kind === 'local' ? 'this server' : h.readOnly ? 'read-only' : '') : seen(h.lastSeen)}</span>
            </button>
          {/each}
          {#if app.hosts.length}<hr />{/if}
          <a role="menuitem" href="#/hosts" onclick={() => (hostMenu = false)}><Icon name="server" /> {isAdmin() ? 'Manage hosts' : 'All hosts'}</a>
        </div>
      {/if}
    </div>

    <label class="search">
      <Icon name="search" />
      <span class="sr-only">Search stacks</span>
      <input bind:value={query} placeholder="Search stacks" spellcheck="false" />
    </label>

    <nav class="list">
      {#if stacks === null}
        {#each [1, 2, 3, 4] as i (i)}
          <div class="stack-item"><span class="skeleton" style="width:8px;height:8px;border-radius:50%"></span><span class="skeleton" style="height:10px;width:{60 + i * 12}px"></span></div>
        {/each}
      {:else if filtered.length === 0}
        <p class="meta empty">{query ? 'No stack matches.' : listError ? '' : 'No stacks yet.'}</p>
      {:else}
        {#each filtered as s (s.name)}
          <a class="stack-item" href={link('stacks/' + encodeURIComponent(s.name))} aria-current={selected === s.name ? 'page' : undefined}>
            <span class="dot {s.status}" title={statusLabel[s.status]}></span>
            <span class="name">{s.name}</span>
            {#if s.busy}<span class="busy" title="Operation running"></span>{/if}
            {#if !s.managed}<span class="ro" title="Read-only"><Icon name="lock" /></span>{/if}
          </a>
        {/each}
      {/if}
    </nav>

    <nav class="pages" aria-label="Monitoring">
      <a class="page-link" href="#/checks" aria-current={onChecks ? 'page' : undefined}>
        <Icon name="pulse" /> <span class="pname">Checks</span>
        {#if checksDown}<span class="cnt" title="Checks down">{checksDown}</span>{/if}
      </a>
      <a class="page-link" href="#/status-pages" aria-current={onStatusPages ? 'page' : undefined}>
        <Icon name="world" /> <span class="pname">Status pages</span>
      </a>
      <a class="page-link" href="#/maintenance" aria-current={onMaintenance ? 'page' : undefined}>
        <Icon name="tool" /> <span class="pname">Maintenance</span>
      </a>
      <a class="page-link" href={link('templates')}
        aria-current={['templates', 'template', 'templateSources'].includes(app.route.page) ? 'page' : undefined}>
        <Icon name="template" /> <span class="pname">Templates</span>
      </a>
      <a class="page-link" href={link('deps')} aria-current={app.route.page === 'deps' ? 'page' : undefined}>
        <Icon name="hierarchy" /> <span class="pname">Dependencies</span>
      </a>
      <a class="page-link" href="#/updates" aria-current={app.route.page === 'updates' ? 'page' : undefined}>
        <Icon name="cloudDown" /> <span class="pname">Updates</span>
      </a>
      {#if isAdmin()}
        <a class="page-link" href="#/notifications" aria-current={app.route.page === 'notifications' ? 'page' : undefined}>
          <Icon name="bell" /> <span class="pname">Notifications</span>
        </a>
        <a class="page-link" href="#/audit" aria-current={app.route.page === 'audit' ? 'page' : undefined}>
          <Icon name="clipboardList" /> <span class="pname">Audit log</span>
        </a>
      {/if}
    </nav>

    <div class="side-foot">
      {#if listError}<p class="warn"><Icon name="alert" /> {listError}</p>{/if}
      <div class="host">
        <span class="dot" class:up={host?.reachable} class:down={host && !host.reachable}></span>
        <span class="meta truncate">{engineLabel(host) || (cur?.connected === false ? 'Agent offline' : 'Docker')}{host?.stacksDir ? ` · ${host.stacksDir}` : ''}</span>
      </div>
      <div class="foot-row">
        <span class="meta num">{`${stacks?.length ?? 0} stacks · ${counts.up} up`}{#if counts.issues}<span class="issues">{` · ${counts.issues} need attention`}</span>{/if}</span>
      </div>
      <div class="foot-row">
        <span class="meta truncate">{app.user?.username}</span>
        <span class="foot-actions">
          <button class="btn btn-ghost btn-icon" onclick={toggleTheme} aria-label="Switch theme" title="Switch theme">
            <Icon name={app.theme === 'dark' ? 'sun' : 'moon'} />
          </button>
          <button class="btn btn-ghost btn-icon" onclick={signOut} aria-label="Sign out" title="Sign out"><Icon name="logout" /></button>
        </span>
      </div>
    </div>
  </aside>

  <main class="main">
    <button class="btn btn-ghost nav-toggle" onclick={() => (navOpen = !navOpen)}>
      <Icon name={navOpen ? 'x' : 'menu'} /> Stacks
    </button>
    {#if app.route.page === 'hosts'}
      <HostsView onChanged={loadHosts} />
    {:else if app.route.page === 'checks'}
      <ChecksView />
    {:else if app.route.page === 'check'}
      {#key app.route.id}<CheckDetail id={app.route.id} />{/key}
    {:else if app.route.page === 'checkForm'}
      {#key JSON.stringify(app.route)}<CheckForm id={app.route.id} host={app.route.host} stack={app.route.stack} />{/key}
    {:else if app.route.page === 'statusPages'}
      <StatusPagesView />
    {:else if app.route.page === 'statusPageForm'}
      {#key app.route.id}<StatusPageForm id={app.route.id} />{/key}
    {:else if app.route.page === 'maintenance'}
      <MaintenanceView />
    {:else if app.route.page === 'maintenanceForm'}
      {#key app.route.id}<MaintenanceForm id={app.route.id} />{/key}
    {:else if app.route.page === 'updates'}
      <UpdatesView />
    {:else if app.route.page === 'notifications'}
      <NotificationsView />
    {:else if app.route.page === 'audit' && isAdmin()}
      <AuditView />
    {:else if app.route.page === 'stack'}
      {#key app.route.name}
        <StackDetail name={app.route.name} onChanged={refresh} />
      {/key}
    {:else if app.route.page === 'templates'}
      <TemplatesView />
    {:else if app.route.page === 'template'}
      {#key app.route.source + '/' + app.route.id + '@' + app.host}
        <TemplateDeploy source={app.route.source} id={app.route.id} onCreated={refresh} />
      {/key}
    {:else if app.route.page === 'templateSources' && isAdmin()}
      <TemplateSources />
    {:else if app.route.page === 'deps'}
      <DepsView />
    {:else if app.route.page === 'new'}
      <NewStack onCreated={(n) => { refresh(); go('stacks/' + encodeURIComponent(n)); }} />
    {:else if hostsLoaded && !app.hosts.length}
      <section class="card card-pad first">
        <h1 class="ftitle">Connect your first host</h1>
        <p class="meta">This Moorage runs as a hub without its own Docker. Pair a Docker host by running the Moorage agent on it.</p>
        {#if isAdmin()}<a class="btn btn-primary" href="#/hosts"><Icon name="plus" /> Add host</a>{/if}
      </section>
    {:else}
      <Home {stacks} {host} />
    {/if}
  </main>
</div>

<style>
  .shell {
    display: grid;
    grid-template-columns: var(--sidebar) minmax(0, 1fr);
    gap: 12px;
    padding: 12px;
    min-height: 100vh;
    max-width: 1464px;
    margin: 0 auto;
  }
  .side {
    position: sticky;
    top: 12px;
    height: calc(100vh - 24px);
    display: flex;
    flex-direction: column;
    padding: 8px;
  }
  .side-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 4px 4px 10px 8px;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--foam);
    text-decoration: none;
  }
  .logo {
    width: 22px;
    height: 22px;
    border-radius: 7px;
    background: var(--buoy);
    color: var(--buoy-ink);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 14px;
  }
  .word {
    font-weight: 600;
    font-size: 15px;
  }
  .search {
    display: flex;
    align-items: center;
    gap: 8px;
    height: 32px;
    padding: 0 10px;
    margin-bottom: 8px;
    border-radius: var(--r-md);
    background: var(--well);
    color: var(--murk);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .search:focus-within {
    box-shadow:
      0 0 0 2px var(--buoy),
      0 0 0 5px var(--buoy-wash);
  }
  .search input {
    flex: 1;
    min-width: 0;
    border: 0;
    background: transparent;
    color: var(--foam);
    font: 400 13px var(--font);
    outline: none;
  }
  .search input::placeholder {
    color: var(--haze);
  }
  .list {
    flex: 1;
    overflow-y: auto;
    display: grid;
    align-content: start;
    gap: 2px;
  }
  .stack-item {
    display: flex;
    align-items: center;
    gap: 10px;
    height: 38px;
    padding: 0 10px;
    border-radius: var(--r-md);
    color: var(--mist);
    text-decoration: none;
    font-size: 14px;
  }
  .stack-item:hover {
    background: var(--rope);
    text-decoration: none;
    color: var(--foam);
  }
  .stack-item[aria-current='page'] {
    background: var(--buoy-wash);
    color: var(--foam);
    font-weight: 500;
  }
  .name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ro {
    color: var(--murk);
    font-size: 13px;
    display: inline-flex;
  }
  .busy {
    width: 12px;
    height: 12px;
    border-radius: 50%;
    border: 2px solid var(--buoy-wash);
    border-top-color: var(--buoy);
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .empty {
    padding: 8px 10px;
  }
  .pages {
    display: grid;
    gap: 2px;
    padding: 8px 0;
    border-top: 1px solid var(--rope);
  }
  .page-link {
    display: flex;
    align-items: center;
    gap: 10px;
    height: 34px;
    padding: 0 10px;
    border-radius: var(--r-md);
    color: var(--mist);
    text-decoration: none;
    font-size: 13px;
  }
  .page-link:hover {
    background: var(--rope);
    color: var(--foam);
    text-decoration: none;
  }
  .page-link[aria-current='page'] {
    background: var(--buoy-wash);
    color: var(--foam);
    font-weight: 500;
  }
  .pname {
    flex: 1;
  }
  .cnt {
    min-width: 20px;
    height: 20px;
    padding: 0 6px;
    border-radius: 999px;
    background: var(--adrift-wash);
    color: var(--adrift-text);
    font-size: 11px;
    font-weight: 600;
    display: inline-flex;
    align-items: center;
    justify-content: center;
  }
  .side-foot {
    display: grid;
    gap: 6px;
    padding: 10px 6px 2px 10px;
    border-top: 1px solid var(--rope);
  }
  .host {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .host .dot {
    width: 6px;
    height: 6px;
  }
  .hostsw {
    position: relative;
    margin-bottom: 8px;
  }
  .host-btn {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    height: 34px;
    padding: 0 8px 0 10px;
    border: 0;
    border-radius: var(--r-md);
    background: var(--rope);
    color: var(--foam);
    font: 500 13px var(--font);
    cursor: pointer;
    text-align: left;
  }
  .host-btn:hover {
    background: var(--rope-strong);
  }
  .host-btn:focus-visible {
    outline: 2px solid var(--buoy);
  }
  .hname {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .host-btn :global(.icon:last-child) {
    color: var(--haze);
  }
  .dot.offline {
    background: var(--becalmed);
  }
  .menu {
    position: absolute;
    left: 0;
    right: 0;
    top: 38px;
    z-index: 20;
    padding: 4px;
    background: var(--pier-raised);
    box-shadow: var(--lift-raised);
    display: grid;
    gap: 2px;
    max-height: 60vh;
    overflow-y: auto;
  }
  .menu button,
  .menu a {
    display: flex;
    align-items: center;
    gap: 10px;
    min-height: 34px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--mist);
    font: 400 13px var(--font);
    text-align: left;
    text-decoration: none;
    cursor: pointer;
  }
  .menu button:hover,
  .menu a:hover {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .menu .on {
    background: var(--buoy-wash);
    color: var(--foam);
  }
  .mname {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .mmeta {
    font-size: 11px;
    color: var(--haze);
    white-space: nowrap;
  }
  .menu hr {
    border: 0;
    border-top: 1px solid var(--rope);
    margin: 2px 0;
  }
  .first {
    display: grid;
    gap: 10px;
    justify-items: start;
    max-width: 560px;
  }
  .ftitle {
    font-size: 20px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .foot-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    min-height: 32px;
  }
  .foot-row + .foot-row {
    min-height: 0;
  }
  .foot-row:first-of-type {
    min-height: 0;
  }
  .foot-actions {
    display: flex;
  }
  .issues {
    color: var(--adrift-text);
  }
  .warn {
    display: flex;
    gap: 6px;
    font-size: 12px;
    color: var(--signal-text);
  }
  .truncate {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .main {
    min-width: 0;
  }
  .nav-toggle {
    display: none;
  }

  @media (max-width: 767px) {
    .shell {
      grid-template-columns: minmax(0, 1fr);
      padding: 8px;
    }
    .side {
      display: none;
      position: static;
      height: auto;
      min-height: calc(100vh - 16px);
    }
    .nav-open .side {
      display: flex;
    }
    .nav-open .main > :global(:not(.nav-toggle)) {
      display: none;
    }
    .nav-open .main {
      order: -1;
    }
    .nav-toggle {
      display: inline-flex;
      margin-bottom: 8px;
    }
  }
</style>
