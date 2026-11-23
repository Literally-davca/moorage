<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type ImportPreview, type SettingsConfig } from '../lib/api';
  import Icon from '../lib/Icon.svelte';

  let cfg = $state<SettingsConfig | null>(null);
  let error = $state('');
  let saving = $state(false);
  let saved = $state(false);
  let genToken = $state('');
  let copied = $state(false);
  // backup & restore
  let exportPw = $state('');
  let exportPw2 = $state('');
  let exporting = $state(false);
  let importFile = $state<File | null>(null);
  let importPw = $state('');
  let importing = $state(false);
  let preview = $state<ImportPreview | null>(null);
  let restoreDb = $state(true);
  let restoreStacks = $state(true);
  let applying = $state(false);
  let importMsg = $state('');
  let backupErr = $state('');

  let publicUrl = $state('');
  let updateChannel = $state<'stable' | 'pre'>('stable');
  let cleanupEnabled = $state(false);
  let cleanupSchedule = $state('03:30');
  let cleanupVolumes = $state(false);

  async function load() {
    try {
      const r = await api.settingsConfig();
      cfg = r;
      publicUrl = r.publicUrl;
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
      if (!cfg.fromEnv.updateChannel) patch.updateChannel = updateChannel;
      cfg = await api.saveSettingsConfig(patch);
      saved = true;
    } catch (err) {
      error = (err as Error).message;
    } finally {
      saving = false;
    }
  }

  async function generateToken() {
    error = '';
    genToken = '';
    try {
      const r = await api.generateMetricsToken();
      genToken = r.token;
      await load();
    } catch (err) {
      error = (err as Error).message;
    }
  }

  async function clearToken() {
    error = '';
    genToken = '';
    try {
      cfg = await api.saveSettingsConfig({ metricsToken: '' });
    } catch (err) {
      error = (err as Error).message;
    }
  }

  async function copy(text: string) {
    try {
      await navigator.clipboard.writeText(text);
      copied = true;
      setTimeout(() => (copied = false), 1500);
    } catch {
      /* ignore */
    }
  }

  async function doExport(e: SubmitEvent) {
    e.preventDefault();
    backupErr = '';
    if (exportPw !== exportPw2) {
      backupErr = "The passwords don't match.";
      return;
    }
    exporting = true;
    try {
      await api.exportBackup(exportPw);
      exportPw = exportPw2 = '';
    } catch (err) {
      backupErr = (err as Error).message;
    } finally {
      exporting = false;
    }
  }

  async function doPreview(e: SubmitEvent) {
    e.preventDefault();
    if (!importFile) return;
    backupErr = '';
    importMsg = '';
    importing = true;
    try {
      preview = await api.importBackupPreview(importFile, importPw);
      restoreDb = true;
      restoreStacks = preview.hosts.some((h) => h.writable && h.stacks.length > 0);
    } catch (err) {
      backupErr = (err as Error).message;
    } finally {
      importing = false;
    }
  }

  async function doApply() {
    if (!preview) return;
    backupErr = '';
    applying = true;
    try {
      const r = await api.importBackupApply(preview.token, restoreDb, restoreStacks);
      const parts: string[] = [];
      if (restoreStacks) parts.push(`${r.restoredStacks} ${r.restoredStacks === 1 ? 'stack' : 'stacks'} restored`);
      if (r.dbStaged) parts.push('the database is replaced when Moorage restarts (the current one is kept as moorage.db.bak)');
      importMsg = parts.join('; ') + '.';
      if (r.failedStacks.length) backupErr = `Not restored: ${r.failedStacks.join(', ')}`;
      preview = null;
      importFile = null;
      importPw = '';
    } catch (err) {
      backupErr = (err as Error).message;
    } finally {
      applying = false;
    }
  }

  const replaced = $derived(preview ? preview.hosts.reduce((n, h) => n + (h.writable ? h.stacks.filter((s) => s.exists).length : 0), 0) : 0);
  const unavailable = $derived(preview ? preview.hosts.filter((h) => !h.writable && h.stacks.length > 0) : []);
</script>

