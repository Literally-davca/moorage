<script lang="ts">
  import { untrack } from 'svelte';
  import {
    api,
    type AddonCatalogEntry,
    type AddonInfo,
    type AddonSource,
  } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';
  import Select from '../lib/Select.svelte';
  import ComposeEditor from '../lib/ComposeEditor.svelte';
  import EnvEditor from '../lib/EnvEditor.svelte';
  import PromGenPanel from './PromGenPanel.svelte';
  import BuiltinAddonPanel from './BuiltinAddonPanel.svelte';

  let addons = $state<AddonInfo[] | null>(null);
  let catalog = $state<AddonCatalogEntry[] | null>(null);
  let sources = $state<AddonSource[]>([]);
  let catalogError = $state('');
  let loadingCatalog = $state(false);
  let error = $state('');
  let busy = $state<Record<string, boolean>>({});
  let rowError = $state<Record<string, string>>({});
  // M17B: what zero-config install filled into add-ons/.env (shown once under the row)
  let rowNotice = $state<Record<string, string>>({});
  let targetHosts = $state<string[]>([]);
  let removing = $state<AddonInfo | null>(null);
  let confirmOpen = $state(false);
  let openPanel = $state<string | null>(null);
  let openGuide = $state<string | null>(null);

  const ADDON_GUIDES: Record<string, { title: string; steps: string[]; envNote?: string }> = {
    'coolify-integration': {
      title: 'Coolify Integration Guide',
      steps: [
        'Enable the Coolify integration add-on here.',
        'Open Settings → Connections → Coolify API.',
        'In Coolify open Keys & Tokens → API tokens and create a token with read + deploy scopes.',
        'Paste your Coolify URL and token (enable Allow private/Tailscale IPs if needed) and click Test connection.',
        'Moorage decodes Coolify stack UUIDs into real project/app names globally and adds Start, Stop, Restart, Redeploy, deployments & .env editing inside stack details.',
      ],
      envNote: 'Optional env overrides: MOORAGE_COOLIFY_URL, MOORAGE_COOLIFY_TOKEN, MOORAGE_COOLIFY_ALLOW_PRIVATE=1',
    },
    'proxy-labels': {
      title: 'Reverse Proxy Labels Guide',
      steps: [
        'Enable Reverse proxy labels here.',
        'Add Traefik (Host(...)), Caddy, or moorage.url labels to your compose services.',
        'Open links appear automatically on stack headers and Ctrl+K Command Palette.',
      ],
    },
    'official-templates': {
      title: 'Official Templates Guide',
      steps: [
        'Enable Official Moorage templates here.',
        'Open Templates in the top navigation bar to deploy curated stacks with auto-generated secrets and variables.',
        'Add additional custom template repositories in Settings → Template sources.',
      ],
    },
    'prometheus-grafana': {
      title: 'Prometheus & Grafana Guide',
      steps: [
        'Enable Prometheus / Grafana metrics.',
        'Click Generators on this card to copy VictoriaMetrics/Prometheus scrape configs or JSON dashboards.',
        'Optionally pair Grafana URL + Service Account Token in Settings → Connections to push dashboards in 1 click.',
      ],
      envNote: 'Set MOORAGE_METRICS_TOKEN in env to lock metrics endpoints.',
    },
    'stack-scheduler': {
      title: 'Stack Scheduler Guide',
      steps: [
        'Enable Stack Scheduler here and click Configure on this card.',
        'Create cron tasks (e.g. 0 4 * * *) to run restart, start, stop, pull+up, or container exec jobs on any stack.',
      ],
    },
    'socket-proxy-check': {
      title: 'Socket Proxy Security Guide',
      steps: [
        'Enable Socket Proxy Auditor and click Configure → Scan hosts.',
        'Inspect any stack mounting /var/run/docker.sock directly and replace it with a scoped docker-socket-proxy snippet.',
      ],
    },
    'registry-webhooks': {
      title: 'Registry Webhooks Guide',
      steps: [
        'Enable Registry Webhooks and click Configure to generate a webhook receiver URL & HMAC secret.',
        'Add the URL to GHCR, Docker Hub, or Gitea webhooks to check or apply image updates automatically on push.',
      ],
    },
    'mcp-server': {
      title: 'MCP AI Server Guide',
      steps: [
        'Enable MCP Server and click Configure to generate an ACL-scoped bearer token.',
        'Point Claude Desktop, Cursor, or another MCP client at the Streamable HTTP endpoint.',
        'Keep "Require human approval" on so AI deploy drafts wait for your approval in this panel.',
      ],
    },
  };

  // Filters
  let statusFilter = $state<'all' | 'installed' | 'enabled' | 'disabled' | 'not_installed'>('all');
  let kindFilter = $state<'all' | 'builtin' | 'sidecar'>('all');
  let categoryFilter = $state<string>('all');
  let search = $state('');

  // Custom sidecar modal (create or edit)
  let customOpen = $state(false);
  let customEditHost = $state('');
  let customMode = $state<'create' | 'edit'>('create');
  let customID = $state('');
  let customIDManual = $state(false);
  let customName = $state('');
  let customCategory = $state('Custom');
  let customDesc = $state('');
  let customHosts = $state<string[]>([]);
  let customCompose = $state(`services:\n  my-sidecar:\n    image: alpine:latest\n    restart: unless-stopped\n`);
  let customEnv = $state('');
  let dockerRunCmd = $state('');
  let convertingRun = $state(false);
  let customSaving = $state(false);
  let customError = $state('');

  // Addon sources drawer
  let sourcesOpen = $state(false);
  let srcName = $state('');
  let srcUrl = $state('');
  let srcAllowPrivate = $state(false);
  let srcBusy = $state(false);
  let srcError = $state('');

  const writableHosts = $derived(app.hosts.filter((h) => h.connected && !h.readOnly));

  $effect(() => {
    if (targetHosts.length === 0 && writableHosts.length > 0) {
      const preferred = writableHosts.find((h) => h.id === app.host)?.id ?? writableHosts[0].id;
      targetHosts = [preferred];
    }
  });

  function rowKey(a: Pick<AddonInfo, 'id' | 'hostId'>): string {
    return `${a.id}@${a.hostId || ''}`;
  }

  function slugify(s: string): string {
    return s
      .toLowerCase()
      .replace(/[^a-z0-9_-]+/g, '-')
      .replace(/^[^a-z0-9]+/, '')
      .replace(/-+/g, '-')
      .slice(0, 56);
  }

  async function load(withCatalog = false, refresh = false) {
    if (withCatalog) loadingCatalog = true;
    try {
      const [r, srcRes] = await Promise.all([
        api.addons({ catalog: withCatalog || catalog !== null, refresh }),
        api.addonSources().catch(() => ({ sources: [] as AddonSource[] })),
      ]);
      addons = r.addons;
      if (r.catalog) catalog = r.catalog;
      catalogError = r.catalogError ?? '';
      sources = srcRes.sources ?? [];
      error = '';
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loadingCatalog = false;
    }
  }

  $effect(() => {
    untrack(() => void load(true));
  });

  async function toggle(a: AddonInfo) {
    if (!a.toggleable) return;
    const key = rowKey(a);
    busy = { ...busy, [key]: true };
    rowError = { ...rowError, [key]: '' };
    try {
      await api.setAddon(a.id, !a.enabled, a.hostId);
      await load();
    } catch (e) {
      rowError = { ...rowError, [key]: (e as Error).message };
    } finally {
      busy = { ...busy, [key]: false };
    }
  }

  async function install(entry: AddonCatalogEntry) {
    const key = `${entry.id}@${entry.sourceName || ''}`;
    busy = { ...busy, [key]: true };
    rowError = { ...rowError, [key]: '' };
    try {
      const installedOn = new Set(
        (addons ?? []).filter((a) => a.id === entry.id && a.hostId).map((a) => a.hostId!),
      );
      const hosts = entry.kind === 'sidecar' ? targetHosts.filter((h) => !installedOn.has(h)) : [];
      if (entry.kind === 'sidecar' && hosts.length === 0) {
        rowError = { ...rowError, [key]: 'Already installed on all selected hosts.' };
        return;
      }
      const res = await api.installAddon(entry.id, hosts, entry.sourceUrl);
      const gen = [...new Set(res?.generatedSecrets ?? [])];
      const added = [...new Set(res?.envAdded ?? [])].filter((k) => !gen.includes(k));
      const notes: string[] = [];
      if (added.length) notes.push(`Filled in add-ons/.env: ${added.join(', ')}.`);
      if (gen.length) notes.push(`Generated ${gen.join(', ')}: keep a copy, backups can't be restored without it.`);
      if (entry.kind === 'sidecar') notes.push('The add-ons stack updates automatically; switch it to notify in Updates if you prefer.');
      rowNotice = { ...rowNotice, [key]: notes.join(' ') };
      await load();
    } catch (e) {
      rowError = { ...rowError, [key]: (e as Error).message };
    } finally {
      busy = { ...busy, [key]: false };
    }
  }

  async function uninstall() {
    const a = removing;
    if (!a) return;
    const key = rowKey(a);
    busy = { ...busy, [key]: true };
    rowError = { ...rowError, [key]: '' };
    try {
      await api.uninstallAddon(a.id, a.hostId);
      await load();
    } catch (e) {
      rowError = { ...rowError, [key]: (e as Error).message };
    } finally {
      busy = { ...busy, [key]: false };
      removing = null;
    }
  }

  function openNewCustom() {
    customMode = 'create';
    customEditHost = '';
    customID = '';
    customIDManual = false;
    customName = '';
    customCategory = 'Custom';
    customDesc = '';
    customHosts = targetHosts.length ? [...targetHosts] : writableHosts.slice(0, 1).map((h) => h.id);
    customCompose = `services:\n  my-sidecar:\n    image: alpine:latest\n    restart: unless-stopped\n`;
    customEnv = '';
    dockerRunCmd = '';
    customError = '';
    customOpen = true;
  }

  async function openEditCustom(a: AddonInfo) {
    customError = '';
    try {
      const detail = await api.getCustomAddon(a.id, a.hostId);
      customMode = 'edit';
      customEditHost = detail.hostId || a.hostId || '';
      customID = detail.id;
      customIDManual = true;
      customName = detail.name;
      customCategory = detail.category || 'Custom';
      customDesc = detail.description || '';
      customHosts = customEditHost ? [customEditHost] : [];
      customCompose = detail.compose || '';
      customEnv = detail.env || '';
      dockerRunCmd = '';
      customOpen = true;
    } catch (e) {
      rowError = { ...rowError, [rowKey(a)]: (e as Error).message };
    }
  }

  async function convertDockerRun() {
    if (!dockerRunCmd.trim()) return;
    convertingRun = true;
    customError = '';
    try {
      const res = await api.convert(dockerRunCmd.trim());
      customCompose = res.yaml;
      if (!customName && res.service) {
        customName = res.service;
        if (!customIDManual) customID = slugify(res.service);
      }
    } catch (e) {
      customError = (e as Error).message;
    } finally {
      convertingRun = false;
    }
  }

  async function saveCustomAddon(e: SubmitEvent) {
    e.preventDefault();
    customSaving = true;
    customError = '';
    try {
      if (customMode === 'create') {
        await api.createCustomAddon({
          id: customID.trim(),
          name: customName.trim(),
          category: customCategory.trim() || 'Custom',
          description: customDesc.trim(),
          hostIds: customHosts,
          compose: customCompose,
          env: customEnv,
        });
      } else {
        await api.updateCustomAddon(customID, customEditHost, {
          name: customName.trim(),
          category: customCategory.trim() || 'Custom',
          description: customDesc.trim(),
          compose: customCompose,
          env: customEnv,
        });
      }
      customOpen = false;
      await load();
    } catch (e) {
      customError = (e as Error).message;
    } finally {
      customSaving = false;
    }
  }

  async function addSource(e: SubmitEvent) {
    e.preventDefault();
    srcBusy = true;
    srcError = '';
    try {
      await api.addAddonSource({ name: srcName.trim(), url: srcUrl.trim(), allowPrivate: srcAllowPrivate });
      srcName = '';
      srcUrl = '';
      srcAllowPrivate = false;
      await load(true, true);
    } catch (e) {
      srcError = (e as Error).message;
    } finally {
      srcBusy = false;
    }
  }

  async function toggleSource(s: AddonSource) {
    try {
      await api.updateAddonSource(s.id, {
        name: s.name,
        url: s.url,
        allowPrivate: s.allowPrivate,
        enabled: !s.enabled,
      });
      await load(true, false);
    } catch (e) {
      srcError = (e as Error).message;
    }
  }

  async function refreshSource(s: AddonSource) {
    srcBusy = true;
    srcError = '';
    try {
      await api.refreshAddonSource(s.id);
      await load(true, true);
    } catch (e) {
      srcError = (e as Error).message;
    } finally {
      srcBusy = false;
    }
  }

  async function removeSource(s: AddonSource) {
    try {
      await api.deleteAddonSource(s.id);
      await load(true, false);
    } catch (e) {
      srcError = (e as Error).message;
    }
  }

  const installedByIdHosts = $derived.by(() => {
    const m = new Map<string, string[]>();
    for (const a of addons ?? []) {
      if (a.kind !== 'sidecar') continue;
      const list = m.get(a.id) ?? [];
      if (a.hostId) list.push(a.hostId);
      m.set(a.id, list);
    }
    return m;
  });

  // Catalog sidecars remain available as long as at least one writable host doesn't have them yet.
  const available = $derived(
    (catalog ?? []).filter((c) => {
      if (c.kind !== 'sidecar') return false;
      const onHosts = new Set(installedByIdHosts.get(c.id) ?? []);
      return writableHosts.length === 0 ? onHosts.size === 0 : writableHosts.some((h) => !onHosts.has(h.id));
    }),
  );

  const categories = $derived.by(() => {
    const s = new Set<string>();
    for (const a of addons ?? []) if (a.category) s.add(a.category);
    for (const c of catalog ?? []) if (c.category) s.add(c.category);
    return [...s].sort();
  });

  const q = $derived(search.trim().toLowerCase());
  const filteredInstalled = $derived(
    (addons ?? []).filter((a) => {
      if (statusFilter === 'not_installed') return false;
      if (statusFilter === 'enabled' && !a.enabled) return false;
      if (statusFilter === 'disabled' && a.enabled) return false;
      if (kindFilter !== 'all' && a.kind !== kindFilter) return false;
      if (categoryFilter !== 'all' && a.category !== categoryFilter) return false;
      if (q && !`${a.name} ${a.description} ${a.category} ${a.hostName ?? ''} ${a.id}`.toLowerCase().includes(q)) return false;
      return true;
    }),
  );
  const filteredAvailable = $derived(
    available.filter((c) => {
      if (statusFilter === 'installed' || statusFilter === 'enabled' || statusFilter === 'disabled') return false;
      if (kindFilter !== 'all' && c.kind !== kindFilter) return false;
      if (categoryFilter !== 'all' && c.category !== categoryFilter) return false;
      if (q && !`${c.name} ${c.description} ${c.category} ${c.sourceName ?? ''} ${c.id}`.toLowerCase().includes(q)) return false;
      return true;
    }),
  );
  const hasFilters = $derived(statusFilter !== 'all' || kindFilter !== 'all' || categoryFilter !== 'all' || q !== '');
  function resetFilters() {
    statusFilter = 'all';
    kindFilter = 'all';
    categoryFilter = 'all';
    search = '';
  }

  function hostNameOf(id?: string): string {
    if (!id) return '';
    return app.hosts.find((h) => h.id === id)?.name ?? id;
  }
