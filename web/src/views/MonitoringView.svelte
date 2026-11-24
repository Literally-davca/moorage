<script lang="ts">
  import { untrack } from 'svelte';
  import { api } from '../lib/api';
  import { app, type MonitoringTab } from '../lib/state.svelte';
  import Icon, { type IconName } from '../lib/Icon.svelte';
  import ChecksView from './ChecksView.svelte';
  import StatusPagesView from './StatusPagesView.svelte';
  import MaintenanceView from './MaintenanceView.svelte';

  let { tab }: { tab: MonitoringTab } = $props();
  let grafanaUrl = $state('');

  $effect(() => {
    untrack(() => {
      api.sisterLinks()
        .then((sl) => (grafanaUrl = sl.grafanaUrl))
        .catch(() => {});
    });
  });

  const tabs: { id: MonitoringTab; label: string; icon: IconName; href: string }[] = [
    { id: 'checks', label: 'Checks', icon: 'pulse', href: '#/monitoring/checks' },
    { id: 'status-pages', label: 'Status pages', icon: 'world', href: '#/monitoring/status-pages' },
    { id: 'maintenance', label: 'Maintenance', icon: 'tool', href: '#/monitoring/maintenance' },
  ];
</script>

<div class="mon-page">
  <nav class="card subtabs" aria-label="Monitoring sections">
    {#each tabs as t (t.id)}
      <a class="tab" href={t.href} aria-current={tab === t.id ? 'page' : undefined}>
        <Icon name={t.icon} /> {t.label}
        {#if t.id === 'checks' && (app.nav?.checksFailing ?? 0) > 0}
          <span class="cnt" title="Checks failing">{app.nav?.checksFailing}</span>
        {:else if t.id === 'maintenance' && (app.nav?.maintenanceActive ?? 0) > 0}
          <span class="cnt blue" title="Maintenance windows running">{app.nav?.maintenanceActive}</span>
        {/if}
      </a>
    {/each}
    {#if grafanaUrl}
      <a class="tab ext-tab" href={grafanaUrl} target="_blank" rel="noopener noreferrer">
        Open Grafana <Icon name="external" />
      </a>
    {/if}
  </nav>

  {#if tab === 'checks'}
    <ChecksView />
  {:else if tab === 'status-pages'}
    <StatusPagesView />
  {:else if tab === 'maintenance'}
    <MaintenanceView />
  {/if}
</div>

<style>
  .mon-page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .subtabs {
    display: flex;
    gap: 4px;
    flex-wrap: wrap;
    padding: 6px 8px;
  }
  .tab {
    display: inline-flex;
    align-items: center;
    gap: 7px;
    height: 32px;
    padding: 0 12px;
    border-radius: var(--r-md);
    color: var(--mist);
    font-size: 13px;
    font-weight: 500;
    text-decoration: none;
  }
  .tab:hover {
    background: var(--rope);
    color: var(--foam);
    text-decoration: none;
  }
  .tab[aria-current='page'] {
    background: var(--buoy-wash);
    color: var(--foam);
  }
  .ext-tab {
    margin-left: auto;
    color: var(--buoy-text);
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
</style>