<div class="page">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if cfg === null && !error}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:45%"></div></section>
  {:else if cfg}
    {#if cfg.diagnostics.length > 0}
      <section class="card card-pad diag">
        <h2 class="section-title">Diagnostics</h2>
        <div class="diag-list">
          {#each cfg.diagnostics as d (d.id)}
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

    <form class="card card-pad form" onsubmit={save}>
      <h2 class="section-title">General</h2>

      <label class="field-wrap">
        <span class="label-row">
          <span>Public URL</span>
          {#if cfg.fromEnv.publicUrl}<span class="pill env">from env</span>{/if}
          {#if !cfg.fromEnv.publicUrl && !publicUrl}
            <button type="button" class="link-btn" onclick={() => (publicUrl = location.origin)}>Use {location.origin}</button>
          {/if}
        </span>
        <input class="field" bind:value={publicUrl} placeholder="https://moorage.example.com" disabled={cfg.fromEnv.publicUrl} />
        {#if cfg.publicUrlRestart}
          <span class="meta">Saved. Restart Moorage to use it in alerts, sign-in and invites.</span>
        {/if}
      </label>

      <label class="field-wrap">
        <span class="label-row">
          <span>Update channel</span>
          {#if cfg.fromEnv.updateChannel}<span class="pill env">from env</span>{/if}
        </span>
        <select class="field" bind:value={updateChannel} disabled={cfg.fromEnv.updateChannel}>
          <option value="stable">Stable</option>
          <option value="pre">Pre-release</option>
        </select>
      </label>

      <div class="divider"></div>

      <h3 class="sub-title">Daily disk cleanup</h3>
      <label class="check-row">
        <input type="checkbox" bind:checked={cleanupEnabled} />
        <span>Run daily cleanup of unused images and build cache</span>
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
        <button type="submit" class="btn primary" disabled={saving}>{saving ? 'Saving…' : 'Save'}</button>
        {#if saved}<span class="meta ok">Saved</span>{/if}
      </div>
    </form>

    <section class="card card-pad metrics-box">
      <div class="head-row">
        <div>
          <h2 class="section-title">Prometheus metrics</h2>
          <div class="meta mono">{cfg.metricsUrl}</div>
        </div>
        {#if cfg.fromEnv.metricsToken}
          <span class="pill env">from env</span>
        {:else if cfg.metricsTokenSet}
          <span class="pill on">Enabled</span>
        {:else}
          <span class="pill">Off</span>
        {/if}
      </div>

      {#if genToken}
        <div class="token-box">
          <code class="mono">{genToken}</code>
          <button type="button" class="btn" onclick={() => copy(genToken)}>{copied ? 'Copied' : 'Copy'}</button>
        </div>
      {/if}

      {#if !cfg.fromEnv.metricsToken}
        <div class="actions">
          <button type="button" class="btn" onclick={generateToken}>
            {cfg.metricsTokenSet ? 'Rotate token' : 'Generate token & enable'}
          </button>
          {#if cfg.metricsTokenSet}
            <button type="button" class="btn danger" onclick={clearToken}>Disable</button>
          {/if}
        </div>
      {/if}
    </section>

    <section class="card card-pad backup">
      <h2 class="section-title">Backup &amp; restore</h2>
      <p class="meta">
        A backup holds the hub database (users, checks, channels, tokens), every stack's compose.yaml and .env and the
        stack history of each connected host, encrypted with the password you set here. Without the password the file
        can't be read. App data in Docker volumes isn't included.
      </p>

      <form class="bk-row" onsubmit={doExport}>
        <input class="field" type="password" bind:value={exportPw} placeholder="Backup password (8+ characters)" autocomplete="new-password" minlength="8" required />
        <input class="field" type="password" bind:value={exportPw2} placeholder="Repeat password" autocomplete="new-password" minlength="8" required />
        <button type="submit" class="btn" disabled={exporting}>{exporting ? 'Exporting…' : 'Export backup'}</button>
      </form>

      <div class="divider"></div>

      {#if !preview}
        <form class="bk-row" onsubmit={doPreview}>
          <input class="field" type="file" accept=".mbk,application/octet-stream" required
            onchange={(e) => (importFile = e.currentTarget.files?.[0] ?? null)} />
          <input class="field" type="password" bind:value={importPw} placeholder="Backup password" autocomplete="off" required />
          <button type="submit" class="btn" disabled={importing || !importFile}>{importing ? 'Checking…' : 'Check backup'}</button>
        </form>
      {:else}
        <div class="bk-preview">
          <p>
            Backup from <b>{new Date(preview.exportedAt).toLocaleString()}</b> (Moorage {preview.version || 'unknown'}):
            {preview.database.users} users, {preview.database.checks} checks, {preview.database.channels} channels,
            {preview.database.agents} agents, {preview.database.statusPages} status pages.
          </p>
          {#if preview.skipped?.length}
            <p class="meta">Not in the backup: {preview.skipped.join(', ')}</p>
          {/if}

          <label class="check-row align-top">
            <input type="checkbox" bind:checked={restoreDb} />
            <span>Replace the hub database (users, checks, channels, settings). Takes effect after a restart and ends all sessions.</span>
          </label>
          <label class="check-row align-top">
            <input type="checkbox" bind:checked={restoreStacks} />
            <span>Restore stack definitions now{replaced ? ` (${replaced} existing ${replaced === 1 ? 'stack is' : 'stacks are'} overwritten; their history keeps the old version)` : ''}. Nothing is started.</span>
          </label>

          <ul class="bk-hosts">
            {#each preview.hosts as h (h.id)}
              <li>
                <b>{h.name}</b>
                {#if !h.known}<span class="tag">not on this hub</span>{:else if !h.writable}<span class="tag">offline or read-only</span>{/if}
                <span class="meta">
                  {#each h.stacks as st, i (st.name)}{i ? ', ' : ''}<span class:over={st.exists && h.writable} title={st.exists ? 'Overwritten' : 'New'}>{st.name}</span>{/each}
                  {#if !h.stacks.length}no stacks{/if}
                </span>
              </li>
            {/each}
          </ul>
          {#if unavailable.length && restoreStacks}
            <p class="meta">Stacks on {unavailable.map((h) => h.name).join(', ')} are skipped: the host isn't connected and writable.</p>
          {/if}
          <p class="meta">Stack histories stay in the backup file for recovery by hand; they aren't imported.</p>

          <div class="actions">
            <button type="button" class="btn btn-primary" disabled={applying || (!restoreDb && !restoreStacks)} onclick={doApply}>
              {applying ? 'Restoring…' : 'Restore'}
            </button>
            <button type="button" class="btn" disabled={applying} onclick={() => (preview = null)}>Cancel</button>
          </div>
        </div>
      {/if}
      {#if importMsg}<p class="meta ok">{importMsg}</p>{/if}
      {#if backupErr}<p class="error-text" role="alert">{backupErr}</p>{/if}
    </section>

    <section class="card card-pad locks">
      <h2 class="section-title">Environment locks</h2>
      <div class="lock-grid">
        <span class="lock">Read-only: <b>{cfg.envLocks.readOnly ? 'On' : 'Off'}</b></span>
        <span class="lock">Terminal: <b>{cfg.envLocks.terminal ? 'On' : 'Off'}</b></span>
        <span class="lock">Trusted proxies: <b>{cfg.envLocks.trustedProxies ? 'Configured' : 'Off'}</b></span>
        <span class="lock">OIDC: <b>{cfg.envLocks.oidc ? 'Configured' : 'Off'}</b></span>
        <span class="lock">Self-update: <b>{cfg.envLocks.selfUpdate ? 'On' : 'Off'}</b></span>
      </div>
    </section>
  {/if}
</div>

<style>
  .page {
    display: grid;
    gap: 12px;
  }
  .form {
    display: grid;
    gap: 12px;
    max-width: 680px;
  }
  .field-wrap {
    display: grid;
    gap: 5px;
    font-size: 13px;
    color: var(--mist);
  }
  .label-row {
    display: flex;
    align-items: center;
    gap: 8px;
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
    grid-template-columns: 140px 1fr;
    gap: 12px;
  }
  .divider {
    height: 1px;
    background: var(--rope);
    margin: 4px 0;
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
    color: var(--afloat);
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
  .metrics-box {
    display: grid;
    gap: 10px;
    max-width: 680px;
  }
  .head-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  .token-box {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    background: var(--well);
    font-size: 12px;
    word-break: break-all;
  }
  .pill {
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .pill.on {
    background: color-mix(in srgb, var(--afloat) 16%, transparent);
    color: var(--afloat);
  }
  .pill.env {
    background: var(--buoy-wash);
    color: var(--buoy-text);
  }
  .backup {
    display: grid;
    gap: 12px;
  }
  .bk-row {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: center;
  }
  .bk-row .field {
    flex: 1 1 200px;
    min-width: 0;
  }
  .bk-preview {
    display: grid;
    gap: 10px;
  }
  .bk-hosts {
    display: grid;
    gap: 4px;
    margin: 0;
    padding-left: 18px;
  }
  .over {
    color: var(--signal-text);
  }
  .align-top {
    align-items: flex-start;
  }
  .align-top input {
    margin-top: 3px;
  }
  .tag {
    display: inline-block;
    margin: 0 6px;
    padding: 1px 8px;
    border-radius: 999px;
    background: var(--well);
    color: var(--haze);
    font-size: 11px;
  }
  .locks {
    max-width: 680px;
    display: grid;
    gap: 8px;
  }
  .lock-grid {
    display: flex;
    flex-wrap: wrap;
    gap: 12px 20px;
    font-size: 12px;
    color: var(--mist);
  }
  .lock b {
    color: var(--foam);
    font-weight: 500;
  }
</style>