</script>

<div class="page">
  <section class="card card-pad head">
    <div class="head-row">
      <h2 class="section-title">Add-ons &amp; integrations</h2>
      <div class="head-actions">
        <button type="button" class="btn" onclick={() => (sourcesOpen = !sourcesOpen)} aria-expanded={sourcesOpen}>
          <Icon name="link" /> Sources ({sources.length + 1})
        </button>
        <button type="button" class="btn btn-primary" onclick={openNewCustom} disabled={writableHosts.length === 0}>
          <Icon name="plus" /> Custom add-on
        </button>
        <button type="button" class="btn" onclick={() => load(true, true)} disabled={loadingCatalog}>
          <Icon name="refresh" /> {loadingCatalog ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>
    </div>

    <div class="filters-bar">
      <input class="field search-in" bind:value={search} placeholder="Search add-ons…" aria-label="Search add-ons" />
      <div class="f-sel">
        <Select
          label="Filter by status"
          value={statusFilter}
          items={[
            { value: 'all', label: 'All statuses' },
            { value: 'installed', label: 'Installed' },
            { value: 'enabled', label: 'Enabled' },
            { value: 'disabled', label: 'Disabled' },
            { value: 'not_installed', label: 'Not installed' },
          ]}
          onchange={(v) => (statusFilter = v as typeof statusFilter)}
        />
      </div>
      <div class="f-sel">
        <Select
          label="Filter by type"
          value={kindFilter}
          items={[
            { value: 'all', label: 'All types' },
            { value: 'builtin', label: 'Built-in' },
            { value: 'sidecar', label: 'Sidecar' },
          ]}
          onchange={(v) => (kindFilter = v as typeof kindFilter)}
        />
      </div>
      <div class="f-sel">
        <Select
          label="Filter by category"
          value={categoryFilter}
          items={[{ value: 'all', label: 'All categories' }, ...categories.map((cat) => ({ value: cat, label: cat }))]}
          onchange={(v) => (categoryFilter = v)}
        />
      </div>
      {#if hasFilters}
        <button type="button" class="btn btn-ghost" onclick={resetFilters}>Reset</button>
      {/if}
    </div>
  </section>

  {#if sourcesOpen}
    <section class="card card-pad sources-card" aria-label="Add-on catalog sources">
      <div class="head-row">
        <div>
          <h3 class="section-title">Add-on catalog sources</h3>
          <p class="meta">Built-in official catalog plus custom HTTPS <code>index.json</code> repositories.</p>
        </div>
        <button type="button" class="btn btn-ghost btn-sm" onclick={() => (sourcesOpen = false)}><Icon name="x" /> Close</button>
      </div>

      <div class="src-list">
        <div class="src-row">
          <div>
            <strong>Official Moorage Add-ons</strong>
            <div class="meta mono">https://raw.githubusercontent.com/Literally-davca/moorage-addons/main/index.json</div>
          </div>
          <span class="badge up">Active</span>
        </div>
        {#each sources as src (src.id)}
          <div class="src-row">
            <div>
              <div class="addon-title">
                <strong>{src.name}</strong>
                {#if src.allowPrivate}<span class="badge">Private/LAN</span>{/if}
                <span class="badge {src.enabled ? 'up' : ''}">{src.enabled ? 'Enabled' : 'Disabled'}</span>
              </div>
              <div class="meta mono">{src.url}</div>
              {#if src.error}<div class="error-text">{src.error}</div>{/if}
            </div>
            <div class="addon-actions">
              <button type="button" class="btn btn-sm" onclick={() => refreshSource(src)} disabled={srcBusy}><Icon name="refresh" /> Refresh</button>
              <button type="button" class="btn btn-sm" onclick={() => toggleSource(src)}>{src.enabled ? 'Disable' : 'Enable'}</button>
              <button type="button" class="btn btn-sm btn-ghost btn-danger" onclick={() => removeSource(src)}><Icon name="trash" /> Remove</button>
            </div>
          </div>
        {/each}
      </div>

      <form class="src-form" onsubmit={addSource}>
        <input class="field" bind:value={srcName} placeholder="Source name (optional)" aria-label="Source name" />
        <input class="field mono" bind:value={srcUrl} placeholder="https://example.com/moorage-addons/index.json" aria-label="HTTPS index.json URL" required />
        <label class="check">
          <input type="checkbox" bind:checked={srcAllowPrivate} />
          <span class="meta">Allow private/Tailscale IP</span>
        </label>
        <button type="submit" class="btn btn-primary" disabled={srcBusy || !srcUrl.trim()}>Add source</button>
      </form>
      {#if srcError}<p class="error-text" role="alert">{srcError}</p>{/if}
    </section>
  {/if}

  {#if customOpen}
    <section class="card card-pad custom-card" aria-label={customMode === 'create' ? 'Add custom sidecar add-on' : 'Edit custom add-on'}>
      <div class="head-row">
        <h3 class="section-title">{customMode === 'create' ? 'New custom sidecar add-on' : `Edit ${customName}`}</h3>
        <button type="button" class="btn btn-ghost btn-sm" onclick={() => (customOpen = false)}><Icon name="x" /> Cancel</button>
      </div>

      <form class="custom-form" onsubmit={saveCustomAddon}>
        <div class="form-grid">
          <label class="f-col">
            <span class="meta">Name</span>
            <input
              class="field"
              bind:value={customName}
              oninput={() => {
                if (customMode === 'create' && !customIDManual) customID = slugify(customName);
              }}
              placeholder="e.g. Node Exporter"
              required
            />
          </label>
          <label class="f-col">
            <span class="meta">Add-on ID (slug)</span>
            <input
              class="field mono"
              bind:value={customID}
              oninput={() => (customIDManual = true)}
              placeholder="node-exporter"
              disabled={customMode === 'edit'}
              pattern="^[a-z0-9][a-z0-9_\-]{'{'}0,56{'}'}$"
              required
            />
          </label>
          <label class="f-col">
            <span class="meta">Category</span>
            <input class="field" bind:value={customCategory} placeholder="Monitoring" />
          </label>
          <label class="f-col">
            <span class="meta">Description</span>
            <input class="field" bind:value={customDesc} placeholder="Short summary of what this sidecar does" />
          </label>
        </div>

        {#if customMode === 'create'}
          <div class="f-col">
            <span class="meta">Target writable hosts</span>
            <MultiSelect
              label="Target hosts"
              placeholder="Select host(s)"
              bind:selected={customHosts}
              items={writableHosts.map((h) => ({ value: h.id, label: h.name, meta: h.kind }))}
            />
          </div>
        {:else}
          <p class="meta">Host: <span class="badge">{hostNameOf(customEditHost)}</span> · Stack: <code>add-ons</code></p>
        {/if}

        <div class="run-conv">
          <input
            class="field mono"
            bind:value={dockerRunCmd}
            placeholder="Optional: paste `docker run …` to convert into Compose"
            aria-label="docker run command"
          />
          <button type="button" class="btn" onclick={convertDockerRun} disabled={convertingRun || !dockerRunCmd.trim()}>
            <Icon name="wand" /> {convertingRun ? 'Converting…' : 'Convert docker run'}
          </button>
        </div>

        <ComposeEditor bind:value={customCompose} bind:env={customEnv} name="add-ons" title="Service fragment (compose.yaml)" />
        <EnvEditor bind:value={customEnv} compose={customCompose} label="Sidecar .env variables (auto-prefixed on collision)" />

        {#if customError}<p class="error-text" role="alert">{customError}</p>{/if}

        <div class="head-actions">
          <button type="submit" class="btn btn-primary" disabled={customSaving || (customMode === 'create' && customHosts.length === 0)}>
            <Icon name="check" /> {customSaving ? 'Saving & deploying…' : customMode === 'create' ? 'Install custom add-on' : 'Save & apply'}
          </button>
        </div>
      </form>
    </section>
  {/if}

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if addons === null && !error}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:45%"></div></section>
  {:else if addons}
    {#if filteredInstalled.length > 0}
      <div class="list">
        {#each filteredInstalled as a (rowKey(a))}
          {@const key = rowKey(a)}
          <section class="card card-pad addon">
            <div class="addon-row">
              <div class="addon-main">
                <div class="addon-title">
                  <span class="name">{a.name}</span>
                  <span class="badge">{a.category}</span>
                  {#if a.hostId}<span class="badge info">{a.hostName || a.hostId}</span>{/if}
                  {#if a.custom}<span class="badge">Custom</span>{/if}
                  <span class="badge {a.enabled ? 'up' : ''}">{a.enabled ? 'Enabled' : 'Disabled'}</span>
                </div>
                <p class="meta">{a.description}</p>
                {#if a.ram || a.cpu}
                  <p class="meta mono">
                    {#if a.ram}RAM: {a.ram}{/if}{#if a.ram && a.cpu} · {/if}{#if a.cpu}CPU: {a.cpu}{/if}
                  </p>
                {/if}
                {#if a.detail}<p class="mono detail">{a.detail}</p>{/if}
                {#if a.linkHref && a.linkLabel}
                  <p class="meta"><a href={a.linkHref}>{a.linkLabel}</a></p>
                {/if}
                {#if rowError[key]}<p class="error-text" role="alert">{rowError[key]}</p>{/if}
              </div>
              <div class="addon-actions">
                {#if ADDON_GUIDES[a.id] || a.docsUrl}
                  <button
                    type="button"
                    class="btn btn-sm btn-ghost"
                    onclick={() => (openGuide = openGuide === a.id ? null : a.id)}
                    aria-expanded={openGuide === a.id}
                  >
                    <Icon name="info" /> {openGuide === a.id ? 'Hide guide' : 'Guide'}
                  </button>
                {/if}
                {#if a.id === 'prometheus-grafana' && a.enabled}
                  <button type="button" class="btn btn-sm" onclick={() => (openPanel = openPanel === a.id ? null : a.id)}>
                    <Icon name="code" /> {openPanel === a.id ? 'Hide generators' : 'Generators'}
                  </button>
                {:else if (a.id === 'stack-scheduler' || a.id === 'socket-proxy-check' || a.id === 'registry-webhooks' || a.id === 'mcp-server') && a.enabled}
                  <button type="button" class="btn btn-sm" onclick={() => (openPanel = openPanel === a.id ? null : a.id)}>
                    <Icon name="settings" /> {openPanel === a.id ? 'Hide' : 'Configure'}
                  </button>
                {/if}
                {#if a.kind === 'sidecar' && a.custom}
                  <button type="button" class="btn btn-sm" onclick={() => openEditCustom(a)} disabled={busy[key]}>
                    <Icon name="code" /> Edit
                  </button>
                {/if}
                {#if a.toggleable}
                  <button
                    type="button"
                    class="btn"
                    disabled={busy[key]}
                    aria-busy={busy[key]}
                    onclick={() => toggle(a)}
                  >
                    {a.enabled ? 'Disable' : 'Enable'}
                  </button>
                {/if}
                {#if a.removable}
                  <button
                    type="button"
                    class="btn btn-ghost btn-danger"
                    disabled={busy[key]}
                    onclick={() => {
                      removing = a;
                      confirmOpen = true;
                    }}
                  >
                    <Icon name="trash" /> Uninstall
                  </button>
                {/if}
              </div>
            </div>
            {#if openGuide === a.id && (ADDON_GUIDES[a.id] || a.docsUrl)}
              <div class="guide-box">
                <div class="head-row">
                  <strong>{ADDON_GUIDES[a.id]?.title ?? `${a.name} Docs`}</strong>
                  {#if a.docsUrl}
                    <a class="meta" href={a.docsUrl} target="_blank" rel="noopener noreferrer">Full docs <Icon name="external" /></a>
                  {/if}
                </div>
                {#if ADDON_GUIDES[a.id]}
                  <ol class="guide-steps">
                    {#each ADDON_GUIDES[a.id].steps as step, si (si)}
                      <li>{step}</li>
                    {/each}
                  </ol>
                  {#if ADDON_GUIDES[a.id].envNote}
                    <p class="meta mono">{ADDON_GUIDES[a.id].envNote}</p>
                  {/if}
                {/if}
              </div>
            {/if}
            {#if openPanel === a.id && a.id === 'prometheus-grafana' && a.enabled}
              <PromGenPanel />
            {:else if openPanel === a.id && (a.id === 'stack-scheduler' || a.id === 'socket-proxy-check' || a.id === 'registry-webhooks' || a.id === 'mcp-server') && a.enabled}
              <BuiltinAddonPanel addonId={a.id} />
            {/if}
          </section>
        {/each}
      </div>
    {/if}
  {/if}

  {#if catalogError}
    <p class="error-text" role="alert">Catalog: {catalogError}</p>
  {/if}

  {#if catalog !== null && filteredAvailable.length > 0}
    <section class="card card-pad catalog-head">
      <div class="head-row">
        <div>
          <h2 class="section-title">Available sidecar add-ons</h2>
          <p class="meta">Installed per host into the shared <code>add-ons</code> stack.</p>
        </div>
        {#if writableHosts.length > 0}
          <div class="host-pick">
            <span class="meta">Target hosts</span>
            <MultiSelect
              label="Target hosts"
              placeholder="Select host(s)"
              bind:selected={targetHosts}
              items={writableHosts.map((h) => ({ value: h.id, label: h.name, meta: h.kind }))}
            />
          </div>
        {/if}
      </div>
    </section>

    <div class="list">
      {#each filteredAvailable as c (`${c.id}@${c.sourceName || 'official'}`)}
        {@const key = `${c.id}@${c.sourceName || ''}`}
        {@const onHosts = installedByIdHosts.get(c.id) ?? []}
        <section class="card card-pad addon">
          <div class="addon-row">
            <div class="addon-main">
              <div class="addon-title">
                <span class="name">{c.name}</span>
                <span class="badge">{c.category}</span>
                {#if c.sourceName && c.sourceName !== 'Official'}<span class="badge info">{c.sourceName}</span>{/if}
                {#each onHosts as hid (hid)}
                  <span class="badge up">Installed: {hostNameOf(hid)}</span>
                {/each}
              </div>
              <p class="meta">{c.description}</p>
              <p class="meta mono">RAM: {c.resources.ram} · CPU: {c.resources.cpu}</p>
              {#if c.readme && (!c.sourceUrl || c.sourceName === 'Official')}
                <p class="meta">
                  <a href={`https://github.com/Literally-davca/moorage-addons/blob/main/${c.readme}`} target="_blank" rel="noopener noreferrer">
                    Read documentation <Icon name="external" />
                  </a>
                </p>
              {/if}
              {#if rowError[key]}<p class="error-text" role="alert">{rowError[key]}</p>{/if}
              {#if rowNotice[key]}<p class="meta notice" role="status">{rowNotice[key]}</p>{/if}
            </div>
            <button
              type="button"
              class="btn"
              disabled={busy[key] || targetHosts.length === 0}
              aria-busy={busy[key]}
              onclick={() => install(c)}
            >
              <Icon name="plus" /> {busy[key] ? 'Installing…' : 'Install'}
            </button>
          </div>
        </section>
      {/each}
    </div>
  {/if}

  {#if addons && filteredInstalled.length === 0 && filteredAvailable.length === 0}
    <section class="card card-pad empty-row">
      <span class="meta">{hasFilters ? 'No add-ons match the selected filters.' : 'No add-ons available.'}</span>
      {#if hasFilters}
        <button type="button" class="btn" onclick={resetFilters}>Reset filters</button>
      {/if}
    </section>
  {/if}
</div>

<Confirm
  bind:open={confirmOpen}
  title={`Uninstall ${removing?.name ?? 'add-on'}?`}
  danger
  confirmLabel="Uninstall"
  body={removing?.kind === 'sidecar'
    ? `Removes ${removing.id} from ${removing.stackName} on ${removing.hostName || removing.hostId}.`
    : 'Removes this add-on configuration.'}
  onConfirm={uninstall}
/>

<style>
  .notice {
    color: var(--info-text);
    text-wrap: pretty;
  }
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head,
  .catalog-head,
  .sources-card,
  .custom-card {
    display: grid;
    gap: 10px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .head-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .filters-bar {
    display: grid;
    grid-template-columns: minmax(160px, 1.4fr) repeat(3, minmax(120px, 1fr)) auto;
    gap: 8px;
  }
  .src-list {
    display: grid;
    gap: 8px;
  }
  .src-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    flex-wrap: wrap;
  }
  .src-form {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .src-form .field {
    flex: 1 1 200px;
  }
  .custom-form {
    display: grid;
    gap: 12px;
  }
  .form-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 10px;
  }
  .f-col {
    display: grid;
    gap: 4px;
  }
  .run-conv {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .run-conv .field {
    flex: 1 1 280px;
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .empty-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  @media (max-width: 720px) {
    .filters-bar {
      grid-template-columns: 1fr 1fr;
    }
    .search-in {
      grid-column: 1 / -1;
    }
  }
  .host-pick {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .list {
    display: grid;
    gap: 10px;
  }
  .addon-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 16px;
    flex-wrap: wrap;
  }
  .addon-main {
    display: grid;
    gap: 6px;
    flex: 1 1 320px;
    min-width: 0;
  }
  .addon-title {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .addon-actions {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .name {
    font-weight: 600;
    font-size: 14px;
    color: var(--foam);
  }
  .detail {
    font-size: 12px;
    color: var(--mist);
    overflow-wrap: anywhere;
  }
  .f-sel {
    min-width: 150px;
  }
  .guide-box {
    margin-top: 8px;
    padding: 12px 14px;
    border-radius: var(--r-md);
    background: var(--well);
    border-left: 3px solid var(--buoy);
    display: grid;
    gap: 8px;
  }
  .guide-steps {
    margin: 0;
    padding-left: 18px;
    display: grid;
    gap: 4px;
    font-size: 13px;
    color: var(--mist);
  }
</style>
