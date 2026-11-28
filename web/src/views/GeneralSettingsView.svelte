<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type HostLockRow, type SettingsConfig } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';

  let cfg = $state<SettingsConfig | null>(null);
  let error = $state('');
  let saving = $state(false);
  let saved = $state(false);

  let publicUrl = $state('');
  let statusPublicUrl = $state('');
  let agentPublicAddr = $state('');
  let updateChannel = $state<'stable' | 'pre'>('stable');
  let cleanupEnabled = $state(false);
  let cleanupSchedule = $state('03:30');
  let cleanupVolumes = $state(false);

  const diags = $derived(cfg?.diagnostics ?? []);

  async function load() {
    try {
      const r = await api.settingsConfig();
      cfg = r;
      publicUrl = r.publicUrl;
      statusPublicUrl = r.statusPublicUrl ?? '';
      agentPublicAddr = r.agentPublicAddr ?? '';
      updateChannel = r.updateChannel;
      cleanupEnabled = r.cleanupEnabled;
      cleanupSchedule = r.cleanupSchedule || '03:30';
      cleanupVolumes = r.cleanupVolumes;
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => void load());
  });

  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (!cfg) return;
    saving = true;
    saved = false;
    error = '';
    try {
      const patch: Record<string, unknown> = {
        cleanupEnabled,
        cleanupSchedule,
        cleanupVolumes,
      };
      if (!cfg.fromEnv.publicUrl) patch.publicUrl = publicUrl.trim();
      if (!cfg.fromEnv.statusPublicUrl) patch.statusPublicUrl = statusPublicUrl.trim();
      if (!cfg.fromEnv.agentPublicAddr) patch.agentPublicAddr = agentPublicAddr.trim();
      if (!cfg.fromEnv.updateChannel) patch.updateChannel = updateChannel;
      cfg = await api.saveSettingsConfig(patch);
      saved = true;
      setTimeout(() => (saved = false), 2500);
    } catch (err) {
      error = (err as Error).message;
    } finally {
      saving = false;
    }
  }

  // Environment locks: column, label and the variable that sets it (shown on hover)
  const lockCols = [
    { key: 'readOnly', label: 'Read-only', env: 'MOORAGE_READ_ONLY' },
    { key: 'manageExternal', label: 'Manage external', env: 'MOORAGE_MANAGE_EXTERNAL' },
    { key: 'containerTerminal', label: 'Container terminal', env: 'MOORAGE_TERMINAL' },
    { key: 'hostTerminal', label: 'Host terminal', env: 'MOORAGE_HOST_TERMINAL' },
    { key: 'exec', label: 'Scheduled exec', env: 'MOORAGE_EXEC' },
    { key: 'selfUpdate', label: 'Self-update', env: 'MOORAGE_SELF_UPDATE' },
  ] as const;
</script>

