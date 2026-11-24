<script lang="ts">
  import { api, type Check, type SisterLinks, type StackSummary } from './api';
  import { app, isAdmin, link, setHost } from './state.svelte';
  import Icon, { type IconName } from './Icon.svelte';

  let { open = $bindable(false) }: { open: boolean } = $props();

  interface PaletteItem {
    id: string;
    group: 'Stack' | 'Service' | 'Host' | 'Check' | 'Page';
    title: string;
    sub?: string;
    icon: IconName;
    dot?: string;
    run: () => void;
  }

  let query = $state('');
  let cursor = $state(0);
  let inputEl = $state<HTMLInputElement | null>(null);
  let allStacks = $state<{ hostId: string; hostName: string; stack: StackSummary }[]>([]);
  let checks = $state<Check[]>([]);
  let sister = $state<SisterLinks | null>(null);
  let loading = $state(false);

  async function loadAll() {
    loading = true;
    const hosts = app.hosts.filter((h) => h.connected);
    const [stackLists, checkList, sisterRes] = await Promise.all([
      Promise.all(
        hosts.map(async (h) => {
          try {
            const r = await api.stacks(h.id);
            return r.stacks.map((stack) => ({ hostId: h.id, hostName: h.name, stack }));
          } catch {
            return [];
          }
        }),
      ),
      api.checks().catch(() => [] as Check[]),
      api.sisterLinks().catch(() => null),
    ]);
    allStacks = stackLists.flat();
    checks = checkList;
    sister = sisterRes;
    loading = false;
  }

  $effect(() => {
    if (open) {
      query = '';
      cursor = 0;
      void loadAll();
      queueMicrotask(() => inputEl?.focus());
    }
  });

  function onGlobalKey(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      open = !open;
    } else if (open && e.key === 'Escape') {
      e.preventDefault();
      open = false;
    }
  }

  const pageItems = $derived<PaletteItem[]>([
    { id: 'pg:home', group: 'Page', title: 'Home', sub: 'Dashboard overview', icon: 'anchor', run: () => { location.hash = link(''); } },
    { id: 'pg:logs', group: 'Page', title: 'Logs', sub: 'Host-wide live & historical container logs', icon: 'logs', run: () => { location.hash = link('logs'); } },
    { id: 'pg:checks', group: 'Page', title: 'Monitoring · Checks', sub: 'Uptime checks', icon: 'pulse', run: () => { location.hash = '#/monitoring/checks'; } },
    { id: 'pg:status', group: 'Page', title: 'Monitoring · Status pages', sub: 'Public status pages', icon: 'world', run: () => { location.hash = '#/monitoring/status-pages'; } },
    { id: 'pg:maint', group: 'Page', title: 'Monitoring · Maintenance', sub: 'Planned maintenance windows', icon: 'tool', run: () => { location.hash = '#/monitoring/maintenance'; } },
    { id: 'pg:tpl', group: 'Page', title: 'Templates', sub: 'Deploy from catalog', icon: 'template', run: () => { location.hash = link('templates'); } },
    { id: 'pg:deps', group: 'Page', title: 'Dependencies', sub: 'Stack dependency graph', icon: 'hierarchy', run: () => { location.hash = link('deps'); } },
    { id: 'pg:upd', group: 'Page', title: 'Image updates', sub: 'Available container updates', icon: 'cloudDown', run: () => { location.hash = '#/updates'; } },
    { id: 'pg:hosts', group: 'Page', title: 'Settings · Hosts', sub: 'Manage Hub and agents', icon: 'server', run: () => { location.hash = '#/settings/hosts'; } },
    ...(isAdmin()
      ? [
          { id: 'pg:gen', group: 'Page' as const, title: 'Settings · General', sub: 'Hub URLs and locks', icon: 'settings' as const, run: () => { location.hash = '#/settings/general'; } },
          { id: 'pg:bk', group: 'Page' as const, title: 'Settings · Backups', sub: 'Local .mbk and S3/WebDAV backups', icon: 'download' as const, run: () => { location.hash = '#/settings/backups'; } },
          { id: 'pg:sv', group: 'Page' as const, title: 'Settings · Shared variables', sub: 'Global and per-host defaults', icon: 'key' as const, run: () => { location.hash = '#/settings/shared-vars'; } },
          { id: 'pg:conn', group: 'Page' as const, title: 'Settings · Connections', sub: 'OIDC, Coolify, registries and sister panels', icon: 'link' as const, run: () => { location.hash = '#/settings/connections'; } },
          { id: 'pg:notif', group: 'Page' as const, title: 'Settings · Notifications', sub: 'Alert channels', icon: 'bell' as const, run: () => { location.hash = '#/settings/notifications'; } },
          { id: 'pg:audit', group: 'Page' as const, title: 'Settings · Audit', sub: 'Action audit log', icon: 'clipboardList' as const, run: () => { location.hash = '#/settings/audit'; } },
          { id: 'pg:users', group: 'Page' as const, title: 'Settings · Users', sub: 'Accounts and SSO', icon: 'users' as const, run: () => { location.hash = '#/settings/users'; } },
          { id: 'pg:tsrc', group: 'Page' as const, title: 'Settings · Template sources', sub: 'Remote template indexes', icon: 'template' as const, run: () => { location.hash = '#/settings/templates'; } },
          { id: 'pg:addons', group: 'Page' as const, title: 'Settings · Add-ons', sub: 'Optional integrations', icon: 'plug' as const, run: () => { location.hash = '#/settings/addons'; } },
        ]
      : []),
    { id: 'pg:acc', group: 'Page', title: 'Account', sub: 'Password, TOTP and sessions', icon: 'userCircle', run: () => { location.hash = '#/account'; } },
  ]);

  const allItems = $derived.by<PaletteItem[]>(() => {
    const items: PaletteItem[] = [];
    for (const { hostId, hostName, stack } of allStacks) {
      items.push({
        id: `st:${hostId}/${stack.name}`,
        group: 'Stack',
        title: stack.displayName || stack.name,
        sub: [hostName, stack.coolifyProject, `${stack.running}/${stack.total} running`, stack.displayName ? stack.name : ''].filter(Boolean).join(' · '),
        icon: 'server',
        dot: stack.status,
        run: () => {
          location.hash = `#/h/${encodeURIComponent(hostId)}/stacks/${encodeURIComponent(stack.name)}`;
        },
      });
      for (const u of stack.urls ?? []) {
        items.push({
          id: `url:${hostId}/${stack.name}:${u}`,
          group: 'Service',
          title: u.replace(/^https?:\/\//, '').replace(/\/+$/, ''),
          sub: `${stack.name} (${hostName})`,
          icon: 'external',
          dot: stack.status,
          run: () => {
            window.open(u, '_blank', 'noopener,noreferrer');
          },
        });
      }
    }
    for (const h of app.hosts) {
      items.push({
        id: `host:${h.id}`,
        group: 'Host',
        title: h.name,
        sub: h.kind === 'local' ? 'Hub' : h.connected ? 'Agent · online' : 'Agent · offline',
        icon: 'server',
        dot: h.connected ? 'up' : 'offline',
        run: () => setHost(h.id),
      });
    }
    for (const c of checks) {
      items.push({
        id: `chk:${c.id}`,
        group: 'Check',
        title: c.name,
        sub: c.stack ? `${c.stack} · ${c.kind}` : c.kind,
        icon: 'pulse',
        dot: c.state.status,
        run: () => {
          location.hash = `#/checks/${c.id}`;
        },
      });
    }
    if (sister?.grafanaUrl) {
      items.push({
        id: 'sis:grafana',
        group: 'Service',
        title: 'Open Grafana',
        sub: sister.grafanaUrl,
        icon: 'external',
        run: () => window.open(sister!.grafanaUrl, '_blank', 'noopener,noreferrer'),
      });
    }
    if (sister?.beszelUrl) {
      items.push({
        id: 'sis:beszel',
        group: 'Service',
        title: 'Open Beszel',
        sub: sister.beszelUrl,
        icon: 'external',
        run: () => window.open(sister!.beszelUrl, '_blank', 'noopener,noreferrer'),
      });
    }
    for (const lk of sister?.links ?? []) {
      items.push({
        id: `sis:${lk.label}:${lk.url}`,
        group: 'Service',
        title: lk.label,
        sub: lk.url,
        icon: 'external',
        run: () => window.open(lk.url, '_blank', 'noopener,noreferrer'),
      });
    }
    items.push(...pageItems);
    return items;
  });

  const results = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (!q) return allItems.slice(0, 12);
    return allItems
      .filter((it) => it.title.toLowerCase().includes(q) || it.sub?.toLowerCase().includes(q) || it.group.toLowerCase().includes(q))
      .slice(0, 18);
  });

  $effect(() => {
    void results.length;
    if (cursor >= results.length) cursor = Math.max(0, results.length - 1);
  });

  function pick(it: PaletteItem) {
    open = false;
    it.run();
  }

  function onInputKey(e: KeyboardEvent) {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (results.length) cursor = (cursor + 1) % results.length;
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (results.length) cursor = (cursor - 1 + results.length) % results.length;
    } else if (e.key === 'Enter' && results[cursor]) {
      e.preventDefault();
      pick(results[cursor]);
    }
  }
