<script lang="ts">
  import { untrack } from 'svelte';
  import { api, engineLabel, formatBytes, type Channel, type HostEntry, type PrunePreview, type WatchdogAgent } from '../lib/api';
  import { app, isAdmin, setHost } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import SystemUpdates from '../lib/SystemUpdates.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';
  import Select from '../lib/Select.svelte';
  import TerminalPanel from './TerminalPanel.svelte';

  let { onChanged }: { onChanged: () => void } = $props();

  let hosts = $state<HostEntry[] | null>(null);
  let agentPort = $state('5081');
  let acceptsAgents = $state(true);
  let error = $state('');

  // add host
  let showAdd = $state(false);
  let newName = $state('');
  let adding = $state(false);
  let addError = $state('');
  let joined = $state<{ name: string; token: string; expires: string } | null>(null);
  let hubAddr = $state('');
  let readOnly = $state(false);
  let copied = $state('');

  // rename / network / remove
  let editing = $state<string | null>(null);
  let editName = $state('');
  let editIp = $state('');
  let editDomain = $state('');
  let removing = $state<HostEntry | null>(null);
  let confirmRemove = $state(false);
  let beszelUrl = $state('');
  let hostTermTarget = $state<HostEntry | null>(null);

  // disk cleanup
  let pruneHost = $state<HostEntry | null>(null);
  let pruneData = $state<PrunePreview | null>(null);
  let pruneLoading = $state(false);
  let pruneRunning = $state(false);
  let pruneMsg = $state('');
  let pruneError = $state('');
  let pruneOpts = $state({ images: true, containers: true, buildCache: true, volumes: false });

  async function openPrune(h: HostEntry) {
    pruneHost = h;
    pruneData = null;
    pruneMsg = '';
    pruneError = '';
    pruneLoading = true;
    try {
      pruneData = await api.prunePreview(h.id);
    } catch (e) {
      pruneError = (e as Error).message;
    } finally {
      pruneLoading = false;
    }
  }

  async function runPrune() {
    if (!pruneHost) return;
    pruneRunning = true;
    pruneError = '';
    pruneMsg = '';
    try {
      const res = await api.pruneDisk(pruneHost.id, pruneOpts);
      pruneMsg = `Reclaimed ${formatBytes(res.reclaimedBytes)}.`;
      pruneData = await api.prunePreview(pruneHost.id);
    } catch (e) {
      pruneError = (e as Error).message;
    } finally {
      pruneRunning = false;
    }
  }

  // hub watchdog
  let wdOpen = $state(false);
  let wdLoaded = $state(false);
  let wdDirty = $state(false);
  let wdEnabled = $state(false);
  let wdChannelId = $state(0);
  let wdGraceMin = $state(2);
  let wdMaintenanceUntil = $state('');
  let wdAgents = $state<WatchdogAgent[]>([]);
  let wdSelected = $state<Record<string, boolean>>({});
  let channels = $state<Channel[]>([]);
  let wdSaving = $state(false);
  let wdSaveOk = $state(false);
  let wdError = $state('');
  let wdTesting = $state(false);
  let wdTestMsg = $state('');
  let wdMaintBusy = $state(false);

  let sysUpdateAvail = $state(false);

  async function load() {
    try {
      const r = await api.hosts();
      hosts = r.hosts;
      app.hosts = r.hosts;
      agentPort = r.agentPort || '5081';
      acceptsAgents = r.acceptsAgents ?? false;
      if (!hubAddr) hubAddr = r.agentPublicAddr || `${location.hostname}:${agentPort}`;
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
    api.sisterLinks()
      .then((sl) => (beszelUrl = sl.beszelUrl))
      .catch(() => {});
    if (isAdmin()) {
      try {
        const [w, chs] = await Promise.all([
          api.watchdog(),
          api.channels(),
        ]);
        channels = chs.filter((c) => c.enabled);
        wdAgents = w.agents;
        wdMaintenanceUntil = w.maintenanceUntil && Date.parse(w.maintenanceUntil) > Date.now() ? w.maintenanceUntil : '';
        if (!wdLoaded || !wdDirty) {
          wdEnabled = w.enabled;
          wdChannelId = w.channelId || (channels[0]?.id ?? 0);
          wdGraceMin = Math.max(1, Math.round((w.grace || 120) / 60));
          wdSelected = Object.fromEntries(w.agents.map((a) => [a.id, a.participates]));
          wdLoaded = true;
        }
      } catch {
        /* watchdog is optional */
      }
    }
  }

  $effect(() => {
    untrack(load);
    const t = setInterval(() => !document.hidden && load(), 5000);
    return () => clearInterval(t);
  });

  async function add(e: SubmitEvent) {
    e.preventDefault();
    adding = true;
    addError = '';
    try {
      const r = await api.addAgent(newName.trim());
      joined = { name: newName.trim(), token: r.token, expires: r.expires };
      newName = '';
      await load();
      onChanged();
    } catch (err) {
      addError = (err as Error).message;
    } finally {
      adding = false;
    }
  }

  function startEdit(h: HostEntry) {
    editing = h.id;
    editName = h.name;
    editIp = h.hostIp ?? '';
    editDomain = h.baseDomain ?? '';
  }

  async function saveHostEdit(h: HostEntry) {
    try {
      const patch: { name?: string; hostIp?: string; baseDomain?: string } = {};
      if (h.kind === 'agent') patch.name = editName.trim();
      if (!h.hostIpFromEnv) patch.hostIp = editIp.trim();
      if (!h.baseDomainFromEnv) patch.baseDomain = editDomain.trim();
      await api.updateHost(h.id, patch);
      editing = null;
      await load();
      onChanged();
    } catch (err) {
      error = (err as Error).message;
    }
  }

  async function remove() {
    if (!removing) return;
    try {
      await api.removeHost(removing.id);
      await load();
      onChanged();
    } catch (err) {
      error = (err as Error).message;
    }
  }

  // Agents should run the hub's release; a dev build of the hub falls back to :latest.
  const agentTag = $derived(/^\d+\.\d+\.\d+$/.test(app.version ?? '') ? app.version : 'latest');

  const composeSnippet = $derived(
    joined
      ? `services:
  moorage-agent:
    image: ghcr.io/literally-davca/moorage:${agentTag}
    command: agent
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - moorage-agent:/data
      # same path inside and outside, like on the hub
      - /opt/stacks:/opt/stacks
    environment:
      MOORAGE_HUB: "${hubAddr}"
      MOORAGE_JOIN_TOKEN: "${joined.token}"${readOnly ? '\n      MOORAGE_READ_ONLY: "true"' : ''}
      # Hub watchdog (needed when 2 or more agents watch the hub):
      # MOORAGE_WATCHDOG_LISTEN: ":5082"
      # MOORAGE_WATCHDOG_ADDR: "<this-host-ip>:5082"
    # ports:
    #   - "5082:5082"

volumes:
  moorage-agent:
`
      : '',
  );

  async function copy(text: string, what: string) {
    try {
      await navigator.clipboard.writeText(text);
      copied = what;
      setTimeout(() => (copied = ''), 2000);
    } catch {
      copied = 'Select the text and copy it by hand.';
    }
  }

  function seen(h: HostEntry): string {
    if (h.connected) return 'Connected';
    if (!h.lastSeen) return 'Waiting for the agent';
    return 'Offline since ' + new Date(h.lastSeen).toLocaleString();
  }

  function fmtTermDuration() {}

  const wdSelectedCount = $derived(wdAgents.filter((a) => wdSelected[a.id]).length);
  const wdSelectedWithAddr = $derived(wdAgents.filter((a) => wdSelected[a.id] && a.addr).length);

  function agentWatchdogNote(a: WatchdogAgent): { canToggle: boolean; note: string } {
    const on = !!wdSelected[a.id];
    if (a.connected && !a.supported) {
      return { canToggle: on, note: 'Update the agent to use the watchdog' };
    }
    if (!a.connected && !a.supported) {
      return { canToggle: on, note: 'Offline · connect the agent to enable' };
    }
    if (!a.addr) {
      if (!on && (wdSelectedCount >= 1 || wdSelectedCount - wdSelectedWithAddr > 0)) {
        return { canToggle: false, note: 'No MOORAGE_WATCHDOG_ADDR (needed when 2+ agents watch)' };
      }
      return {
        canToggle: true,
        note: a.connected ? 'No peer address · can watch alone (1 agent)' : 'Offline · no peer address',
      };
    }
    return {
      canToggle: true,
      note: a.connected ? a.addr : `${a.addr} · currently offline`,
    };
  }

  async function saveWatchdog(e: SubmitEvent) {
    e.preventDefault();
    wdSaving = true;
    wdError = '';
    wdSaveOk = false;
    try {
      await api.saveWatchdog({
        enabled: wdEnabled,
        channelId: Number(wdChannelId) || 0,
        grace: Math.max(1, Math.min(60, Math.round(wdGraceMin || 2))) * 60,
        agents: wdSelected,
      });
      wdDirty = false;
      wdSaveOk = true;
      setTimeout(() => (wdSaveOk = false), 2500);
      await load();
    } catch (err) {
      wdError = (err as Error).message;
    } finally {
      wdSaving = false;
    }
  }

  async function testWatchdog() {
    wdTesting = true;
    wdTestMsg = '';
    wdError = '';
    try {
      const r = await api.testWatchdog(Number(wdChannelId) || undefined);
      wdTestMsg = `Sent via ${r.agent}.`;
    } catch (err) {
      wdError = (err as Error).message;
    } finally {
      wdTesting = false;
    }
  }

  async function setMaintenance(minutes: number) {
    wdMaintBusy = true;
    wdError = '';
    try {
      const r = await api.watchdogMaintenance(minutes);
      wdMaintenanceUntil = r.maintenanceUntil && Date.parse(r.maintenanceUntil) > Date.now() ? r.maintenanceUntil : '';
    } catch (err) {
      wdError = (err as Error).message;
    } finally {
      wdMaintBusy = false;
    }
  }
</script>

<div class="hosts">
  <section class="card card-pad head">
    <div class="head-bar">
      <h1 class="title">Hosts</h1>
      {#if isAdmin()}
        <button type="button" class="btn {showAdd || joined ? '' : 'btn-primary'}" onclick={() => (showAdd = !showAdd)}>
          <Icon name={showAdd || joined ? 'x' : 'plus'} /> {showAdd || joined ? 'Close' : 'Add host'}
        </button>
      {/if}
    </div>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  <section class="card list" aria-label="Hosts">
    {#if !hosts}
      <div class="row"><span class="skeleton" style="height:14px;width:40%"></span></div>
    {:else if hosts.length === 0}
      <p class="meta row">No hosts yet. Add the first one below.</p>
    {:else}
      {#each hosts as h (h.id)}
        <div class="row">
          <span class="dot {h.connected ? 'up' : 'offline'}"></span>
          <div class="info">
            {#if editing === h.id}
              <form class="rename" onsubmit={(e) => { e.preventDefault(); saveHostEdit(h); }}>
                {#if h.kind === 'agent'}
                  <input class="field" bind:value={editName} maxlength="40" placeholder="Host name" aria-label="Host name" />
                {/if}
                <input
                  class="field mono"
                  bind:value={editIp}
                  placeholder="Primary IP / hostname (e.g. 100.87.100.2)"
                  aria-label="Primary IP or hostname"
                  disabled={h.hostIpFromEnv}
                  title={h.hostIpFromEnv ? 'Set via MOORAGE_HOST_IP environment variable' : 'Used for container port links'}
                />
                <input
                  class="field mono"
                  bind:value={editDomain}
                  placeholder="Base domain (e.g. apps.example.com)"
                  aria-label="Base domain"
                  disabled={h.baseDomainFromEnv}
                  title={h.baseDomainFromEnv ? 'Set via MOORAGE_BASE_DOMAIN environment variable' : 'Prefills template domains on this host'}
                />
                <button class="btn btn-primary" disabled={h.kind === 'agent' && !editName.trim()}>Save</button>
                <button type="button" class="btn btn-ghost" onclick={() => (editing = null)}>Cancel</button>
              </form>
            {:else}
              <div class="name">
                <a href={'#/h/' + encodeURIComponent(h.id)} onclick={(e) => { e.preventDefault(); setHost(h.id); }}>{h.name}</a>
                {#if h.kind === 'local'}<span class="tag">This server</span>{/if}
                {#if h.readOnly}<span class="tag ro"><Icon name="lock" /> Read-only</span>{/if}
                {#if h.terminal && !h.readOnly}<span class="tag"><Icon name="terminal" /> Terminal</span>{/if}
                {#if beszelUrl}
                  <a class="ext-link" href={beszelUrl} target="_blank" rel="noopener noreferrer">Open in Beszel <Icon name="external" /></a>
                {/if}
              </div>
            {/if}
            <div class="meta details">
              <span class:ok={h.connected}>{seen(h)}</span>
              {#if h.hostIp}<span class="mono">{h.hostIp}</span>{/if}
              {#if h.baseDomain}<span class="mono">*.{h.baseDomain}</span>{/if}
              {#if h.hostname && h.hostname !== h.name}<span>{h.hostname}</span>{/if}
              {#if engineLabel(h)}<span>{engineLabel(h)}</span>{/if}
              {#if h.version}<span>Agent {h.version}</span>{/if}
              {#if h.stacksDir}<span class="mono">{h.stacksDir}</span>{/if}
            </div>
          </div>
          {#if isAdmin() && editing !== h.id}
            <div class="row-actions">
              {#if h.connected}
                {@const hostTermOk = !!h.locks?.hostTerminal && !h.readOnly}
                <button
                  type="button"
                  class="btn btn-ghost"
                  disabled={!hostTermOk}
                  title={
                    hostTermOk
                      ? 'Open interactive shell in this host OS'
                      : h.readOnly
                        ? 'Host is read-only (MOORAGE_READ_ONLY=true)'
                        : 'Set MOORAGE_HOST_TERMINAL=true on this host to enable host terminal'
                  }
                  onclick={() => (hostTermTarget = h)}
                >
                  <Icon name={hostTermOk ? 'terminal' : 'lock'} /> Host terminal
                </button>
              {/if}
              {#if h.connected && !h.readOnly}
                <button class="btn btn-ghost" onclick={() => openPrune(h)}>Disk cleanup</button>
              {/if}
              <button class="btn btn-ghost" onclick={() => startEdit(h)}>Edit</button>
              {#if h.kind === 'agent'}
                <button class="btn btn-ghost danger" onclick={() => { removing = h; confirmRemove = true; }}>Remove</button>
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    {/if}
  </section>

  {#if isAdmin() && hostTermTarget}
    {#key hostTermTarget.id}
      <TerminalPanel
        mode="host"
        hostId={hostTermTarget.id}
        hostName={hostTermTarget.name}
        onClose={() => (hostTermTarget = null)}
      />
    {/key}
  {/if}

  {#if isAdmin() && pruneHost}
    <section class="card card-pad prune-card">
      <div class="head-bar">
        <h2 class="section-title">Disk cleanup · {pruneHost.name}</h2>
        <button type="button" class="btn btn-ghost" onclick={() => (pruneHost = null)}><Icon name="x" /> Close</button>
      </div>
      {#if pruneLoading}
        <div class="skeleton" style="height:14px;width:45%"></div>
      {:else if pruneData}
        <div class="prune-grid">
          <label class="check">
            <input type="checkbox" bind:checked={pruneOpts.images} />
            <span>Unused images ({pruneData.imagesCount}) · <span class="mono">{formatBytes(pruneData.imagesBytes)}</span></span>
          </label>
          <label class="check">
            <input type="checkbox" bind:checked={pruneOpts.containers} />
            <span>Stopped containers ({pruneData.containersCount}) · <span class="mono">{formatBytes(pruneData.containersBytes)}</span></span>
          </label>
          <label class="check">
            <input type="checkbox" bind:checked={pruneOpts.buildCache} />
            <span>Build cache · <span class="mono">{formatBytes(pruneData.buildCacheBytes)}</span></span>
          </label>
          <label class="check">
            <input type="checkbox" bind:checked={pruneOpts.volumes} />
            <span>Unused volumes ({pruneData.volumesCount}) · <span class="mono">{formatBytes(pruneData.volumesBytes)}</span></span>
          </label>
        </div>
        <div class="wd-actions">
          <span class="meta">Estimated total: <strong class="mono">{formatBytes(pruneData.totalBytes)}</strong></span>
          <span class="grow"></span>
          {#if pruneMsg}<span class="meta ok">{pruneMsg}</span>{/if}
          <button
            type="button"
            class="btn btn-primary"
            disabled={pruneRunning || (!pruneOpts.images && !pruneOpts.containers && !pruneOpts.buildCache && !pruneOpts.volumes)}
            onclick={runPrune}
          >
            {pruneRunning ? 'Cleaning…' : 'Clean up selected'}
          </button>
        </div>
      {/if}
      {#if pruneError}<p class="error-text" role="alert">{pruneError}</p>{/if}
    </section>
  {/if}

  {#if isAdmin()}
    <SystemUpdates primaryBusy={wdDirty || editing !== null} onUpdateAvail={(v) => (sysUpdateAvail = v)} />

    {#if showAdd || joined}
    <section class="card card-pad add">
      <h2 class="section-title">Add a host</h2>
      {#if !acceptsAgents}
        <p class="meta">The agent port is off on this hub. Set <code>MOORAGE_AGENT_LISTEN</code> (for example <code>:5081</code>) and restart Moorage.</p>
      {:else if !joined}
        <form class="addform" onsubmit={add}>
          <label class="lbl" for="hname">Name</label>
          <div class="addrow">
            <input id="hname" class="field" bind:value={newName} maxlength="40" placeholder="Raspberry Pi" autocomplete="off" />
            <button class="btn {sysUpdateAvail || wdDirty || editing !== null ? '' : 'btn-primary'}" disabled={!newName.trim() || adding} aria-busy={adding}><Icon name="plug" /> Create join token</button>
          </div>
          <p class="meta">The token pairs one agent, works once and expires in 24 hours.</p>
          {#if addError}<p class="error-text" role="alert">{addError}</p>{/if}
        </form>
      {:else}
        <div class="joined">
          <p>Run the agent on <strong>{joined.name}</strong>. The token is shown only now.</p>

          <label class="lbl" for="hub">Hub address the agent connects to</label>
          <input id="hub" class="field mono" bind:value={hubAddr} spellcheck="false" />
          <p class="meta">Port {agentPort} of this server must be reachable from the host. Don't put it behind your HTTPS proxy: the agent needs a direct TLS connection.</p>

          <label class="check">
            <input type="checkbox" bind:checked={readOnly} />
            <span>Read-only: Moorage can watch this host but never change it</span>
          </label>

          <div class="snip-head">
            <span class="lbl">compose.yaml for the agent</span>
            <button class="btn btn-ghost" onclick={() => copy(composeSnippet, 'Compose file copied')}><Icon name="copy" /> Copy</button>
          </div>
          <pre class="snip">{composeSnippet}</pre>

          <div class="snip-head">
            <span class="lbl">Join token only</span>
            <button class="btn btn-ghost" onclick={() => copy(joined!.token, 'Token copied')}><Icon name="copy" /> Copy</button>
          </div>
          <pre class="snip token">{joined.token}</pre>
          {#if copied}<p class="meta ok" role="status">{copied}</p>{/if}

          <p class="meta">Valid until {new Date(joined.expires).toLocaleString()}. The host appears above as connected once the agent starts.</p>
          <button class="btn" onclick={() => { joined = null; showAdd = false; }}>Done</button>
        </div>
      {/if}
    </section>
    {/if}

    <form class="card card-pad watchdog" onsubmit={saveWatchdog}>
      <div class="wd-head">
        <div class="wd-summary">
          <h2 class="section-title">Hub watchdog</h2>
          <span class="tag {wdEnabled ? 'ok-tag' : ''}">{wdEnabled ? 'Enabled' : 'Off'}</span>
          {#if wdEnabled}
            <span class="meta">{wdSelectedCount} {wdSelectedCount === 1 ? 'agent' : 'agents'}</span>
          {/if}
        </div>
        <div class="wd-summary">
          <label class="check wd-switch">
            <input type="checkbox" bind:checked={wdEnabled} onchange={() => { wdDirty = true; wdOpen = true; }} />
            <span>{wdEnabled ? 'On' : 'Off'}</span>
          </label>
          <button type="button" class="btn btn-ghost" onclick={() => (wdOpen = !wdOpen)}>
            {wdOpen || wdDirty || wdError ? 'Hide' : 'Configure'}
          </button>
        </div>
      </div>

      {#if wdOpen || wdDirty || wdError}

      <div class="wd-grid">
        <div>
          <span class="lbl">Notification channel</span>
          {#if channels.length === 0}
            <p class="meta">No enabled notification channels yet. <a href="#/notifications">Add one</a> first.</p>
          {:else}
            <Select
              label="Watchdog notification channel"
              value={String(wdChannelId)}
              items={channels.map((ch) => ({ value: String(ch.id), label: `${ch.name} (${ch.kind})` }))}
              onchange={(v) => {
                wdChannelId = Number(v);
                wdDirty = true;
              }}
            />
          {/if}
          <p class="meta">
            This channel's credentials are copied to the watching agents so they can send when the hub is down. Other channels stay on the hub.
          </p>
        </div>
        <div>
          <label class="lbl" for="wd-grace">Alert after hub is unreachable for</label>
          <div class="grace-row">
            <input
              id="wd-grace"
              type="number"
              class="field mono grace-in"
              min="1"
              max="60"
              bind:value={wdGraceMin}
              oninput={() => (wdDirty = true)}
            />
            <span class="meta">minutes (1–60)</span>
          </div>
          <p class="meta">Long enough to ignore a normal hub restart.</p>
        </div>
      </div>

      <div class="wd-agents">
        <span class="lbl">Watching agents</span>
        {#if wdAgents.length === 0}
          <p class="meta">Pair at least one agent above to watch this hub.</p>
        {:else}
          <MultiSelect label="Watching agents" placeholder="No agent watches"
            items={wdAgents.map((a) => {
              const info = agentWatchdogNote(a);
              return { value: a.id, label: a.name, meta: info.canToggle ? info.note : undefined, disabled: info.canToggle ? undefined : info.note };
            })}
            selected={wdAgents.filter((a) => wdSelected[a.id]).map((a) => a.id)}
            onchange={(ids) => {
              wdSelected = Object.fromEntries(wdAgents.map((a) => [a.id, ids.includes(a.id)]));
              wdDirty = true;
            }} />
        {/if}
      </div>

      <div class="wd-maint">
        <span class="lbl">Planned maintenance</span>
        <div class="wd-maint-row">
          {#if wdMaintenanceUntil}
            <span class="tag ro">Paused until {new Date(wdMaintenanceUntil).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
            <button type="button" class="btn" onclick={() => setMaintenance(0)} disabled={wdMaintBusy}>End pause</button>
          {:else}
            <button type="button" class="btn" onclick={() => setMaintenance(60)} disabled={wdMaintBusy}>Pause for 1 h</button>
            <span class="meta">Silences the watchdog while you upgrade or reboot the hub.</span>
          {/if}
        </div>
      </div>

      {#if wdTestMsg}<p class="meta ok" role="status">{wdTestMsg}</p>{/if}
      {#if wdError}<p class="error-text" role="alert">{wdError}</p>{/if}

      <div class="wd-actions">
        <button
          type="button"
          class="btn"
          onclick={testWatchdog}
          disabled={wdTesting || !wdChannelId || wdAgents.length === 0}
          aria-busy={wdTesting}
        >
          Send test via agent
        </button>
        <span class="grow"></span>
        {#if wdSaveOk}<span class="meta ok"><Icon name="check" /> Saved</span>{/if}
        <button class="btn {wdDirty ? 'btn-primary' : ''}" disabled={wdSaving} aria-busy={wdSaving}>Save watchdog</button>
      </div>

      <p class="meta wd-foot">
        Quorum: majority of watching agents (1 decides alone, 2 require both). Single-agent disconnect sends a normal host-offline alert.
      </p>
      {/if}
    </form>
  {/if}
</div>

<Confirm bind:open={confirmRemove} title="Remove {removing?.name}?" danger confirmLabel="Remove host"
  body="The agent is disconnected and its certificate stops working. Stacks on that host keep running. To add it again, create a new join token and delete the agent's data folder."
  onConfirm={remove} />

<style>
  .hosts {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .ext-link {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 12px;
    font-weight: 500;
    color: var(--buoy-text);
  }
  .head {
    display: grid;
    gap: 6px;
  }
  .head-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  .wd-summary {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .ok-tag {
    background: color-mix(in srgb, var(--afloat) 16%, transparent);
    color: var(--afloat);
  }
  .prune-card {
    display: grid;
    gap: 10px;
  }
  .prune-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 8px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .list {
    padding: 4px 0;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 14px;
    padding: 14px 20px;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .dot.offline {
    background: var(--becalmed);
  }
  .info {
    flex: 1;
    min-width: 0;
    display: grid;
    gap: 3px;
  }
  .name {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    font-weight: 500;
  }
  .name a {
    color: var(--foam);
  }
  .tag {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .tag.ro {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .details {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 12px;
    font-size: 12px;
  }
  .details .ok {
    color: var(--afloat-text);
  }
  .row-actions {
    display: flex;
    gap: 4px;
  }
  .danger {
    color: var(--adrift-text);
  }
  .rename {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .rename .field {
    max-width: 240px;
  }
  .add {
    display: grid;
    gap: 12px;
  }
  .addform,
  .joined {
    display: grid;
    gap: 8px;
  }
  .addrow {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .addrow .field {
    flex: 1;
    min-width: 180px;
    max-width: 320px;
  }
  .lbl {
    font-size: 12px;
    font-weight: 500;
    color: var(--mist);
  }
  .joined .lbl {
    margin-top: 6px;
  }
  .check {
    display: flex;
    gap: 10px;
    align-items: center;
    font-size: 13px;
    color: var(--mist);
    margin-top: 4px;
    cursor: pointer;
  }
  .check input {
    accent-color: var(--buoy);
    width: 16px;
    height: 16px;
  }
  .snip-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-top: 6px;
  }
  .snip {
    margin: 0;
    padding: 12px 14px;
    background: var(--well);
    border-radius: var(--r-md);
    font: 12px/1.6 var(--mono);
    color: var(--mist);
    overflow-x: auto;
    white-space: pre;
  }
  .snip.token {
    white-space: pre-wrap;
    word-break: break-all;
  }
  .ok {
    color: var(--afloat-text);
  }
  .joined .btn:last-child {
    justify-self: start;
    margin-top: 4px;
  }
  .watchdog {
    display: grid;
    gap: 16px;
  }
  .wd-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 16px;
    flex-wrap: wrap;
  }
  .wd-head .meta {
    max-width: 64ch;
    margin-top: 4px;
  }
  .wd-switch {
    font-weight: 500;
    color: var(--foam);
    margin-top: 0;
  }
  .wd-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 14px;
  }
  .wd-grid > div,
  .wd-agents,
  .wd-maint {
    display: grid;
    gap: 6px;
    align-content: start;
  }
  .grace-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .grace-in {
    width: 88px;
  }
  .wd-maint-row {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .wd-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .grow {
    flex: 1;
  }
  .wd-foot {
    border-top: 1px solid var(--rope);
    padding-top: 12px;
  }

  @media (max-width: 767px) {
    .wd-grid {
      grid-template-columns: minmax(0, 1fr);
    }
    .row {
      flex-wrap: wrap;
      padding: 12px 14px;
    }
    .row-actions {
      width: 100%;
      padding-left: 22px;
    }
  }
</style>
