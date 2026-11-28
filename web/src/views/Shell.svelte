<script lang="ts">
  import { dismiss, menuKeys } from '../lib/dismiss';
  import { untrack } from 'svelte';
  import { api, engineLabel, statusLabel, type HostEntry, type HostInfo, type StackSummary } from '../lib/api';
  import { app, canEdit, currentHost, go, isAdmin, link, monitoringBadge, setHost, toggleTheme, updatesBadge, type NavBadge } from '../lib/state.svelte';
  import Icon, { type IconName } from '../lib/Icon.svelte';
  import StackDetail from './StackDetail.svelte';
  import NewStack from './NewStack.svelte';
  import Home from './Home.svelte';
  import SettingsView from './SettingsView.svelte';
  import MonitoringView from './MonitoringView.svelte';
  import CheckDetail from './CheckDetail.svelte';
  import CheckForm from './CheckForm.svelte';
  import StatusPageForm from './StatusPageForm.svelte';
  import UpdatesView from './UpdatesView.svelte';
  import MaintenanceForm from './MaintenanceForm.svelte';
  import DepsView from './DepsView.svelte';
  import LogsView from './LogsView.svelte';
  import TemplatesView from './TemplatesView.svelte';
  import TemplateDeploy from './TemplateDeploy.svelte';
  import AccountView from './AccountView.svelte';
  import ModeOnboarding from './ModeOnboarding.svelte';
  import CommandPalette from '../lib/CommandPalette.svelte';
  import AssistantPanel from '../lib/AssistantPanel.svelte';
  import AssistantView from './AssistantView.svelte';
  import { chat, open as openAssistant, ask as askAssistant } from '../lib/assistantStore.svelte';
  import { noteToast } from '../lib/pageNotices.svelte';

  let { onSignedOut }: { onSignedOut: () => void } = $props();
  let paletteOpen = $state(false);
  // M18H: rolled-back Apply & restart runs no admin dismissed yet
  let selfNotices = $state<{ hostId: string; hostName: string; stack: string; at: string; message: string; reason?: string }[]>([]);
  let selfLog = $state<string | null>(null);
  async function loadSelfNotices() {
    if (app.user?.role !== 'admin') return;
    try {
      selfNotices = await api.selfApplyNotices();
    } catch {
      selfNotices = [];
    }
  }
  loadSelfNotices();
  async function ackSelf(n: { hostId: string }) {
    try {
      await api.ackSelfApply(n.hostId);
      selfNotices = selfNotices.filter((x) => x !== n);
    } catch {
      /* keep the banner */
    }
  }
  async function showSelfLog(n: { hostId: string }) {
    try {
      selfLog = (await api.selfApplyLog(n.hostId)).log;
    } catch (e) {
      selfLog = (e as Error).message;
    }
  }
  // AI assistant (M18G, M19E): shown only when the add-on is on and configured. The
  // state is re-read on window focus and after its settings are saved (event), so Ctrl+J
  // works without a reload.
  let assistantOn = $state(false);
  async function loadAssistant() {
    try {
      const st = await api.assistantStatus();
      assistantOn = st.enabled && st.configured;
      chat.info = { ...(chat.info ?? {}), ...st };
    } catch {
      assistantOn = false;
    }
  }
  loadAssistant();
  $effect(() => {
    const h = () => void loadAssistant();
    window.addEventListener('moorage-assistant-changed', h);
    return () => window.removeEventListener('moorage-assistant-changed', h);
  });
  function assistantKey(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && !e.shiftKey && e.key.toLowerCase() === 'j' && assistantOn) {
      e.preventDefault();
      if (chat.open) chat.open = false;
      else openAssistant();
    }
  }
  // ?ask=explain on an incident or check opens the panel with a question (M19I)
  $effect(() => {
    void app.route;
    const q = new URLSearchParams(location.hash.split('?')[1] ?? '');
    if (q.get('ask') === 'explain' && assistantOn) {
      const r = app.route as { page: string; id?: number; name?: string };
      if (r.page === 'check' && r.id) askAssistant(`Explain this alert of check #${r.id}: what failed, why, and how do I fix it?`);
      else if (r.page === 'stack' && r.name)
        askAssistant(`Explain the latest alert of the stack ${r.name}: what failed, why, what changed before it, and how do I fix it?`);
      // once: the question is filled in, nothing is sent; reloading doesn't ask again
      history.replaceState(null, '', location.pathname + location.search + location.hash.split('?')[0]);
    }
  });

  let stacks = $state<StackSummary[] | null>(null);
  let listError = $state('');
  let host = $state<HostInfo | null>(null);
  let query = $state('');
  let navOpen = $state(false);
  let hostMenu = $state(false);
  let hostsLoaded = $state(false);

  const cur = $derived(currentHost());
  let availUpdateVer = $state('');
  let dismissedUpdateVer = $state('');
  try {
    dismissedUpdateVer = localStorage.getItem('moorage-dismissed-update') || '';
  } catch {
    /* ignore */
  }
  const onMonitoring = $derived(
    ['monitoring', 'check', 'checkForm', 'statusPageForm', 'maintenanceForm'].includes(app.route.page),
  );

  // Pages live in the top bar (the sidebar is for hosts and stacks). Links that don't fit
  // the bar's width fold into "More" from the end (priority+), measured, not by breakpoint.
  type PageLink = { href: string; label: string; icon: IconName; current: boolean; badge?: NavBadge };
  const pages = $derived<PageLink[]>([
    { href: link('logs'), label: 'Logs', icon: 'logs', current: app.route.page === 'logs' },
    { href: '#/monitoring/checks', label: 'Monitoring', icon: 'pulse', current: onMonitoring, badge: monitoringBadge() },
    { href: link('templates'), label: 'Templates', icon: 'template', current: ['templates', 'template'].includes(app.route.page) },
    { href: link('deps'), label: 'Dependencies', icon: 'hierarchy', current: app.route.page === 'deps' },
    { href: '#/updates', label: 'Image updates', icon: 'cloudDown', current: app.route.page === 'updates', badge: updatesBadge() },
    {
      // with MCP drafts waiting, Settings leads to them (M20G)
      href: app.nav?.mcpDrafts ? '#/settings/addons?open=mcp-server' : '#/settings/hosts',
      label: 'Settings',
      icon: 'settings',
      current: app.route.page === 'settings',
      badge: app.nav?.mcpDrafts ? { n: app.nav.mcpDrafts, tone: 'blue', title: `${app.nav.mcpDrafts} MCP drafts awaiting approval` } : undefined,
    },
  ]);
  let barWidth = $state(0);
  let measureEl = $state<HTMLElement | null>(null);
  let linkWidths = $state<number[]>([]);
  let moreMenu = $state(false);
  let userMenu = $state(false);
  const MORE_W = 96; // room for the "More" button
  const GAP = 2;
  // Re-measure whenever the set of links (labels, badges) changes.
  $effect(() => {
    void pages.map((p) => p.label + (p.badge ? p.badge.n + p.badge.tone : '')).join();
    const el = measureEl;
    if (!el) return;
    queueMicrotask(() => {
      linkWidths = Array.from(el.children).map((c) => (c as HTMLElement).offsetWidth);
    });
  });
  const visibleCount = $derived.by(() => {
    if (!barWidth || linkWidths.length !== pages.length) return pages.length;
    const total = linkWidths.reduce((a, w) => a + w + GAP, 0);
    if (total <= barWidth) return pages.length;
    let used = MORE_W;
    let n = 0;
    for (const w of linkWidths) {
      if (used + w + GAP > barWidth) break;
      used += w + GAP;
      n++;
    }
    return n;
  });
  const shown = $derived(pages.slice(0, visibleCount));
  const overflow = $derived(pages.slice(visibleCount));
  const overflowCurrent = $derived(overflow.find((p) => p.current));

  type Toast = { id: number; title: string; body: string };
  let toasts = $state<Toast[]>([]);
  let prevDownIds: Set<number> | null = null;
  let toastSeq = 0;

  function pushToast(title: string, body: string) {
    const id = ++toastSeq;
    toasts = [...toasts.slice(-3), { id, title, body }];
    // M19I: the assistant knows the alerts of the last 5 minutes (check messages quote
    // outside services, so they count as outside content)
    noteToast({ id: 'toast:' + id, severity: 'error', title, text: body, foreign: !!body });
    setTimeout(() => {
      toasts = toasts.filter((t) => t.id !== id);
    }, 8000);
    try {
      if (localStorage.getItem('moorage-browser-alerts') === '1' && 'Notification' in window && Notification.permission === 'granted') {
        new Notification(title, { body });
      }
    } catch {
      /* ignore */
    }
  }

  // One light summary call feeds every badge (it never touches registries or hosts). On a
  // network error or sign-out the badges keep their last value instead of jumping.
  async function loadCheckCount() {
    try {
      const nav = await api.navSummary();
      app.nav = nav;
      const curIds = new Set(nav.failing.map((c) => c.id));
      if (prevDownIds !== null) {
        for (const c of nav.failing) {
          if (!prevDownIds.has(c.id)) {
            pushToast(`${c.status === 'degraded' ? 'Check partially down' : 'Check down'}: ${c.name}`, c.msg || '');
          }
        }
      }
      prevDownIds = curIds;
    } catch {
      /* not important */
    }
  }
  async function loadAvailUpdate() {
    if (!isAdmin()) {
      availUpdateVer = '';
      return;
    }
    try {
      const ov = await api.systemUpdates();
      const hasUpd = ov.release && ov.nodes.some((n) => n.updateAvailable);
      availUpdateVer = hasUpd && ov.release ? ov.release.version : '';
    } catch {
      /* optional */
    }
  }
  function dismissUpdateBanner() {
    if (!availUpdateVer) return;
    dismissedUpdateVer = availUpdateVer;
    try {
      localStorage.setItem('moorage-dismissed-update', availUpdateVer);
    } catch {
      /* ignore */
    }
  }
  $effect(() => {
    untrack(loadCheckCount);
    const t = setInterval(() => !document.hidden && loadCheckCount(), 15000);
    return () => clearInterval(t);
  });
  const adminRole = $derived(isAdmin());
  $effect(() => {
    void adminRole;
    untrack(() => {
      void loadAvailUpdate();
    });
    const t = setInterval(() => {
      if (!document.hidden && adminRole) void loadAvailUpdate();
    }, 60000);
    return () => clearInterval(t);
  });

  // Hub self-update: "waiting" while the job runs other steps (agents first in "Update
  // all"), so the page stays usable; the overlay shows from "restarting" on, and turns
  // into "failed" with the job's message after RESTART_TIMEOUT_MS.
  const RESTART_TIMEOUT_MS = 4 * 60 * 1000;
  const restartingActive = $derived(app.restarting !== null);
  let restartPhase = $state<'waiting' | 'restarting' | 'failed'>('waiting');
  let restartStatus = $state('');
  async function hubJob(id: number) {
    const ov = await api.systemUpdates();
    const job = ov.nodes.find((n) => n.id === 'hub')?.job;
    return job && job.id === id ? job : null;
  }
  $effect(() => {
    if (!restartingActive) return;
    const target = untrack(() => app.restarting);
    if (!target) return;
    let stopped = false;
    let busy = false;
    let sawDisconnect = false;
    let restartingSince = 0;
    restartPhase = 'waiting';
    restartStatus = '';
    const enterRestarting = () => {
      if (restartPhase !== 'waiting') return;
      restartPhase = 'restarting';
      restartingSince = Date.now();
    };
    const done = () => {
      stopped = true;
      app.restarting = null;
      location.reload();
    };
    const fail = (msg: string) => {
      restartPhase = 'failed';
      restartStatus = msg;
    };
    const check = async () => {
      if (restartPhase === 'waiting') {
        try {
          const job = await hubJob(target.jobId);
          if (stopped) return;
          if (!job || job.status === 'running') return;
          if (job.status === 'restarting') {
            enterRestarting();
            return;
          }
          // Settled before the hub restarted (e.g. an agent failed and the hub was
          // skipped): the updates page shows the result, no overlay needed.
          stopped = true;
          app.restarting = null;
        } catch {
          // The hub went away before we saw "restarting": it is restarting.
          sawDisconnect = true;
          enterRestarting();
        }
        return;
      }
      if (restartPhase !== 'restarting') return;
      if (Date.now() - restartingSince > RESTART_TIMEOUT_MS) {
        try {
          const job = await hubJob(target.jobId);
          if (stopped) return;
          fail(
            job && job.status !== 'restarting' && job.message
              ? job.message
              : 'Moorage has not come back within 4 minutes. Check the server and its logs.',
          );
        } catch {
          if (!stopped) fail('Moorage has not come back within 4 minutes. Check the server and its logs.');
        }
        return;
      }
      try {
        const s = await api.session();
        if (stopped) return;
        // Boot ID changes on every process start — catches sub-2s container restarts even if sawDisconnect never fired.
        if (s.bootId && app.bootId && s.bootId !== app.bootId) return done();
        if (s.version && s.version !== target.fromVersion) return done();
        // Same version answering: either not stopped yet, or rolled back. The job tells.
        const job = await hubJob(target.jobId);
        if (stopped) return;
        if (job && job.status !== 'restarting' && job.status !== 'running') {
          if (sawDisconnect || job.status === 'done' || job.status === 'rolled-back') return done();
          fail(job.message || 'The hub update failed.');
        }
      } catch {
        sawDisconnect = true;
        restartStatus = 'Waiting for Moorage to come back online…';
      }
    };
    const t = setInterval(() => {
      if (stopped || busy || restartPhase === 'failed') return;
      busy = true;
      void check().finally(() => (busy = false));
    }, 2000);
    return () => {
      stopped = true;
      clearInterval(t);
    };
  });
  function closeRestart() {
    app.restarting = null;
  }

  const filtered = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (!q) return stacks ?? [];
    return (stacks ?? []).filter((s) => {
      const hay = [s.name, s.displayName, s.coolifyProject, ...(s.urls ?? [])].filter(Boolean).join(' ').toLowerCase();
      return hay.includes(q);
    });
  });

  const hasCoolifyGroups = $derived(filtered.some((s) => !!s.coolifyUuid || !!s.coolifyProject));
  let collapsedGroups = $state<Record<string, boolean>>({});
  try {
    const raw = localStorage.getItem('moorage-sidebar-groups');
    if (raw) collapsedGroups = JSON.parse(raw) as Record<string, boolean>;
  } catch {
    /* ignore */
  }
  function toggleGroup(key: string) {
    collapsedGroups = { ...collapsedGroups, [key]: !collapsedGroups[key] };
    try {
      localStorage.setItem('moorage-sidebar-groups', JSON.stringify(collapsedGroups));
    } catch {
      /* ignore */
    }
  }

  const groupedStacks = $derived.by(() => {
    if (!hasCoolifyGroups) return [] as { key: string; label: string; items: StackSummary[] }[];
    const managed: StackSummary[] = [];
    const coolifyByProj = new Map<string, StackSummary[]>();
    const coolifySys: StackSummary[] = [];
    const other: StackSummary[] = [];
    for (const s of filtered) {
      if (s.managed) {
        managed.push(s);
      } else if (s.coolifyUuid || s.coolifyProject) {
        const proj = s.coolifyProject || 'Coolify';
        const list = coolifyByProj.get(proj) ?? [];
        list.push(s);
        coolifyByProj.set(proj, list);
      } else if (s.name === 'coolify' || s.name === 'coolify-proxy') {
        coolifySys.push(s);
      } else {
        other.push(s);
      }
    }
    const groups: { key: string; label: string; items: StackSummary[] }[] = [];
    if (managed.length) groups.push({ key: 'managed', label: 'Managed', items: managed });
    for (const proj of [...coolifyByProj.keys()].sort((a, b) => a.localeCompare(b))) {
      const items = [...(coolifyByProj.get(proj) ?? [])].sort((a, b) =>
        (a.displayName || a.name).localeCompare(b.displayName || b.name),
      );
      groups.push({ key: `coolify:${proj}`, label: `Coolify · ${proj}`, items });
    }
    if (coolifySys.length) groups.push({ key: 'coolify-system', label: 'Coolify system', items: coolifySys });
    if (other.length) groups.push({ key: 'other', label: 'Other', items: other });
    return groups;
  });
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
      if (r.hosts.length && !r.hosts.some((h) => h.id === app.host)) setHost(r.hosts[0].id, app.route.page !== 'settings');
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
    moreMenu = false;
    userMenu = false;
  });

  async function signOut() {
    const r = await api.logout().catch(() => undefined);
    app.user = null;
    // An SSO user continues to the identity provider's sign-out when it has one.
    if (r?.redirect) {
      location.href = r.redirect;
      return;
    }
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

    <div class="hostsw" use:dismiss={() => (hostMenu = false)}>
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
          <a role="menuitem" href="#/settings/hosts" onclick={() => (hostMenu = false)}><Icon name="server" /> {isAdmin() ? 'Manage hosts' : 'All hosts'}</a>
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
      {:else if hasCoolifyGroups}
        {#each groupedStacks as grp (grp.key)}
          {@const collapsed = !!collapsedGroups[grp.key] && !query.trim()}
          <div class="sgrp">
            <button type="button" class="sgrp-btn" onclick={() => toggleGroup(grp.key)} aria-expanded={!collapsed}>
              <Icon name={collapsed ? 'chevronDown' : 'chevronDown'} />
              <span class="sgrp-lbl">{grp.label}</span>
              <span class="meta num">{grp.items.length}</span>
            </button>
            {#if !collapsed}
              {#each grp.items as s (s.name)}
                <a class="stack-item" href={link('stacks/' + encodeURIComponent(s.name))} aria-current={selected === s.name ? 'page' : undefined}>
                  <span class="dot {s.status}" title={statusLabel[s.status]}></span>
                  <span class="name">{s.displayName || s.name}</span>
                  {#if s.busy || s.coolifyDeploying}<span class="busy" title="Deploying"></span>{/if}
                  {#if !s.managed && !s.coolifyUuid}<span class="ro" title="Read-only"><Icon name="lock" /></span>{/if}
                </a>
              {/each}
            {/if}
          </div>
        {/each}
      {:else}
        {#each filtered as s (s.name)}
          <a class="stack-item" href={link('stacks/' + encodeURIComponent(s.name))} aria-current={selected === s.name ? 'page' : undefined}>
            <span class="dot {s.status}" title={statusLabel[s.status]}></span>
            <span class="name">{s.displayName || s.name}</span>
            {#if s.busy || s.coolifyDeploying}<span class="busy" title="Operation running"></span>{/if}
            {#if !s.managed && !s.coolifyUuid}<span class="ro" title="Read-only"><Icon name="lock" /></span>{/if}
          </a>
        {/each}
      {/if}
    </nav>

    <!-- Phones only: the top bar has no room for page links, so the drawer lists them. -->
    <nav class="pages" aria-label="Pages">
      {#each pages as p (p.href)}
        <a class="page-link" href={p.href} aria-current={p.current ? 'page' : undefined}>
          <Icon name={p.icon} /> <span class="pname">{p.label}</span>
          {#if p.badge}<span class="cnt" class:blue={p.badge.tone === 'blue'} title={p.badge.title}>{p.badge.n}</span>{/if}
        </a>
      {/each}
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
    </div>
  </aside>

  <main class="main">
    <header class="topbar card">
      <button class="btn btn-ghost nav-toggle" onclick={() => (navOpen = !navOpen)} aria-expanded={navOpen}>
        <Icon name={navOpen ? 'x' : 'menu'} /> Menu
      </button>
      <nav class="tabs" aria-label="Pages" bind:clientWidth={barWidth}>
        {#each shown as p (p.href)}
          <a class="tab" href={p.href} aria-current={p.current ? 'page' : undefined}>
            <Icon name={p.icon} /> {p.label}
            {#if p.badge}<span class="cnt" class:blue={p.badge.tone === 'blue'} title={p.badge.title}>{p.badge.n}</span>{/if}
          </a>
        {/each}
        {#if overflow.length}
          <div class="pop" use:dismiss={() => (moreMenu = false)}>
            <button class="tab more-btn" class:cur={!!overflowCurrent} onclick={() => (moreMenu = !moreMenu)}
              aria-haspopup="menu" aria-expanded={moreMenu}>
              {overflowCurrent ? overflowCurrent.label : 'More'} <Icon name="chevronDown" />
            </button>
            {#if moreMenu}
              <div class="menu card end" role="menu" use:menuKeys>
                {#each overflow as p (p.href)}
                  <a role="menuitem" href={p.href} class:on={p.current} onclick={() => (moreMenu = false)}>
                    <Icon name={p.icon} /> <span class="mname">{p.label}</span>
                    {#if p.badge}<span class="cnt" class:blue={p.badge.tone === 'blue'} title={p.badge.title}>{p.badge.n}</span>{/if}
                  </a>
                {/each}
              </div>
            {/if}
          </div>
        {/if}
      </nav>
      <!-- Hidden copy used only to measure each link's natural width. -->
      <div class="tabs measure" aria-hidden="true" bind:this={measureEl}>
        {#each pages as p (p.href)}
          <span class="tab"><Icon name={p.icon} /> {p.label}{#if p.badge}<span class="cnt">{p.badge.n}</span>{/if}</span>
        {/each}
      </div>
      {#if assistantOn}
        <button type="button" id="assistant-toggle" class="tab pal-btn" onclick={() => (chat.open ? (chat.open = false) : openAssistant())} title="Assistant (Ctrl+J)" aria-label="Assistant" aria-expanded={chat.open}>
          <Icon name="wand" />
        </button>
      {/if}
      <button type="button" class="tab pal-btn" onclick={() => (paletteOpen = true)} title="Quick search (Ctrl+K)" aria-label="Quick search">
        <Icon name="search" /> <kbd class="kbd-hint">Ctrl K</kbd>
      </button>
      <div class="pop user" use:dismiss={() => (userMenu = false)}>
        <button class="tab user-btn" class:cur={app.route.page === 'account'} onclick={() => (userMenu = !userMenu)}
          aria-haspopup="menu" aria-expanded={userMenu} title="Your account">
          <Icon name="userCircle" /> <span class="uname">{app.user?.username}</span> <Icon name="chevronDown" />
        </button>
        {#if userMenu}
          <div class="menu card end" role="menu" use:menuKeys>
            {#if !app.demo}<a role="menuitem" href="#/account" onclick={() => (userMenu = false)}><Icon name="userCircle" /> Account</a>{/if}
            <button role="menuitem" onclick={() => { toggleTheme(); userMenu = false; }}>
              <Icon name={app.theme === 'dark' ? 'sun' : 'moon'} /> {app.theme === 'dark' ? 'Light theme' : 'Dark theme'}
            </button>
            {#if !app.demo}
              <hr />
              <button role="menuitem" onclick={signOut}><Icon name="logout" /> Sign out</button>
            {/if}
          </div>
        {/if}
      </div>
    </header>
    {#each selfNotices as n (n.hostId + n.stack)}
      <div class="card self-banner" role="alert">
        <Icon name="rollback" />
        <span><strong>Apply &amp; restart was rolled back</strong> on {n.hostName} ({new Date(n.at).toLocaleString()}) – {n.reason || n.message}. The previous configuration is running again.</span>
        <span class="self-acts">
          <button type="button" class="btn btn-sm" onclick={() => showSelfLog(n)}>Show log</button>
          <a class="btn btn-sm" href={`#/h/${encodeURIComponent(n.hostId)}/stacks/${encodeURIComponent(n.stack)}`}>Open stack</a>
          <button type="button" class="btn btn-sm btn-primary" onclick={() => ackSelf(n)}>Acknowledge</button>
        </span>
      </div>
    {/each}
    {#if selfLog !== null}
      <div class="card self-log" role="dialog" aria-label="Apply & restart log">
        <pre class="mono">{selfLog}</pre>
        <button type="button" class="btn btn-sm" onclick={() => (selfLog = null)}>Close</button>
      </div>
    {/if}
    {#if app.demo}
      <div class="card demo-banner" role="status">
        <Icon name="eye" />
        <span><strong>Read-only demo.</strong> Look around freely: the stacks, logs and checks are sample data, and every change is turned off.</span>
      </div>
    {/if}
    {#if adminRole && availUpdateVer && dismissedUpdateVer !== availUpdateVer}
      <div class="card upd-banner" role="status">
        <span class="upd-msg">
          <Icon name="cloudDown" />
          <span>Moorage <strong class="mono">{availUpdateVer}</strong> is available.</span>
        </span>
        <span class="upd-actions">
          <a class="btn btn-ghost" href="#/settings/hosts">View updates</a>
          <button type="button" class="btn btn-ghost btn-icon" aria-label="Dismiss update notice" onclick={dismissUpdateBanner}>
            <Icon name="x" />
          </button>
        </span>
      </div>
    {/if}
    {#if app.route.page === 'settings'}
      <SettingsView tab={app.route.tab} onHostsChanged={loadHosts} />
    {:else if app.route.page === 'monitoring'}
      <MonitoringView tab={app.route.tab} />
    {:else if app.route.page === 'check'}
      {#key app.route.id}<CheckDetail id={app.route.id} />{/key}
    {:else if app.route.page === 'checkForm'}
      {#key JSON.stringify(app.route)}<CheckForm id={app.route.id} host={app.route.host} stack={app.route.stack} />{/key}
    {:else if app.route.page === 'statusPageForm'}
      {#key app.route.id}<StatusPageForm id={app.route.id} />{/key}
    {:else if app.route.page === 'maintenanceForm'}
      {#key app.route.id}<MaintenanceForm id={app.route.id} />{/key}
    {:else if app.route.page === 'updates'}
      <UpdatesView />
    {:else if app.route.page === 'account'}
      <AccountView />
    {:else if app.route.page === 'assistant'}
      <AssistantView />
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
    {:else if app.route.page === 'logs'}
      <LogsView />
    {:else if app.route.page === 'deps'}
      <DepsView />
    {:else if app.route.page === 'new'}
      <NewStack onCreated={(n) => { refresh(); go('stacks/' + encodeURIComponent(n)); }} />
    {:else if hostsLoaded && !app.hosts.length}
      <section class="card card-pad first">
        <h1 class="ftitle">Connect your first host</h1>
        <p class="meta">This Moorage runs as a hub without its own Docker. Pair a Docker host by running the Moorage agent on it.</p>
        {#if isAdmin()}<a class="btn btn-primary" href="#/settings/hosts"><Icon name="plus" /> Add host</a>{/if}
      </section>
    {:else}
      <Home {stacks} {host} />
    {/if}
  </main>
</div>

<CommandPalette bind:open={paletteOpen} />
<AssistantPanel />
<svelte:window onkeydown={assistantKey} onfocus={loadAssistant} />

{#if app.user && app.user.preferences && !app.user.preferences.onboardingDone}
  <ModeOnboarding />
{/if}

{#if app.restarting && restartPhase !== 'waiting'}
  <div class="restart-overlay" role="dialog" aria-modal="true" aria-labelledby="restart-title">
    <div class="card card-pad restart-box">
      <div class="restart-head">
        {#if restartPhase === 'restarting'}<span class="busy"></span>{/if}
        <h2 id="restart-title">
          {restartPhase === 'failed' ? 'Moorage did not restart' : 'Moorage is restarting, reconnecting…'}
        </h2>
      </div>
      <p class="meta">{app.restarting.label}</p>
      {#if restartStatus}
        <p class={restartPhase === 'failed' ? 'error-text' : 'meta'} role={restartPhase === 'failed' ? 'alert' : undefined}>
          {restartStatus}
        </p>
      {/if}
      {#if restartPhase === 'restarting'}
        <p class="meta">This page reloads automatically once Moorage answers again.</p>
      {/if}
      <div class="restart-actions">
        <button type="button" class="btn" onclick={closeRestart}>Close</button>
      </div>
    </div>
  </div>
{/if}

{#if toasts.length > 0}
  <div class="toasts" aria-live="polite">
    {#each toasts as t (t.id)}
      <div class="toast card">
        <Icon name="alert" />
        <div class="toast-body">
          <div class="toast-title">{t.title}</div>
          {#if t.body}<div class="meta">{t.body}</div>{/if}
        </div>
        <button type="button" class="btn btn-ghost btn-icon" aria-label="Dismiss" onclick={() => (toasts = toasts.filter((x) => x.id !== t.id))}>
          <Icon name="x" />
        </button>
      </div>
    {/each}
  </div>
{/if}

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
  .toasts {
    position: fixed;
    right: 16px;
    bottom: 16px;
    z-index: 60;
    display: grid;
    gap: 8px;
    width: min(360px, calc(100vw - 32px));
  }
  .toast {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    padding: 10px 12px;
    background: var(--pier-raised);
    box-shadow: 0 0 0 1px var(--rope-strong), inset 3px 0 0 var(--adrift);
  }
  .toast-body {
    flex: 1;
    min-width: 0;
  }
  .toast-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--foam);
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
  .sgrp {
    display: grid;
    gap: 1px;
    margin-bottom: 4px;
  }
  .sgrp-btn {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 4px 8px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--haze);
    font: 600 11px var(--font);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    cursor: pointer;
    text-align: left;
  }
  .sgrp-btn:hover {
    color: var(--foam);
    background: var(--rope);
  }
  .sgrp-lbl {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .empty {
    padding: 8px 10px;
  }
  .pages {
    display: none;
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
  .cnt.blue {
    background: var(--info-wash);
    color: var(--info-text);
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
  .foot-row:first-of-type {
    min-height: 0;
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
    /* M19E: room for the docked assistant */
    margin-right: var(--assistant-dock, 0px);
  }
  .nav-toggle {
    display: none;
  }
  .topbar {
    position: sticky;
    top: 12px;
    z-index: 15;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px;
    margin-bottom: 12px;
  }
  .tabs {
    flex: 1;
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 2px;
  }
  /* zero-size and clipped, so the measuring copy never widens the page */
  .tabs.measure {
    position: absolute;
    visibility: hidden;
    pointer-events: none;
    flex: none;
    left: 0;
    top: 0;
    width: 0;
    height: 0;
    overflow: hidden;
  }
  .tabs.measure > :global(*) {
    flex: none;
  }
  .tab {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 32px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--r-md);
    background: transparent;
    color: var(--mist);
    font: 400 13px var(--font);
    text-decoration: none;
    white-space: nowrap;
    cursor: pointer;
  }
  .tab:hover {
    background: var(--rope);
    color: var(--foam);
    text-decoration: none;
  }
  .tab:focus-visible {
    outline: 2px solid var(--buoy);
  }
  .tab[aria-current='page'],
  .tab.cur {
    background: var(--buoy-wash);
    color: var(--foam);
    font-weight: 500;
  }
  .pop {
    position: relative;
  }
  .pop.user {
    margin-left: auto;
  }
  .uname {
    max-width: 140px;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .menu.end {
    left: auto;
    right: 0;
    min-width: 200px;
  }
  .self-banner {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    padding: 10px 14px;
    margin-bottom: 12px;
    box-shadow: inset 0 0 0 1px var(--danger);
  }
  .self-acts {
    display: flex;
    gap: 6px;
    margin-left: auto;
    flex-wrap: wrap;
  }
  .self-log {
    padding: 10px;
    margin-bottom: 12px;
    display: grid;
    gap: 8px;
  }
  .self-log pre {
    margin: 0;
    max-height: 320px;
    overflow: auto;
    font-size: 12px;
    white-space: pre-wrap;
  }
  .demo-banner {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    margin-bottom: 12px;
    font-size: 13px;
    color: var(--foam);
    background: var(--info-wash);
    box-shadow: inset 0 0 0 1px var(--rope-strong);
  }
  .upd-banner {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
    padding: 8px 12px;
    margin-bottom: 12px;
    max-width: 880px;
    background: var(--buoy-wash);
    box-shadow: inset 0 0 0 1px var(--rope-strong);
  }
  .upd-msg {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    color: var(--foam);
  }
  .upd-actions {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .restart-overlay {
    position: fixed;
    inset: 0;
    z-index: 100;
    background: rgba(11, 20, 24, 0.78);
    display: grid;
    place-items: center;
    padding: 16px;
  }
  .restart-box {
    width: min(440px, 100%);
    display: grid;
    gap: 8px;
  }
  .restart-head {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .restart-head h2 {
    font-size: 16px;
    font-weight: 600;
  }
  .restart-actions {
    display: flex;
    justify-content: flex-end;
  }

  .pal-btn {
    border: 0;
    background: transparent;
    cursor: pointer;
    font: 500 13px var(--font);
    gap: 6px;
  }
  .kbd-hint {
    font: 500 11px var(--mono);
    color: var(--haze);
    padding: 1px 5px;
    border-radius: 5px;
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }

  @media (max-width: 767px) {
    .kbd-hint {
      display: none;
    }
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
    .nav-open .main > :global(:not(.topbar)) {
      display: none;
    }
    .pages {
      display: grid;
    }
    .tabs {
      display: none;
    }
    .topbar {
      top: 8px;
      margin-bottom: 8px;
    }
    .nav-open .main {
      order: -1;
    }
    .nav-toggle {
      display: inline-flex;
    }
  }
</style>
