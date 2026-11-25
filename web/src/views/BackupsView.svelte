<script lang="ts">
  import { untrack } from 'svelte';
  import { api, formatBytes, type ImportPreview, type RemoteBackupConfig } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';
  import AddonGate from '../lib/AddonGate.svelte';
  import SetupHint from '../lib/SetupHint.svelte';

  // Local encrypted export / import (.mbk)
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

  // Remote S3 / Garage / R2 / WebDAV scheduled backups
  let remote = $state<RemoteBackupConfig | null>(null);
  let remErr = $state('');
  let remSaved = $state(false);
  let remSaving = $state(false);
  let remRunning = $state(false);
  let remMsg = $state('');

  let provider = $state<'off' | 's3' | 'webdav'>('off');
  let enabled = $state(false);
  let schedule = $state('04:00');
  let retention = $state(7);
  let backupPassword = $state('');

  let s3Endpoint = $state('');
  let s3Bucket = $state('');
  let s3Region = $state('auto');
  let s3AccessKey = $state('');
  let s3SecretKey = $state('');
  let s3Prefix = $state('moorage/');
  let s3AllowPrivate = $state(false);

  let webdavUrl = $state('');
  let webdavUsername = $state('');
  let webdavPassword = $state('');
  let webdavAllowPrivate = $state(false);

  async function loadRemote() {
    try {
      const r = await api.remoteBackup();
      remote = r;
      provider = r.provider;
      enabled = r.enabled;
      schedule = r.schedule || '04:00';
      retention = r.retention || 7;
      s3Endpoint = r.s3Endpoint;
      s3Bucket = r.s3Bucket;
      s3Region = r.s3Region || 'auto';
      s3AccessKey = r.s3AccessKey;
      s3Prefix = r.s3Prefix;
      s3AllowPrivate = r.s3AllowPrivate;
      webdavUrl = r.webdavUrl;
      webdavUsername = r.webdavUsername;
      webdavAllowPrivate = r.webdavAllowPrivate;
      backupPassword = '';
      s3SecretKey = '';
      webdavPassword = '';
      remErr = '';
    } catch (e) {
      remErr = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => void loadRemote());
  });

  async function saveRemote(e: SubmitEvent) {
    e.preventDefault();
    remSaving = true;
    remSaved = false;
    remErr = '';
    remMsg = '';
    try {
      const r = await api.saveRemoteBackup({
        enabled,
        provider,
        schedule,
        retention: Number(retention) || 7,
        backupPassword,
        s3Endpoint,
        s3Bucket,
        s3Region,
        s3AccessKey,
        s3SecretKey,
        s3Prefix,
        s3AllowPrivate,
        webdavUrl,
        webdavUsername,
        webdavPassword,
        webdavAllowPrivate,
      });
      remote = r;
      backupPassword = '';
      s3SecretKey = '';
      webdavPassword = '';
      remSaved = true;
      setTimeout(() => (remSaved = false), 2500);
    } catch (err) {
      remErr = (err as Error).message;
    } finally {
      remSaving = false;
    }
  }

  async function backupAndTestNow() {
    remRunning = true;
    remErr = '';
    remMsg = '';
    try {
      // Save current form changes first so test uses what the admin typed
      await api.saveRemoteBackup({
        enabled,
        provider,
        schedule,
        retention: Number(retention) || 7,
        backupPassword,
        s3Endpoint,
        s3Bucket,
        s3Region,
        s3AccessKey,
        s3SecretKey,
        s3Prefix,
        s3AllowPrivate,
        webdavUrl,
        webdavUsername,
        webdavPassword,
        webdavAllowPrivate,
      });
      backupPassword = '';
      s3SecretKey = '';
      webdavPassword = '';
      const st = await api.runRemoteBackupNow();
      await loadRemote();
      remMsg = `Uploaded ${formatBytes(st.sizeBytes ?? 0)} to ${st.target ?? provider}${st.pruned ? ` · pruned ${st.pruned} older` : ''}.`;
    } catch (err) {
      remErr = (err as Error).message;
      await loadRemote();
    } finally {
      remRunning = false;
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
  <section class="card card-pad">
    <h1 class="title">Backups</h1>
    <p class="meta">
      Encrypted <code>.mbk</code> archives contain the hub database (users, checks, channels, connections, shared variables), every managed stack's
      <code>compose.yaml</code> + <code>.env</code>, and host <code>history.git</code>. Application Docker volumes are backed up separately via Restic.
    </p>
  </section>

  <section class="card card-pad block">
    <h2 class="section-title">Manual export &amp; restore</h2>

    <form class="bk-row" onsubmit={doExport}>
      <input class="field" type="password" bind:value={exportPw} placeholder="Backup password (8+ characters)" autocomplete="new-password" minlength="8" required />
      <input class="field" type="password" bind:value={exportPw2} placeholder="Repeat password" autocomplete="new-password" minlength="8" required />
      <button type="submit" class="btn" disabled={exporting}><Icon name="download" /> {exporting ? 'Exporting…' : 'Export .mbk'}</button>
    </form>

    <div class="divider"></div>

    {#if !preview}
      <form class="bk-row" onsubmit={doPreview}>
        <label class="file-pick field" title={importFile ? importFile.name : 'Choose a .mbk backup archive'}>
          <input
            type="file"
            class="sr-only"
            accept=".mbk,application/octet-stream"
            required
            onchange={(e) => (importFile = e.currentTarget.files?.[0] ?? null)}
          />
          <span class="btn btn-ghost pick-btn"><Icon name="upload" /> Choose .mbk file</span>
          <span class="file-name" class:empty={!importFile}>{importFile ? importFile.name : 'No file selected'}</span>
        </label>
        <input class="field" type="password" bind:value={importPw} placeholder="Backup password" autocomplete="off" required />
        <button type="submit" class="btn" disabled={importing || !importFile}><Icon name="upload" /> {importing ? 'Checking…' : 'Check & preview'}</button>
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
              {#if !h.known}<span class="pill">not on this hub</span>{:else if !h.writable}<span class="pill">offline or read-only</span>{/if}
              <span class="meta">
                {#each h.stacks as st, i (st.name)}{i ? ', ' : ''}<span class:over={st.exists && h.writable}>{st.name}</span>{/each}
                {#if !h.stacks.length}no stacks{/if}
              </span>
            </li>
          {/each}
        </ul>
        {#if unavailable.length && restoreStacks}
          <p class="meta">Stacks on {unavailable.map((h) => h.name).join(', ')} are skipped: the host isn't connected and writable.</p>
        {/if}

        <div class="actions">
          <button type="button" class="btn btn-primary" disabled={applying || (!restoreDb && !restoreStacks)} onclick={doApply}>
            {applying ? 'Restoring…' : 'Restore selected'}
          </button>
          <button type="button" class="btn" disabled={applying} onclick={() => (preview = null)}>Cancel</button>
        </div>
      </div>
    {/if}
    {#if importMsg}<p class="meta ok"><Icon name="check" /> {importMsg}</p>{/if}
    {#if backupErr}<p class="error-text" role="alert">{backupErr}</p>{/if}
  </section>

  {#if remote}
    <form class="card card-pad block" onsubmit={saveRemote}>
      <div class="head-row">
        <div>
          <h2 class="section-title">Scheduled remote encrypted backups</h2>
          <p class="meta">Automatically encrypts a fresh <code>.mbk</code> locally with <code>backupcrypt</code> and uploads it to S3 / Garage / R2 or WebDAV.</p>
        </div>
        {#if remote.last}
          <span class="pill {remote.last.ok ? 'on' : 'bad-pill'}">
            {remote.last.ok ? `Last OK · ${new Date(remote.last.at).toLocaleString()}` : `Last failed · ${new Date(remote.last.at).toLocaleString()}`}
          </span>
        {/if}
      </div>

      <div class="grid-3">
        <div class="field-wrap">
          <span>Storage provider</span>
          <Select
            label="Storage provider"
            value={provider}
            disabled={remote.fromEnv.s3Endpoint || remote.fromEnv.webdavUrl}
            items={[
              { value: 'off', label: 'Disabled' },
              { value: 's3', label: 'S3 / Garage / Cloudflare R2 / MinIO' },
              { value: 'webdav', label: 'WebDAV (Nextcloud / Hetzner Storage Box)' },
            ]}
            onchange={(v) => (provider = v as 'off' | 's3' | 'webdav')}
          />
        </div>

        <label class="field-wrap">
          <span class="label-row">
            <span>Daily schedule (HH:MM)</span>
            {#if remote.fromEnv.schedule}<span class="pill env">Set via environment</span>{/if}
          </span>
          <input class="field mono" bind:value={schedule} placeholder="04:00" maxlength="5" disabled={remote.fromEnv.schedule} />
        </label>

        <label class="field-wrap">
          <span class="label-row">
            <span>Keep last N backups</span>
            {#if remote.fromEnv.retention}<span class="pill env">Set via environment</span>{/if}
          </span>
          <input class="field mono" type="number" min="1" max="365" bind:value={retention} disabled={remote.fromEnv.retention} />
        </label>
      </div>

      {#if provider !== 'off'}
        <label class="check-row">
          <input type="checkbox" bind:checked={enabled} />
          <span>Run automatic daily remote backups at {schedule}</span>
        </label>

        <label class="field-wrap">
          <span class="label-row">
            <span>Encryption password</span>
            {#if remote.fromEnv.backupPassword}<span class="pill env">Set via environment</span>{:else if remote.backupPasswordSet}<span class="pill on">Set (********)</span>{/if}
          </span>
          <input
            class="field"
            type="password"
            bind:value={backupPassword}
            placeholder={remote.backupPasswordSet ? '******** (leave empty to keep existing)' : '8+ characters'}
            disabled={remote.fromEnv.backupPassword}
            autocomplete="new-password"
          />
        </label>

        {#if provider === 's3'}
          <div class="grid-2">
            <label class="field-wrap">
              <span class="label-row"><span>S3 Endpoint URL</span>{#if remote.fromEnv.s3Endpoint}<span class="pill env">Set via environment</span>{/if}</span>
              <input class="field mono" bind:value={s3Endpoint} placeholder="https://s3.eu-central-1.amazonaws.com" disabled={remote.fromEnv.s3Endpoint} />
            </label>
            <label class="field-wrap">
              <span class="label-row"><span>Bucket</span>{#if remote.fromEnv.s3Bucket}<span class="pill env">Set via environment</span>{/if}</span>
              <input class="field mono" bind:value={s3Bucket} placeholder="moorage-backups" disabled={remote.fromEnv.s3Bucket} />
            </label>
            <label class="field-wrap">
              <span class="label-row"><span>Region</span>{#if remote.fromEnv.s3Region}<span class="pill env">Set via environment</span>{/if}</span>
              <input class="field mono" bind:value={s3Region} placeholder="auto" disabled={remote.fromEnv.s3Region} />
            </label>
            <label class="field-wrap">
              <span class="label-row"><span>Key prefix</span>{#if remote.fromEnv.s3Prefix}<span class="pill env">Set via environment</span>{/if}</span>
              <input class="field mono" bind:value={s3Prefix} placeholder="moorage/" disabled={remote.fromEnv.s3Prefix} />
            </label>
            <label class="field-wrap">
              <span class="label-row"><span>Access Key ID</span>{#if remote.fromEnv.s3AccessKey}<span class="pill env">Set via environment</span>{/if}</span>
              <input class="field mono" bind:value={s3AccessKey} disabled={remote.fromEnv.s3AccessKey} autocomplete="off" />
            </label>
            <label class="field-wrap">
              <span class="label-row">
                <span>Secret Access Key</span>
                {#if remote.fromEnv.s3SecretKey}<span class="pill env">Set via environment</span>{:else if remote.s3SecretKeySet}<span class="pill on">Set (********)</span>{/if}
              </span>
              <input
                class="field mono"
                type="password"
                bind:value={s3SecretKey}
                placeholder={remote.s3SecretKeySet ? '********' : ''}
                disabled={remote.fromEnv.s3SecretKey}
                autocomplete="new-password"
              />
            </label>
          </div>
          <label class="check-row">
            <input type="checkbox" bind:checked={s3AllowPrivate} />
            <span>Allow private / Tailscale IP addresses for self-hosted S3 (Garage / MinIO)</span>
          </label>
        {:else if provider === 'webdav'}
          <div class="grid-3">
            <label class="field-wrap">
              <span class="label-row"><span>WebDAV folder URL</span>{#if remote.fromEnv.webdavUrl}<span class="pill env">Set via environment</span>{/if}</span>
              <input class="field mono" bind:value={webdavUrl} placeholder="https://cloud.example.com/remote.php/dav/files/user/moorage" disabled={remote.fromEnv.webdavUrl} />
            </label>
            <label class="field-wrap">
              <span class="label-row"><span>Username</span>{#if remote.fromEnv.webdavUsername}<span class="pill env">Set via environment</span>{/if}</span>
              <input class="field" bind:value={webdavUsername} disabled={remote.fromEnv.webdavUsername} autocomplete="off" />
            </label>
            <label class="field-wrap">
              <span class="label-row">
                <span>Password / App token</span>
                {#if remote.fromEnv.webdavPassword}<span class="pill env">Set via environment</span>{:else if remote.webdavPasswordSet}<span class="pill on">Set (********)</span>{/if}
              </span>
              <input
                class="field"
                type="password"
                bind:value={webdavPassword}
                placeholder={remote.webdavPasswordSet ? '********' : ''}
                disabled={remote.fromEnv.webdavPassword}
                autocomplete="new-password"
              />
            </label>
          </div>
          <label class="check-row">
            <input type="checkbox" bind:checked={webdavAllowPrivate} />
            <span>Allow private / Tailscale IP addresses for self-hosted WebDAV</span>
          </label>
        {/if}
      {/if}

      {#if remote.last && !remote.last.ok && remote.last.error}
        <p class="error-text" role="alert">Last run error: {remote.last.error}</p>
      {/if}
      {#if remErr}<p class="error-text" role="alert">{remErr}</p>{/if}
      {#if remMsg}<p class="meta ok"><Icon name="check" /> {remMsg}</p>{/if}

      <div class="actions">
        <button type="submit" class="btn btn-primary" disabled={remSaving}>{remSaving ? 'Saving…' : 'Save backup settings'}</button>
        {#if provider !== 'off'}
          <button type="button" class="btn" disabled={remRunning || remSaving} onclick={backupAndTestNow}>
            <Icon name="upload" /> {remRunning ? 'Uploading backup…' : 'Backup & test now'}
          </button>
        {/if}
        {#if remSaved}<span class="meta ok"><Icon name="check" /> Saved</span>{/if}
      </div>
    </form>
  {/if}

  <AddonGate
    addonId="restic-backup"
    featureTitle="Application volume backups (Restic)"
    featureDescription="Back up named Docker volumes on schedule to S3, B2, SFTP, or local storage."
  >
    <SetupHint
      title="Restic volume backup sidecar is enabled"
      detail="Configure repository, schedule, and volume targets directly in Settings → Add-ons → Restic Backup."
      actionLabel="Open Restic Add-on"
      actionHref="#/settings/addons"
    />
  </AddonGate>
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .block {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    min-width: 0;
    gap: 14px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
    flex-wrap: wrap;
  }
  .grid-2 {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
    gap: 12px 16px;
  }
  .grid-3 {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(210px, 1fr));
    gap: 12px 16px;
  }
  .field-wrap {
    display: grid;
    gap: 5px;
    font-size: 13px;
    color: var(--mist);
    align-content: start;
    min-width: 0;
  }
  .label-row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .bk-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) auto;
    gap: 8px;
    align-items: center;
    min-width: 0;
  }
  .bk-row .field {
    width: 100%;
    min-width: 0;
  }
  @media (max-width: 720px) {
    .bk-row {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .file-pick {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 2px 10px 2px 3px;
    min-height: 36px;
    cursor: pointer;
    overflow: hidden;
  }
  .pick-btn {
    height: 28px;
    padding: 0 10px;
    font-size: 12.5px;
    flex-shrink: 0;
  }
  .file-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: 13px;
    color: var(--foam);
  }
  .file-name.empty {
    color: var(--haze);
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
  .check-row {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    color: var(--foam);
    cursor: pointer;
  }
  .align-top {
    align-items: flex-start;
  }
  .align-top input {
    margin-top: 3px;
  }
  .divider {
    height: 1px;
    background: var(--rope);
    margin: 2px 0;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .ok {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: var(--afloat-text);
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
  .pill.bad-pill {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .pill.env {
    background: var(--buoy-wash);
    color: var(--buoy-text);
  }
</style>