</script>

<svelte:window onkeydown={onGlobalKey} />

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="pal-backdrop" onclick={() => (open = false)} onkeydown={(e) => e.key === 'Escape' && (open = false)}>
    <div
      class="card pal-box"
      role="dialog"
      aria-modal="true"
      aria-label="Quick search"
      tabindex="-1"
      onclick={(e) => e.stopPropagation()}
      onkeydown={onInputKey}
    >
      <div class="pal-input">
        <Icon name="search" />
        <input
          bind:this={inputEl}
          bind:value={query}
          placeholder="Jump to stack, host, service URL, check or page…"
          spellcheck="false"
          autocomplete="off"
        />
        {#if loading}<span class="meta">loading…</span>{/if}
        <kbd class="kbd">Esc</kbd>
      </div>
      <div class="pal-list" role="listbox">
        {#if results.length === 0}
          <p class="meta pal-empty">Nothing matches "{query}".</p>
        {:else}
          {#each results as it, i (it.id)}
            <button
              type="button"
              class="pal-item"
              class:active={i === cursor}
              role="option"
              aria-selected={i === cursor}
              onmouseenter={() => (cursor = i)}
              onclick={() => pick(it)}
            >
              {#if it.dot}
                <span class="dot {it.dot}"></span>
              {:else}
                <Icon name={it.icon} />
              {/if}
              <span class="pal-main">
                <span class="pal-title">{it.title}</span>
                {#if it.sub}<span class="meta pal-sub">{it.sub}</span>{/if}
              </span>
              <span class="badge">{it.group}</span>
            </button>
          {/each}
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .pal-backdrop {
    position: fixed;
    inset: 0;
    background: rgba(8, 14, 17, 0.68);
    display: grid;
    place-items: start center;
    padding: 10vh 16px 16px;
    z-index: 90;
  }
  .pal-box {
    width: min(600px, 100%);
    overflow: hidden;
    box-shadow: 0 0 0 1px var(--rope-stronger);
  }
  .pal-input {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 12px 14px;
    border-bottom: 1px solid var(--rope);
    color: var(--haze);
  }
  .pal-input input {
    flex: 1;
    min-width: 0;
    border: 0;
    background: transparent;
    color: var(--foam);
    font: 400 14px var(--font);
    outline: none;
  }
  .pal-input input::placeholder {
    color: var(--haze);
  }
  .kbd {
    font: 500 11px var(--mono);
    color: var(--haze);
    padding: 2px 6px;
    border-radius: 5px;
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .pal-list {
    max-height: 55vh;
    overflow-y: auto;
    padding: 6px;
    display: grid;
    gap: 2px;
  }
  .pal-item {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    padding: 8px 10px;
    border: 0;
    border-radius: var(--r-md);
    background: transparent;
    color: var(--foam);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .pal-item.active {
    background: var(--buoy-wash);
  }
  .pal-main {
    flex: 1;
    min-width: 0;
    display: flex;
    align-items: baseline;
    gap: 8px;
    flex-wrap: wrap;
  }
  .pal-title {
    font-weight: 500;
    font-size: 13px;
  }
  .pal-sub {
    font-size: 12px;
  }
  .pal-empty {
    padding: 16px 12px;
  }
</style>
