<script lang="ts">
  import { untrack } from 'svelte';
  import { api, engineLabel, type Channel, type HostEntry, type TerminalAuditRow, type WatchdogAgent } from '../lib/api';
  import { app, isAdmin, setHost } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';

  let { onChanged }: { onChanged: () => void } = $props();

  let hosts = $state<HostEntry[] | null>(null);
  let agentPort = $state('5081');
  let acceptsAgents = $state(true);
  let metricsOn = $state(false);
  let metricsURL = $state('');
  let error = $state('');

  // add host
  let newName = $state('');
  let adding = $state(false);
  let addError = $state('');
  let joined = $state<{ name: string; token: string; expires: string } | null>(null);
  let hubAddr = $state('');
  let readOnly = $state(false);
  let copied = $state('');

  // rename / remove
  let editing = $state<string | null>(null);
  let editName = $state('');
  let removing = $state<HostEntry | null>(null);
  let confirmRemove = $state(false);

  // hub watchdog
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

  // terminal audit log
  let termSessions = $state<TerminalAuditRow[]>([]);

  async function load() {
    try {
      const r = await api.hosts();
      hosts = r.hosts;
      app.hosts = r.hosts;
      agentPort = r.agentPort || '5081';
      acceptsAgents = r.acceptsAgents ?? false;
      metricsOn = r.metrics?.enabled ?? false;
      metricsURL = r.metrics?.url || `${location.origin}/metrics`;
      if (!hubAddr) hubAddr = `${location.hostname}:${agentPort}`;
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
    if (isAdmin()) {
      try {
        const [w, chs, ts] = await Promise.all([
          api.watchdog(),
          api.channels(),
          api.terminalSessions().catch(() => []),
        ]);
        channels = chs.filter((c) => c.enabled);
        wdAgents = w.agents;
        termSessions = ts ?? [];
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

  async function rename(h: HostEntry) {
    try {
      await api.renameHost(h.id, editName.trim());
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

  const composeSnippet = $derived(
    joined
      ? `services:
  moorage-agent:
    image: moorage:dev
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

  function fmtTermDuration(row: TerminalAuditRow): string {
    if (!row.endedAt || row.endedAt.startsWith('0001-')) return 'active';
    const sec = Math.max(0, Math.round((new Date(row.endedAt).getTime() - new Date(row.startedAt).getTime()) / 1000));
    if (sec < 60) return `${sec} s`;
    if (sec < 3600) return `${Math.floor(sec / 60)} m ${sec % 60} s`;
    return `${Math.floor(sec / 3600)} h ${Math.floor((sec % 3600) / 60)} m`;
  }

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
    <h1 class="title">Hosts</h1>
    <p class="meta">
      Each Docker host runs a Moorage agent that connects out to this hub over mutual TLS. An agent offers only stack
      operations, never the raw Docker API.
    </p>
    {#if isAdmin() && hosts}
      <p class="meta metrics">
        {#if metricsOn}
          Prometheus metrics: on at <span class="mono">{metricsURL}</span> (bearer token from <code>MOORAGE_METRICS_TOKEN</code>)
        {:else}
          Prometheus metrics: off. Set <code>MOORAGE_METRICS_TOKEN</code> to serve <span class="mono">/metrics</span>.
        {/if}
      </p>
    {/if}
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
              <form class="rename" onsubmit={(e) => { e.preventDefault(); rename(h); }}>
                <input class="field" bind:value={editName} maxlength="40" aria-label="Host name" />
                <button class="btn btn-primary" disabled={!editName.trim()}>Save</button>
                <button type="button" class="btn btn-ghost" onclick={() => (editing = null)}>Cancel</button>
              </form>
            {:else}
              <div class="name">
                <a href={'#/h/' + encodeURIComponent(h.id)} onclick={(e) => { e.preventDefault(); setHost(h.id); }}>{h.name}</a>
                {#if h.kind === 'local'}<span class="tag">This server</span>{/if}
                {#if h.readOnly}<span class="tag ro"><Icon name="lock" /> Read-only</span>{/if}
                {#if h.terminal && !h.readOnly}<span class="tag"><Icon name="terminal" /> Terminal</span>{/if}
              </div>
            {/if}
            <div class="meta details">
              <span class:ok={h.connected}>{seen(h)}</span>
              {#if h.hostname && h.hostname !== h.name}<span>{h.hostname}</span>{/if}
              {#if engineLabel(h)}<span>{engineLabel(h)}</span>{/if}
              {#if h.version}<span>Agent {h.version}</span>{/if}
              {#if h.stacksDir}<span class="mono">{h.stacksDir}</span>{/if}
            </div>
          </div>
          {#if isAdmin() && h.kind === 'agent' && editing !== h.id}
            <div class="row-actions">
              <button class="btn btn-ghost" onclick={() => { editing = h.id; editName = h.name; }}>Rename</button>
              <button class="btn btn-ghost danger" onclick={() => { removing = h; confirmRemove = true; }}>Remove</button>
            </div>
          {/if}
        </div>
      {/each}
    {/if}
  </section>

  {#if isAdmin()}
    <section class="card card-pad add">
      <h2 class="section-title">Add a host</h2>
      {#if !acceptsAgents}
        <p class="meta">The agent port is off on this hub. Set <code>MOORAGE_AGENT_LISTEN</code> (for example <code>:5081</code>) and restart Moorage.</p>
      {:else if !joined}
        <form class="addform" onsubmit={add}>
          <label class="lbl" for="hname">Name</label>
          <div class="addrow">
            <input id="hname" class="field" bind:value={newName} maxlength="40" placeholder="Raspberry Pi" autocomplete="off" />
            <button class="btn btn-primary" disabled={!newName.trim() || adding} aria-busy={adding}><Icon name="plug" /> Create join token</button>
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
          <button class="btn" onclick={() => (joined = null)}>Done</button>
        </div>
      {/if}
    </section>

    <form class="card card-pad watchdog" onsubmit={saveWatchdog}>
      <div class="wd-head">
        <div>
          <h2 class="section-title">Hub watchdog</h2>
          <p class="meta">
            When this hub goes down, watching agents agree on a single emergency notice (and a recovery notice when it is back)
            and send it directly through the chosen channel.
          </p>
        </div>
        <label class="check wd-switch">
          <input type="checkbox" bind:checked={wdEnabled} onchange={() => (wdDirty = true)} />
          <span>{wdEnabled ? 'Enabled' : 'Off'}</span>
        </label>
      </div>

      <div class="wd-grid">
        <div>
          <label class="lbl" for="wd-chan">Notification channel</label>
          {#if channels.length === 0}
            <p class="meta">No enabled notification channels yet. <a href="#/notifications">Add one</a> first.</p>
          {:else}
            <select id="wd-chan" class="field" bind:value={wdChannelId} onchange={() => (wdDirty = true)}>
              {#each channels as ch (ch.id)}
                <option value={ch.id}>{ch.name} ({ch.kind})</option>
              {/each}
            </select>
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
          <div class="wd-agent-list">
            {#each wdAgents as a (a.id)}
              {@const info = agentWatchdogNote(a)}
              <label class="wd-agent" class:off={!info.canToggle && !wdSelected[a.id]}>
                <input
                  type="checkbox"
                  checked={!!wdSelected[a.id]}
                  disabled={!info.canToggle && !wdSelected[a.id]}
                  onchange={(e) => {
                    wdSelected = { ...wdSelected, [a.id]: e.currentTarget.checked };
                    wdDirty = true;
                  }}
                />
                <span class="wd-aname">{a.name}</span>
                <span class="meta mono">{info.note}</span>
              </label>
            {/each}
          </div>
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
        The hub counts as down when most watching agents lose it. With 1 agent it decides alone, with 2 both must lose it.
        When only one agent loses the hub, the hub sends a normal host-offline alert instead.
      </p>
    </form>

    <section class="card card-pad term-audit" aria-label="Terminal sessions">
      <h2 class="section-title">Terminal sessions</h2>
      <p class="meta">
        Audit log of interactive container shells across all hosts (kept for 1 year). Typed commands and terminal output are
        never recorded.
      </p>
      {#if termSessions.length === 0}
        <p class="meta" style="margin-top:8px">No terminal sessions recorded yet.</p>
      {:else}
        <div class="term-table-wrap">
          <table class="term-table">
            <thead>
              <tr>
                <th>Started</th>
                <th>User · IP</th>
                <th>Host · Stack</th>
                <th>Container · Shell</th>
                <th>Duration</th>
                <th>Ended</th>
              </tr>
            </thead>
            <tbody>
              {#each termSessions as ts (ts.id)}
                <tr>
                  <td class="mono">{new Date(ts.startedAt).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</td>
                  <td>
                    <span class="foam">{ts.user}</span>
                    <span class="meta mono"> · {ts.ip}</span>
                  </td>
                  <td>
                    <a href={`#/h/${encodeURIComponent(ts.hostId)}/stacks/${encodeURIComponent(ts.stack)}`}>
                      {ts.hostName || ts.hostId} / {ts.stack}
                    </a>
                  </td>
                  <td class="mono">{ts.containerName} · {ts.shell}</td>
                  <td class="mono">{fmtTermDuration(ts)}</td>
                  <td>
                    <span class="tag {ts.reason === 'revoked' || ts.reason === 'error' ? 'ro' : ''}">
                      {ts.reason || 'open'}{ts.exitCode !== undefined ? ` (${ts.exitCode})` : ''}
                    </span>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </section>
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
    max-width: 880px;
  }
  .head {
    display: grid;
    gap: 6px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .head .meta {
    max-width: 70ch;
  }
  .metrics .mono {
    overflow-wrap: anywhere;
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
  .wd-agent-list {
    display: grid;
    gap: 6px;
  }
  .wd-agent {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    padding: 8px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    cursor: pointer;
    font-size: 13px;
  }
  .wd-agent.off {
    opacity: 0.6;
    cursor: not-allowed;
  }
  .wd-agent input {
    accent-color: var(--buoy);
    width: 16px;
    height: 16px;
  }
  .wd-aname {
    font-weight: 500;
    color: var(--foam);
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
  .term-audit {
    display: grid;
    gap: 8px;
  }
  .term-table-wrap {
    overflow-x: auto;
    margin-top: 6px;
  }
  .term-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 13px;
  }
  .term-table th {
    text-align: left;
    font-size: 11px;
    font-weight: 500;
    letter-spacing: 0.04em;
    color: var(--haze);
    padding: 8px 10px;
  }
  .term-table td {
    padding: 8px 10px;
    border-top: 1px solid var(--rope);
    vertical-align: middle;
  }
  .term-table .foam {
    color: var(--foam);
    font-weight: 500;
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
