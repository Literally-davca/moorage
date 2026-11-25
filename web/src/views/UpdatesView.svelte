<script lang="ts">
  import { untrack } from 'svelte';
  import {
    api,
    type Channel,
    type ImageCheckRow,
    type ImagePolicy,
    type ImageStatus,
    type ImageUpdate,
    type StackImages,
    type UpdateSettings,
  } from '../lib/api';
  import { isAdmin, stackLabel } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';

  let settings = $state<UpdateSettings | null>(null);
  let stacks = $state<StackImages[] | null>(null);
  let updates = $state<ImageUpdate[]>([]);
  let channels = $state<Channel[]>([]);
  let error = $state('');
  let savingSettings = $state(false);
  let checkingAll = $state(false);
  let settingsMsg = $state<{ ok: boolean; text: string } | null>(null);

  const intervals = [
    { v: 3600, label: 'Every 1 hour' },
    { v: 10800, label: 'Every 3 hours' },
    { v: 21600, label: 'Every 6 hours' },
    { v: 43200, label: 'Every 12 hours' },
    { v: 86400, label: 'Every 24 hours' },
    { v: 604800, label: 'Every 7 days' },
  ];

  async function load() {
    try {
      const res = await api.updates();
      if (!settings) {
        settings = res.settings;
      } else {
        settings.timezone = res.settings.timezone;
      }
      stacks = res.stacks;
      updates = res.updates ?? [];
      if (isAdmin() && channels.length === 0) {
        channels = await api.channels().catch(() => []);
      }
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => {
      void load();
    });
  });

  async function saveSettings(e: SubmitEvent) {
    e.preventDefault();
    if (!settings) return;
    savingSettings = true;
    settingsMsg = null;
    try {
      settings = await api.saveUpdateSettings({
        enabled: settings.enabled,
        interval: Number(settings.interval),
        windowStart: settings.windowStart,
        windowEnd: settings.windowEnd,
        channels: settings.channels,
      });
      settingsMsg = { ok: true, text: 'Update settings saved.' };
    } catch (err) {
      settingsMsg = { ok: false, text: (err as Error).message };
    } finally {
      savingSettings = false;
    }
  }

  function toggleChannel(id: number) {
    if (!settings) return;
    if (settings.channels.includes(id)) {
      settings.channels = settings.channels.filter((c) => c !== id);
    } else {
      settings.channels = [...settings.channels, id];
    }
  }

  async function checkAllNow() {
    checkingAll = true;
    error = '';
    try {
      await api.checkAllUpdates();
      const res = await api.updates();
      stacks = res.stacks;
      updates = res.updates ?? [];
    } catch (e) {
      error = (e as Error).message;
    } finally {
      checkingAll = false;
    }
  }

  async function changePolicy(st: StackImages, policy: ImagePolicy) {
    try {
      await api.saveStackImagePolicy(st.stack, policy, st.hostId);
      st.policy = policy;
    } catch (e) {
      error = (e as Error).message;
    }
  }

  function statusText(s: ImageCheckRow): string {
    switch (s.status) {
      case 'up-to-date':
        return 'Up to date';
      case 'update-available':
        return 'Update available';
      case 'pulled-not-applied':
        return 'Pulled, not applied';
      case 'pinned':
        return 'Pinned digest';
      case 'built':
        return 'Built locally';
      case 'local-only':
        return 'Local image only';
      case 'unsupported':
        return s.error || 'Update the agent to check images';
      case 'unreachable':
        return s.error ? `Unreachable (${s.error})` : 'Unreachable';
      default:
        return s.status || 'Not checked yet';
    }
  }

  function badgeClass(status: ImageStatus): string {
    switch (status) {
      case 'up-to-date':
        return 'ok';
      case 'update-available':
      case 'pulled-not-applied':
        return 'warn';
      case 'unreachable':
      case 'unsupported':
        return 'bad';
      default:
        return 'neutral';
    }
  }

  function resultLabel(r: ImageUpdate['result']): string {
    switch (r) {
      case 'ok':
        return 'Updated';
      case 'pull-failed':
        return 'Pull failed';
      case 'up-failed':
        return 'Up failed';
      case 'rolled-back':
        return 'Rolled back';
    }
  }

  function shortDigest(d?: string): string {
    if (!d) return '';
    const clean = d.replace(/^sha256:/, '');
    return clean.slice(0, 7);
  }

  function fmtTime(iso?: string): string {
    if (!iso || iso.startsWith('0001-')) return 'Never';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return 'Never';
    return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  }

  const groupedByHost = $derived.by(() => {
    if (!stacks) return [];
    const map = new Map<string, { hostId: string; hostName: string; readOnly: boolean; list: StackImages[] }>();
    for (const s of stacks) {
      let g = map.get(s.hostId);
      if (!g) {
        g = { hostId: s.hostId, hostName: s.hostName || s.hostId, readOnly: s.readOnly, list: [] };
        map.set(s.hostId, g);
      }
      g.list.push(s);
    }
    return Array.from(map.values());
  });
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Image updates</h1>
      <p class="meta">
        Check compose services for rebuilt image digests and newer version tags. Auto-update applies same-tag digest
        updates inside the apply window; version tag changes are notify-only.
      </p>
    </div>
    {#if isAdmin()}
      <button type="button" class="btn" onclick={checkAllNow} disabled={checkingAll} aria-busy={checkingAll}>
        <Icon name="refresh" /> <span>{checkingAll ? 'Checking…' : 'Check all now'}</span>
      </button>
    {/if}
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if settings}
    <section class="card card-pad">
      <h2 class="section-title">Schedule &amp; apply window</h2>
      {#if isAdmin()}
        <form class="settings-form" onsubmit={saveSettings}>
          <label class="check-row">
            <input type="checkbox" bind:checked={settings.enabled} />
            <span>Check for image updates automatically</span>
          </label>

          <div class="grid-3">
            <div>
              <span class="label">Check interval</span>
              <Select
                label="Check interval"
                value={String(settings.interval)}
                items={intervals.map((it) => ({ value: String(it.v), label: it.label }))}
                onchange={(v) => {
                  if (settings) settings.interval = Number(v);
                }}
              />
            </div>
            <div>
              <label class="label" for="upd-start">Apply window start</label>
              <input id="upd-start" class="field mono" placeholder="03:00" maxlength="5" bind:value={settings.windowStart} required />
            </div>
            <div>
              <label class="label" for="upd-end">Apply window end</label>
              <input id="upd-end" class="field mono" placeholder="05:00" maxlength="5" bind:value={settings.windowEnd} required />
            </div>
          </div>

          <p class="hint">
            Hub time zone: <strong class="mono">{settings.timezone}</strong>. Stacks set to <strong>Auto</strong> pull and
            recreate containers only during this window (at most once per window).
          </p>

          {#if channels.length > 0}
            <div>
              <span class="label">Notification channels</span>
              <MultiSelect
                label="Notification channels"
                placeholder="No channels selected"
                items={channels.map((ch) => ({
                  value: String(ch.id),
                  label: ch.name,
                  meta: ch.enabled ? 'enabled' : 'disabled',
                }))}
                selected={settings.channels.map(String)}
                onchange={(next) => {
                  if (settings) settings.channels = next.map(Number);
                }}
              />
            </div>
          {:else}
            <p class="hint">
              No notification channels configured yet. <a href="#/notifications">Add a channel</a> to receive update alerts.
            </p>
          {/if}

          {#if settingsMsg}
            <div class="msg {settingsMsg.ok ? 'ok' : 'bad'}" role="status">
              <Icon name={settingsMsg.ok ? 'check' : 'alert'} />
              <span>{settingsMsg.text}</span>
            </div>
          {/if}

          <div class="actions">
            <button class="btn btn-primary" disabled={savingSettings} aria-busy={savingSettings}>Save settings</button>
          </div>
        </form>
      {:else}
        <p class="meta" style="margin-top:8px">
          {settings.enabled ? 'Enabled' : 'Disabled'} · interval {Math.round(settings.interval / 3600)} h · apply window
          <span class="mono">{settings.windowStart}–{settings.windowEnd}</span> ({settings.timezone})
        </p>
      {/if}
    </section>
  {/if}

  {#if stacks === null}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:50%"></div></section>
  {:else if stacks.length === 0}
    <section class="card card-pad">
      <p class="section-title">No stacks found</p>
      <p class="meta">Deploy a compose stack first to track its images.</p>
    </section>
  {:else}
    {#each groupedByHost as group (group.hostId)}
      <section class="card list-card" aria-label="Stacks on {group.hostName}">
        <header class="host-head">
          <span class="section-title">{group.hostName}</span>
          {#if group.readOnly}<span class="badge neutral">Read-only host</span>{/if}
        </header>

        {#each group.list as st (st.stack)}
          <div class="stack-block">
            <div class="stack-top">
              <div class="stack-title-wrap">
                <span class="dot {st.status}"></span>
                <a class="stack-link" href={`#/h/${encodeURIComponent(st.hostId)}/stacks/${encodeURIComponent(st.stack)}`}>
                  {st.displayName || stackLabel(st.hostId, st.stack)}
                </a>
                {#if (st.displayName || stackLabel(st.hostId, st.stack)) !== st.stack}
                  <span class="meta mono">{st.coolifyProject ? `${st.coolifyProject} · ` : ''}{st.stack}</span>
                {/if}
                {#if !st.managed}
                  <span class="badge neutral" title="Lives outside the Moorage stacks folder">Unmanaged</span>
                {/if}
                <span class="meta sub-time">Checked: {fmtTime(st.checkedAt)}</span>
              </div>

              <div class="policy-wrap">
                {#if isAdmin()}
                  <div class="seg" role="group" aria-label="Update policy for {st.stack}">
                    <button
                      type="button"
                      class:active={st.policy === 'off'}
                      onclick={() => changePolicy(st, 'off')}
                    >Off</button>
                    <button
                      type="button"
                      class:active={st.policy === 'notify'}
                      onclick={() => changePolicy(st, 'notify')}
                    >Notify</button>
                    <button
                      type="button"
                      class:active={st.policy === 'auto'}
                      disabled={Boolean(st.autoReason)}
                      title={st.autoReason || 'Auto-apply same-tag digest updates inside the apply window'}
                      onclick={() => changePolicy(st, 'auto')}
                    >Auto</button>
                  </div>
                {:else}
                  <span class="badge neutral">Policy: {st.policy}</span>
                {/if}
              </div>
            </div>

            {#if st.services.length === 0}
              <p class="meta svc-empty">Not checked yet.</p>
            {:else}
              <div class="svc-list">
                {#each st.services as svc (svc.service)}
                  <div class="svc-row">
                    <span class="svc-name mono">{svc.service}</span>
                    <span class="svc-ref mono">{svc.ref || '—'}</span>
                    <span class="badge {badgeClass(svc.status)}">{statusText(svc)}</span>
                    {#if svc.localDigest || svc.remoteDigest}
                      <span class="meta mono digest">
                        {#if svc.localDigest && svc.remoteDigest && svc.localDigest !== svc.remoteDigest}
                          {shortDigest(svc.localDigest)} → {shortDigest(svc.remoteDigest)}
                        {:else}
                          {shortDigest(svc.remoteDigest || svc.localDigest)}
                        {/if}
                      </span>
                    {/if}
                    {#if svc.newerTag}
                      <span class="badge tag-cand" title="Newer version tag in same major">
                        New tag: <strong class="mono">{svc.newerTag}</strong>
                      </span>
                    {/if}
                    {#if svc.newerMajor}
                      <span class="badge major-cand" title="Newer major version tag">
                        Major: <strong class="mono">{svc.newerMajor}</strong>
                      </span>
                    {/if}
                  </div>
                {/each}
              </div>
            {/if}

            {#if st.lastUpdate}
              <div class="last-upd meta">
                <span>
                  Last update: <strong>{resultLabel(st.lastUpdate.result)}</strong> by
                  <span class="mono">{st.lastUpdate.actor}</span> · {fmtTime(st.lastUpdate.startedAt)}
                </span>
              </div>
            {/if}
          </div>
        {/each}
      </section>
    {/each}
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 16px;
    flex-wrap: wrap;
  }
  .head .meta {
    max-width: 68ch;
    margin-top: 4px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .settings-form {
    display: grid;
    gap: 14px;
    margin-top: 12px;
  }
  .check-row {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    font-size: 14px;
    cursor: pointer;
  }
  .grid-3 {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 12px;
    max-width: 640px;
  }
  .ch-pills {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-top: 6px;
  }
  .pill {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    height: 32px;
    padding: 0 12px;
    border-radius: 999px;
    border: 1px solid var(--rope);
    background: var(--well);
    color: var(--mist);
    font: 500 13px var(--font);
    cursor: pointer;
  }
  .pill.on {
    border-color: var(--buoy);
    background: var(--buoy-wash);
    color: var(--foam);
  }
  .msg {
    display: flex;
    gap: 10px;
    align-items: center;
    padding: 10px 14px;
    border-radius: var(--r-md);
    font-size: 13px;
  }
  .msg.ok {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .msg.bad {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .actions {
    display: flex;
    gap: 8px;
  }
  .list-card {
    padding: 0;
    overflow: hidden;
  }
  .host-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 14px 20px;
    border-bottom: 1px solid var(--rope);
    background: var(--well);
  }
  .stack-block {
    padding: 14px 20px;
    display: grid;
    gap: 10px;
  }
  .stack-block + .stack-block {
    border-top: 1px solid var(--rope);
  }
  .stack-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
  }
  .stack-title-wrap {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .stack-link {
    font-weight: 600;
    font-size: 15px;
    color: var(--foam);
    text-decoration: none;
  }
  .stack-link:hover {
    text-decoration: underline;
  }
  .sub-time {
    font-size: 12px;
  }
  .seg {
    display: inline-flex;
    background: var(--well);
    border-radius: var(--r-md);
    padding: 2px;
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .seg button {
    border: 0;
    background: transparent;
    color: var(--mist);
    font: 500 12px var(--font);
    padding: 4px 10px;
    border-radius: var(--r-sm);
    cursor: pointer;
  }
  .seg button.active {
    background: var(--buoy-wash);
    color: var(--foam);
  }
  .seg button:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
  .svc-list {
    display: grid;
    gap: 6px;
    padding-left: 18px;
  }
  .svc-row {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    font-size: 13px;
  }
  .svc-name {
    font-weight: 500;
    color: var(--foam);
    min-width: 90px;
  }
  .svc-ref {
    color: var(--mist);
  }
  .digest {
    font-size: 12px;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
  }
  .badge.ok {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .badge.warn {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .badge.bad {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .badge.neutral {
    background: var(--rope);
    color: var(--mist);
  }
  .badge.tag-cand {
    background: var(--buoy-wash);
    color: var(--foam);
  }
  .badge.major-cand {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .last-upd {
    padding-left: 18px;
    font-size: 12px;
  }
  .svc-empty {
    padding-left: 18px;
    font-size: 12px;
  }

  @media (max-width: 767px) {
    .grid-3 {
      grid-template-columns: minmax(0, 1fr);
    }
    .stack-block {
      padding: 12px 14px;
    }
    .svc-list,
    .last-upd,
    .svc-empty {
      padding-left: 0;
    }
  }
</style>
