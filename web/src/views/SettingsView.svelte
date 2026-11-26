<script lang="ts">
  import { isAdmin, type SettingsTab } from '../lib/state.svelte';
  import Icon, { type IconName } from '../lib/Icon.svelte';
  import HostsView from './HostsView.svelte';
  import GeneralSettingsView from './GeneralSettingsView.svelte';
  import BackupsView from './BackupsView.svelte';
  import SharedVarsView from './SharedVarsView.svelte';
  import ConnectionsView from './ConnectionsView.svelte';
  import NotificationsView from './NotificationsView.svelte';
  import AuditView from './AuditView.svelte';
  import UsersView from './UsersView.svelte';
  import TemplateSources from './TemplateSources.svelte';
  import AddonsView from './AddonsView.svelte';
  import ImportView from './ImportView.svelte';

  let { tab, onHostsChanged }: { tab: SettingsTab; onHostsChanged: () => void } = $props();

  const tabs = $derived<{ id: SettingsTab; label: string; icon: IconName; href: string }[]>([
    { id: 'hosts', label: 'Hosts', icon: 'server', href: '#/settings/hosts' },
    ...(isAdmin()
      ? [
          { id: 'general' as const, label: 'General', icon: 'settings' as const, href: '#/settings/general' },
          { id: 'backups' as const, label: 'Backups', icon: 'download' as const, href: '#/settings/backups' },
          { id: 'shared-vars' as const, label: 'Shared variables', icon: 'key' as const, href: '#/settings/shared-vars' },
          { id: 'connections' as const, label: 'Connections', icon: 'link' as const, href: '#/settings/connections' },
          { id: 'notifications' as const, label: 'Notifications', icon: 'bell' as const, href: '#/settings/notifications' },
          { id: 'audit' as const, label: 'Audit', icon: 'clipboardList' as const, href: '#/settings/audit' },
          { id: 'users' as const, label: 'Users', icon: 'users' as const, href: '#/settings/users' },
          { id: 'templates' as const, label: 'Templates', icon: 'template' as const, href: '#/settings/templates' },
          { id: 'addons' as const, label: 'Add-ons', icon: 'plug' as const, href: '#/settings/addons' },
          { id: 'import' as const, label: 'Import', icon: 'swap' as const, href: '#/settings/import' },
        ]
      : []),
  ]);

  const activeTab = $derived<SettingsTab>(tab !== 'hosts' && !isAdmin() ? 'hosts' : tab);
</script>

<div class="settings-page">
  <nav class="card subtabs" aria-label="Settings sections">
    {#each tabs as t (t.id)}
      <a class="tab" href={t.href} aria-current={activeTab === t.id ? 'page' : undefined}>
        <Icon name={t.icon} /> {t.label}
      </a>
    {/each}
  </nav>

  {#if activeTab === 'hosts'}
    <HostsView onChanged={onHostsChanged} />
  {:else if activeTab === 'general'}
    <GeneralSettingsView />
  {:else if activeTab === 'backups'}
    <BackupsView />
  {:else if activeTab === 'shared-vars'}
    <SharedVarsView />
  {:else if activeTab === 'connections'}
    <ConnectionsView />
  {:else if activeTab === 'notifications'}
    <NotificationsView />
  {:else if activeTab === 'audit'}
    <AuditView />
  {:else if activeTab === 'users'}
    <UsersView />
  {:else if activeTab === 'templates'}
    <TemplateSources />
  {:else if activeTab === 'addons'}
    <AddonsView />
  {:else if activeTab === 'import'}
    <ImportView />
  {/if}
</div>

<style>
  .settings-page {
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
</style>
