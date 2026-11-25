<script lang="ts">
  import type { Snippet } from 'svelte';
  import { api, type AddonInfo } from './api';
  import { app, isAdmin, isManualOverride, userPrefs } from './state.svelte';
  import Icon from './Icon.svelte';
  import Select from './Select.svelte';

  let {
    addonId,
    featureTitle,
    featureDescription = '',
    allowManualOverride = true,
    onchanged,
    children,
  }: {
    addonId: string;
    featureTitle: string;
    featureDescription?: string;
    allowManualOverride?: boolean;
    onchanged?: () => void;
    children: Snippet;
  } = $props();

  let loading = $state(true);
  let busy = $state(false);
  let err = $state('');
  let info = $state<AddonInfo | null>(null);
  let selectedHost = $state(app.host || 'local');

  const overridden = $derived(isManualOverride(addonId));
  const enabled = $derived(info ? info.enabled : false);
  const ready = $derived(enabled || overridden);

  async function refresh() {
    loading = true;
    err = '';
    try {
      const res = await api.addons();
      info = res.addons.find((a) => a.id === addonId) ?? null;
    } catch (e) {
      err = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void addonId;
    void refresh();
  });

  $effect(() => {
    if (!selectedHost && app.hosts.length) {
      selectedHost = app.hosts[0].id;
    }
  });

  async function enableAddon() {
    if (!info) return;
    busy = true;
    err = '';
    try {
      await api.setAddon(addonId, true, info.hostId);
      await refresh();
      onchanged?.();
    } catch (e) {
      err = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function installAndEnable() {
    busy = true;
    err = '';
    try {
      await api.installAddon(addonId, [selectedHost || app.host || 'local']);
      await refresh();
      onchanged?.();
    } catch (e) {
      err = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function manualOverride() {
    busy = true;
    err = '';
    try {
      const prefs = userPrefs();
      const current = prefs.manualOverrides ?? {};
      if (!current[addonId]) {
        const next = { ...prefs, manualOverrides: { ...current, [addonId]: true } };
        const saved = await api.savePreferences(next);
        if (app.user) app.user = { ...app.user, preferences: saved };
      }
      onchanged?.();
    } catch (e) {
      err = (e as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

{#if loading && !info}
  <div class="gate-loading meta">Checking {featureTitle} add-on status…</div>
{:else if ready || !info}
  {@render children()}
{:else}
  <div class="gate card">
    <div class="gate-head">
      <div class="gate-badge"><Icon name="puzzle" /></div>
      <div class="gate-copy">
        <h3>{featureTitle}</h3>
        <p class="meta">{featureDescription || info.description}</p>
        <p class="gate-why">
          {#if info.installed}
            The <strong>{info.name}</strong> add-on is installed but currently disabled. Enable it to unlock this feature{#if allowManualOverride}, or use manual override to configure it directly{/if}.
          {:else}
            The <strong>{info.name}</strong> add-on ({info.kind === 'sidecar' ? `sidecar container · ${info.ram ?? ''} RAM` : 'built-in'}) is not installed yet. Install & enable it to use {featureTitle}.
          {/if}
        </p>
      </div>
    </div>

    {#if err}<p class="error-text">{err}</p>{/if}

    <div class="gate-actions">
      {#if isAdmin()}
        {#if info.installed}
          <button type="button" class="btn btn-primary" disabled={busy} onclick={enableAddon}>
            <Icon name="power" />
            {busy ? 'Enabling…' : `Enable ${info.name}`}
          </button>
        {:else if info.kind === 'sidecar'}
          {#if app.hosts.length > 1}
            <div class="host-pick">
              <Select
                label="Target host"
                items={app.hosts.map((h) => ({ value: h.id, label: h.name, disabled: h.readOnly ? 'Read-only' : undefined }))}
                bind:value={selectedHost}
              />
            </div>
          {/if}
          <button type="button" class="btn btn-primary" disabled={busy} onclick={installAndEnable}>
            <Icon name="download" />
            {busy ? 'Installing…' : `Install & enable ${info.name}`}
          </button>
        {:else}
          <button type="button" class="btn btn-primary" disabled={busy} onclick={enableAddon}>
            <Icon name="power" />
            {busy ? 'Enabling…' : `Enable ${info.name}`}
          </button>
        {/if}
      {:else}
        <span class="meta">Ask a Moorage administrator to enable <strong>{info.name}</strong> in Settings → Add-ons.</span>
      {/if}

      {#if allowManualOverride}
        <button type="button" class="btn btn-ghost" disabled={busy} onclick={manualOverride}>
          <Icon name="tool" />
          Manual override
        </button>
      {/if}

      <a class="btn btn-ghost" href="#/settings/addons">
        <Icon name="settings" />
        Open Add-ons
      </a>
    </div>
  </div>
{/if}

<style>
  .gate-loading {
    padding: 12px 0;
  }
  .gate {
    padding: 18px 20px;
    display: grid;
    gap: 14px;
    border-left: 3px solid var(--buoy);
  }
  .gate-head {
    display: flex;
    gap: 14px;
    align-items: flex-start;
  }
  .gate-badge {
    width: 36px;
    height: 36px;
    border-radius: var(--r-sm);
    background: var(--buoy-wash);
    color: var(--buoy-text, var(--buoy));
    display: grid;
    place-items: center;
    flex-shrink: 0;
  }
  .gate-copy {
    display: grid;
    gap: 4px;
    min-width: 0;
  }
  .gate-copy h3 {
    margin: 0;
    font-size: 15px;
    color: var(--foam);
  }
  .gate-why {
    margin: 4px 0 0;
    font-size: 13px;
    color: var(--mist);
    line-height: 1.5;
  }
  .gate-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
  }
  .host-pick {
    min-width: 170px;
  }
</style>