<div class="page">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if cfg === null && !error}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:45%"></div></section>
  {:else if cfg}
    {#if diags.length > 0}
      <section class="card card-pad diag">
        <h2 class="section-title">Diagnostics</h2>
        <div class="diag-list">
          {#each diags as d (d.id)}
            <div class="diag-row" class:warn={d.level === 'warn'}>
              <Icon name="alert" />
              <div>
                <div class="diag-title">{d.title}</div>
                <div class="meta">{d.detail}</div>
              </div>
            </div>
          {/each}
        </div>
      </section>
    {/if}

    <section class="card card-pad head-row" aria-label="Editor experience">
      <div>
        <h2 class="section-title">Experience &amp; editor defaults</h2>
        <p class="meta">Beginner vs. Advanced mode, Compose Form/YAML defaults, linter tips, and confirmations are stored per user.</p>
      </div>
      <a class="btn btn-ghost" href="#/account"><Icon name="userCircle" /> Configure in Account</a>
    </section>

    <form class="card card-pad form" onsubmit={save}>
      <h2 class="section-title">Hub network &amp; updates</h2>

      <div class="grid-2">
        <label class="field-wrap">
          <span class="label-row">
            <span>Public URL</span>
            {#if cfg.fromEnv.publicUrl}<span class="pill env">Set via environment</span>{/if}
            {#if !cfg.fromEnv.publicUrl && !publicUrl}
              <button type="button" class="link-btn" onclick={() => (publicUrl = location.origin)}>Use {location.origin}</button>
            {/if}
          </span>
          <input class="field" bind:value={publicUrl} placeholder="https://moorage.example.com" disabled={cfg.fromEnv.publicUrl} />
          <span class="meta">Used immediately for alert links, invite URLs, RSS feeds and OIDC callbacks.</span>
        </label>

        <label class="field-wrap">
          <span class="label-row">
            <span>Status Page URL</span>
            {#if cfg.fromEnv.statusPublicUrl}<span class="pill env">Set via environment</span>{/if}
          </span>
          <input class="field" bind:value={statusPublicUrl} placeholder="https://status.example.com (optional)" disabled={cfg.fromEnv.statusPublicUrl} />
          <span class="meta">Public address of a separate <code>MOORAGE_STATUS_LISTEN</code> listener.</span>
        </label>

        <label class="field-wrap">
          <span class="label-row">
            <span>Agent Connect Address</span>
            {#if cfg.fromEnv.agentPublicAddr}<span class="pill env">Set via environment</span>{/if}
          </span>
          <input class="field mono" bind:value={agentPublicAddr} placeholder="100.87.100.2:5081" disabled={cfg.fromEnv.agentPublicAddr} />
          <span class="meta">Prefills <code>MOORAGE_HUB</code> in generated agent compose files when pairing a host.</span>
        </label>

        <div class="field-wrap">
          <span class="label-row">
            <span>Update channel</span>
            {#if cfg.fromEnv.updateChannel}<span class="pill env">Set via environment</span>{/if}
          </span>
          <Select
            label="Update channel"
            items={[
              { value: 'stable', label: 'Stable' },
              { value: 'pre', label: 'Pre-release' },
            ]}
            value={updateChannel}
            disabled={cfg.fromEnv.updateChannel}
            onchange={(v) => (updateChannel = v as 'stable' | 'pre')}
          />
          <span class="meta">Release stream checked in <a href="#/settings/hosts">Hosts → Moorage updates</a>.</span>
        </div>
      </div>

      <div class="divider"></div>

      <h3 class="sub-title">Daily disk cleanup</h3>
      <label class="check-row">
        <input type="checkbox" bind:checked={cleanupEnabled} />
        <span>Run daily cleanup of unused images, stopped containers and build cache on writable hosts</span>
      </label>
      {#if cleanupEnabled}
        <div class="row-2">
          <label class="field-wrap">
            <span>Schedule (HH:MM)</span>
            <input class="field mono" bind:value={cleanupSchedule} placeholder="03:30" maxlength="5" />
          </label>
          <label class="check-row" style="align-self:end;height:32px">
            <input type="checkbox" bind:checked={cleanupVolumes} />
            <span>Also prune unused volumes</span>
          </label>
        </div>
      {/if}

      <div class="actions">
        <button type="submit" class="btn btn-primary" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
        {#if saved}<span class="meta ok"><Icon name="check" /> Applied live</span>{/if}
      </div>
    </form>

    <section class="card card-pad locks">
      <div class="head-row">
        <div>
          <h2 class="section-title">Environment locks</h2>
          <p class="meta">Security switches controlled strictly by machine environment variables (never editable from the web).</p>
        </div>
        {#if cfg.envLocks.trustedProxies}
          <span class="pill on">Trusted proxies active</span>
        {:else}
          <span class="pill">Trusted proxies off</span>
        {/if}
      </div>

      {#if cfg.hostLocks?.length}
        {#snippet lockPill(on: boolean, c: (typeof lockCols)[number])}
          <span class="pill {on ? (c.key === 'readOnly' ? 'warn-pill' : 'on') : ''}">{c.key === 'readOnly' ? (on ? 'Locked (RO)' : 'Writable') : on ? 'On' : 'Off'}</span>
        {/snippet}
        {#snippet hostCell(hl: HostLockRow)}
          <span class="hname">{hl.name}</span>
          <span class="pill">{hl.kind === 'local' ? 'Hub' : hl.connected ? 'Agent' : 'Offline'}</span>
        {/snippet}
        <!-- M20A: a table from 768 px, a card per host below -->
        <div class="lock-table">
          <table>
            <thead>
              <tr>
                <th class="hcol">Host</th>
                {#each lockCols as c (c.key)}<th title={c.env}>{c.label}</th>{/each}
              </tr>
            </thead>
            <tbody>
              {#each cfg.hostLocks as hl (hl.id)}
                <tr>
                  <td class="hcol">{@render hostCell(hl)}</td>
                  {#each lockCols as c (c.key)}<td>{@render lockPill(hl[c.key], c)}</td>{/each}
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
        <div class="lock-cards">
          {#each cfg.hostLocks as hl (hl.id)}
            <div class="lock-card">
              <div>{@render hostCell(hl)}</div>
              {#each lockCols as c (c.key)}
                <div class="lock-row"><span class="meta" title={c.env}>{c.label}</span>{@render lockPill(hl[c.key], c)}</div>
              {/each}
            </div>
          {/each}
        </div>
      {/if}
    </section>
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .form {
    display: grid;
    gap: 14px;
  }
  .grid-2 {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
    gap: 14px 16px;
  }
  .field-wrap {
    display: grid;
    gap: 5px;
    font-size: 13px;
    color: var(--mist);
    align-content: start;
  }
  .label-row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .link-btn {
    background: none;
    border: 0;
    color: var(--buoy-text);
    font-size: 12px;
    cursor: pointer;
    padding: 0;
  }
  .check-row {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    color: var(--foam);
    cursor: pointer;
  }
  .row-2 {
    display: grid;
    grid-template-columns: 160px 1fr;
    gap: 12px;
  }
  .divider {
    height: 1px;
    background: var(--rope);
    margin: 2px 0;
  }
  .sub-title {
    margin: 0;
    font-size: 13px;
    font-weight: 600;
    color: var(--foam);
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .ok {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: var(--afloat-text);
  }
  .diag-list {
    display: grid;
    gap: 8px;
    margin-top: 8px;
  }
  .diag-row {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--well);
  }
  .diag-row.warn {
    box-shadow: inset 3px 0 0 var(--signal);
  }
  .diag-title {
    font-size: 13px;
    font-weight: 500;
    color: var(--foam);
  }
  .head-row {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
  }
  .locks {
    display: grid;
    gap: 12px;
  }
  .hname {
    font-weight: 500;
    color: var(--foam);
    margin-right: 6px;
  }
  .lock-table table {
    width: 100%;
    table-layout: fixed;
    border-collapse: collapse;
    font-size: 12px;
  }
  .lock-table th,
  .lock-table td {
    text-align: left;
    padding: 7px 8px;
    border-bottom: 1px solid var(--rope);
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .lock-table th {
    color: var(--haze);
    font-weight: 500;
  }
  .lock-table .hcol {
    width: 26%;
    white-space: nowrap;
  }
  .lock-cards {
    display: none;
    gap: 8px;
  }
  .lock-card {
    display: grid;
    gap: 4px;
    padding: 8px 0;
    border-bottom: 1px solid var(--rope);
  }
  .lock-row {
    display: flex;
    justify-content: space-between;
    gap: 8px;
  }
  @media (max-width: 767px) {
    .lock-table {
      display: none;
    }
    .lock-cards {
      display: grid;
    }
  }
  .pill {
    display: inline-flex;
    align-items: center;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .pill.on {
    background: color-mix(in srgb, var(--afloat) 16%, transparent);
    color: var(--afloat-text, var(--afloat));
  }
  .pill.warn-pill {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .pill.env {
    background: var(--buoy-wash);
    color: var(--buoy-text);
  }
  @media (max-width: 560px) {
    .row-2 {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
