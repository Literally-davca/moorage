<script lang="ts">
  import { untrack } from 'svelte';
  import { api } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';

  let tab = $state<'grafana' | 'scrape' | 'homelab'>('grafana');
  let scope = $state<'all' | 'host' | 'custom'>('all');
  let hostId = $state(untrack(() => app.host));
  let stacksFilter = $state('');
  let datasourceUid = $state('${DS_PROMETHEUS}');

  let dashJson = $state('');
  let pushing = $state(false);
  let pushResult = $state<{ url: string; status: string } | null>(null);
  let scrape = $state<{ victoriametrics: { scrape: string; alerts: string }; prometheus: { scrape: string; alerts: string } } | null>(null);
  let homelab = $state<{ homepage: string; glance: string; homarr: string } | null>(null);
  let homelabKind = $state<'homepage' | 'glance' | 'homarr'>('homepage');
  let scrapeKind = $state<'vm' | 'prom'>('vm');
  let loading = $state(false);
  let error = $state('');
  let copied = $state('');

  async function loadDashboard() {
    loading = true;
    error = '';
    try {
      const d = await api.promDashboard({
        scope,
        host: scope === 'host' ? hostId : undefined,
        stacks: scope === 'custom' ? stacksFilter : undefined,
        datasourceUid,
      });
      dashJson = JSON.stringify(d, null, 2);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  async function pushToGrafana() {
    pushing = true;
    error = '';
    pushResult = null;
    try {
      const res = await api.pushGrafanaDashboard({
        scope,
        hostId: scope === 'host' ? hostId : undefined,
        stacks: scope === 'custom'
          ? stacksFilter.split(',').map((s) => s.trim()).filter(Boolean)
          : undefined,
        datasourceUid,
      });
      pushResult = { url: res.url, status: res.status };
    } catch (e) {
      error = (e as Error).message;
    } finally {
      pushing = false;
    }
  }

  async function loadScrape() {
    if (scrape) return;
    loading = true;
    error = '';
    try {
      scrape = await api.promScrape();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  async function loadHomelab() {
    if (homelab) return;
    loading = true;
    error = '';
    try {
      homelab = await api.promHomelab();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    if (tab === 'grafana') {
      void scope;
      void hostId;
      void datasourceUid;
      untrack(() => void loadDashboard());
    } else if (tab === 'scrape') {
      untrack(() => void loadScrape());
    } else {
      untrack(() => void loadHomelab());
    }
  });

  async function copy(text: string, what: string) {
    try {
      await navigator.clipboard.writeText(text);
      copied = what;
      setTimeout(() => {
        if (copied === what) copied = '';
      }, 2000);
    } catch {
      /* ignore */
    }
  }

  function downloadDash() {
    if (!dashJson) return;
    const blob = new Blob([dashJson], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `moorage-dashboard-${scope}.json`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
  }
</script>

<div class="prom-panel">
  <div class="subtabs" role="tablist">
    <button type="button" class="stab" role="tab" aria-selected={tab === 'grafana'} onclick={() => (tab = 'grafana')}>Grafana dashboard</button>
    <button type="button" class="stab" role="tab" aria-selected={tab === 'scrape'} onclick={() => (tab = 'scrape')}>Scrape &amp; alerts</button>
    <button type="button" class="stab" role="tab" aria-selected={tab === 'homelab'} onclick={() => (tab = 'homelab')}>Homelab dashboards</button>
  </div>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if tab === 'grafana'}
    <div class="controls">
      <label class="ctrl">
        <span class="meta">Scope</span>
        <select class="field" bind:value={scope}>
          <option value="all">All hosts (Overview)</option>
          <option value="host">Single host</option>
          <option value="custom">Selected stacks</option>
        </select>
      </label>
      {#if scope === 'host'}
        <label class="ctrl">
          <span class="meta">Host</span>
          <select class="field" bind:value={hostId}>
            {#each app.hosts as h (h.id)}
              <option value={h.id}>{h.name}</option>
            {/each}
          </select>
        </label>
      {:else if scope === 'custom'}
        <label class="ctrl">
          <span class="meta">Stacks (comma-separated)</span>
          <input class="field mono" bind:value={stacksFilter} placeholder="traefik, postgres" onchange={loadDashboard} />
        </label>
      {/if}
      <label class="ctrl">
        <span class="meta">Datasource UID</span>
        <input class="field mono" bind:value={datasourceUid} placeholder="${'{DS_PROMETHEUS}'}" />
      </label>
    </div>

    <div class="btn-row">
      <button type="button" class="btn btn-primary" onclick={pushToGrafana} disabled={pushing}>
        <Icon name="upload" /> {pushing ? 'Pushing…' : 'Push / Update in Grafana'}
      </button>
      <button type="button" class="btn" onclick={downloadDash} disabled={!dashJson}>
        <Icon name="download" /> Download JSON
      </button>
      <button type="button" class="btn" onclick={() => copy(dashJson, 'dash')} disabled={!dashJson}>
        <Icon name="copy" /> {copied === 'dash' ? 'Copied' : 'Copy JSON'}
      </button>
    </div>

    {#if pushResult}
      <p class="ok-msg" role="status">
        <Icon name="check" /> Dashboard {pushResult.status || 'saved'} in Grafana.
        {#if pushResult.url}
          <a href={pushResult.url} target="_blank" rel="noopener noreferrer">Open dashboard <Icon name="external" /></a>
        {/if}
      </p>
    {/if}

    <pre class="code mono">{loading ? 'Generating…' : dashJson}</pre>
  {:else if tab === 'scrape' && scrape}
    <div class="btn-row">
      <button type="button" class="btn btn-sm" class:active={scrapeKind === 'vm'} onclick={() => (scrapeKind = 'vm')}>VictoriaMetrics + vmalert</button>
      <button type="button" class="btn btn-sm" class:active={scrapeKind === 'prom'} onclick={() => (scrapeKind = 'prom')}>Prometheus + Alertmanager</button>
    </div>
    {@const activeScrape = scrapeKind === 'vm' ? scrape.victoriametrics : scrape.prometheus}
    <div class="snip-head">
      <span class="meta">Scrape config (token is never embedded in plaintext)</span>
      <button type="button" class="btn btn-sm" onclick={() => copy(activeScrape.scrape, 'sc')}>
        <Icon name="copy" /> {copied === 'sc' ? 'Copied' : 'Copy'}
      </button>
    </div>
    <pre class="code mono">{activeScrape.scrape}</pre>
    <div class="snip-head">
      <span class="meta">Alerting rules</span>
      <button type="button" class="btn btn-sm" onclick={() => copy(activeScrape.alerts, 'al')}>
        <Icon name="copy" /> {copied === 'al' ? 'Copied' : 'Copy'}
      </button>
    </div>
    <pre class="code mono">{activeScrape.alerts}</pre>
  {:else if tab === 'homelab' && homelab}
    <div class="btn-row">
      <button type="button" class="btn btn-sm" class:active={homelabKind === 'homepage'} onclick={() => (homelabKind = 'homepage')}>Homepage (services.yaml)</button>
      <button type="button" class="btn btn-sm" class:active={homelabKind === 'glance'} onclick={() => (homelabKind = 'glance')}>Glance (glance.yml)</button>
      <button type="button" class="btn btn-sm" class:active={homelabKind === 'homarr'} onclick={() => (homelabKind = 'homarr')}>Homarr (JSON)</button>
      <button type="button" class="btn btn-sm" onclick={() => copy(homelab![homelabKind], 'hl')}>
        <Icon name="copy" /> {copied === 'hl' ? 'Copied' : 'Copy'}
      </button>
    </div>
    <pre class="code mono">{homelab[homelabKind]}</pre>
  {/if}
</div>

<style>
  .prom-panel {
    margin-top: 12px;
    padding-top: 12px;
    border-top: 1px solid var(--rope);
    display: grid;
    gap: 10px;
  }
  .subtabs {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .stab {
    border: 0;
    border-radius: var(--r-md);
    padding: 5px 10px;
    background: var(--well);
    color: var(--mist);
    font: 500 12px var(--font);
    cursor: pointer;
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .stab[aria-selected='true'] {
    background: var(--buoy-wash);
    color: var(--buoy-text);
    box-shadow: inset 0 0 0 1px var(--buoy);
  }
  .controls {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: 8px;
  }
  .ctrl {
    display: grid;
    gap: 4px;
  }
  .btn-row {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    align-items: center;
  }
  .btn.active {
    background: var(--buoy-wash);
    color: var(--buoy-text);
    box-shadow: inset 0 0 0 1px var(--buoy);
  }
  .snip-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }
  .ok-msg {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--afloat-text);
    font-size: 13px;
  }
  .code {
    margin: 0;
    padding: 10px 12px;
    max-height: 280px;
    overflow: auto;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    font-size: 12px;
    line-height: 1.5;
    color: var(--foam);
    white-space: pre;
  }
</style>
