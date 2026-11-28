<script lang="ts">
  import { untrack } from 'svelte';
  import {
    api,
    type MCPACLRule,
    type MCPDraft,
    type MCPToken,
    type RegistryWebhook,
    type ScheduledTask,
    type SocketFinding,
  } from '../lib/api';
  import { app, stackLabel } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';
  import { absoluteEndpoint, mcpClients, type MCPClientId } from '../lib/mcpClients';

  let { addonId }: { addonId: 'stack-scheduler' | 'socket-proxy-check' | 'registry-webhooks' | 'mcp-server' } = $props();

  let loading = $state(false);
  let error = $state('');
  let copied = $state('');

  // stack-scheduler
  let tasks = $state<ScheduledTask[]>([]);
  let taskName = $state('');
  let taskHost = $state(untrack(() => app.host));
  let taskStack = $state('');
  let taskCron = $state('0 4 * * *');
  let taskAction = $state<'restart' | 'start' | 'stop' | 'pull-up' | 'exec'>('restart');
  let taskService = $state('');
  let taskCommand = $state('');

  // socket-proxy-check
  let findings = $state<SocketFinding[]>([]);
  let proxySnippet = $state('');
  let proxyMode = $state<'ro' | 'write'>('ro');

  // registry-webhooks
  let webhooks = $state<RegistryWebhook[]>([]);
  let whName = $state('');
  let whProvider = $state('ghcr');
  let whSecret = $state('');
  let whApplyNow = $state(false);
  let createdWebhookUrl = $state('');

  // mcp-server
  let mcpEndpoint = $state('');
  let mcpTokens = $state<MCPToken[]>([]);
  let mcpDrafts = $state<MCPDraft[]>([]);
  let mcpName = $state('');
  let mcpHost = $state('*');
  let mcpStackPat = $state('*');
  let mcpScope = $state<'read' | 'operate' | 'deploy'>('read');
  let mcpRequireApproval = $state(true);
  let mcpExpiresDays = $state(0);
  let createdMCPSecret = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      if (addonId === 'stack-scheduler') {
        const r = await api.schedulerTasks();
        tasks = r.tasks;
      } else if (addonId === 'socket-proxy-check') {
        const r = await api.socketProxyScan(proxyMode);
        findings = r.findings;
        proxySnippet = r.snippet;
      } else if (addonId === 'registry-webhooks') {
        const r = await api.registryWebhooks();
        webhooks = r.webhooks;
      } else if (addonId === 'mcp-server') {
        const [t, d] = await Promise.all([api.mcpTokens(), api.mcpDrafts()]);
        mcpEndpoint = t.endpoint;
        mcpTokens = t.tokens;
        mcpDrafts = d.drafts;
      }
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void addonId;
    untrack(() => void load());
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

  async function createTask(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    try {
      await api.saveSchedulerTask({
        name: taskName.trim(),
        hostId: taskHost,
        stack: taskStack.trim(),
        cron: taskCron.trim(),
        action: taskAction,
        service: taskService.trim(),
        command: taskCommand.trim(),
        enabled: true,
        channels: [],
      });
      taskName = '';
      taskStack = '';
      taskCommand = '';
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function toggleTask(t: ScheduledTask) {
    try {
      await api.saveSchedulerTask({ ...t, enabled: !t.enabled });
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function runTaskNow(id: number) {
    try {
      await api.runSchedulerTask(id);
      setTimeout(() => void load(), 1200);
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function deleteTask(id: number) {
    try {
      await api.deleteSchedulerTask(id);
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function createWebhook(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    createdWebhookUrl = '';
    try {
      const res = await api.createRegistryWebhook({
        name: whName.trim(),
        provider: whProvider,
        secret: whSecret.trim(),
        applyNow: whApplyNow,
      });
      createdWebhookUrl = res.url;
      whName = '';
      whSecret = '';
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function deleteWebhook(id: number) {
    try {
      await api.deleteRegistryWebhook(id);
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function createToken(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    createdMCPSecret = '';
    try {
      const acl: MCPACLRule[] = [{ hostId: mcpHost || '*', stackPattern: mcpStackPat.trim() || '*', scope: mcpScope }];
      const res = await api.createMCPToken({
        name: mcpName.trim(),
        acl,
        requireApproval: mcpRequireApproval,
        expiresDays: Number(mcpExpiresDays) || 0,
      });
      createdMCPSecret = res.secret;
      mcpName = '';
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function deleteToken(id: number) {
    try {
      await api.deleteMCPToken(id);
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  async function decideDraft(id: number, approve: boolean) {
    error = '';
    try {
      if (approve) await api.approveMCPDraft(id);
      else await api.rejectMCPDraft(id);
      await load();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  // the badge counts MCP token drafts only; assistant proposals are decided in the chat (M20G)
  const pendingDrafts = $derived(mcpDrafts.filter((d) => d.status === 'pending' && !d.tokenName.startsWith('assistant:')));
  let draftsEl = $state<HTMLElement | null>(null);
  let scrolled = false;
  $effect(() => {
    if (draftsEl && !scrolled && location.hash.includes('open=mcp-server')) {
      scrolled = true;
      draftsEl.scrollIntoView({ block: 'start' });
    }
  });

  // Client config files: the whole JSON to download or paste. With a fresh token it is
  // filled in; otherwise a placeholder (tokens are shown only once).
  let mcpClient = $state<MCPClientId>('claude-code');
  const fullEndpoint = $derived(absoluteEndpoint(mcpEndpoint, location.origin));
  const clients = $derived(mcpClients(fullEndpoint, createdMCPSecret));
  const client = $derived(clients.find((c) => c.id === mcpClient) ?? clients[0]);
  function downloadConfig() {
    const url = URL.createObjectURL(new Blob([client.json], { type: 'application/json' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = client.file;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
  }
</script>

<div class="bpanel">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if addonId === 'stack-scheduler'}
    <form class="grid-form" onsubmit={createTask}>
      <input class="field" bind:value={taskName} placeholder="Task name (e.g. Nightly Nextcloud cron)" required />
      <div class="sel-wrap">
        <Select
          label="Task host"
          value={taskHost}
          items={app.hosts.map((h) => ({ value: h.id, label: h.name }))}
          onchange={(v) => (taskHost = v)}
        />
      </div>
      <input class="field mono" bind:value={taskStack} placeholder="Stack name" required />
      <input class="field mono" bind:value={taskCron} placeholder="0 4 * * *" title="5-field cron: min hour day month weekday" required />
      <div class="sel-wrap">
        <Select
          label="Task action"
          value={taskAction}
          items={[
            { value: 'restart', label: 'Restart' },
            { value: 'pull-up', label: 'Pull + Up' },
            { value: 'start', label: 'Start' },
            { value: 'stop', label: 'Stop' },
            { value: 'exec', label: 'Container exec' },
          ]}
          onchange={(v) => (taskAction = v as typeof taskAction)}
        />
      </div>
      {#if taskAction === 'exec'}
        <input class="field mono" bind:value={taskService} placeholder="Service (optional)" />
        <input class="field mono" bind:value={taskCommand} placeholder="Command (e.g. php occ cron:run)" required />
      {/if}
      <button type="submit" class="btn btn-primary">Add task</button>
    </form>

    {#if tasks.length === 0 && !loading}
      <p class="meta">No scheduled tasks configured yet.</p>
    {:else}
      <div class="rows">
        {#each tasks as t (t.id)}
          <div class="row-item">
            <div>
              <div class="rtitle">
                <strong>{t.name}</strong>
                <span class="badge mono">{t.cron}</span>
                <span class="badge">{t.action}</span>
                <span class="meta mono">{t.hostId}/{t.stackDisplayName || stackLabel(t.hostId, t.stack)}</span>
              </div>
              {#if t.lastStatus}
                <div class="meta mono">Last: {t.lastStatus}{t.lastRunAt ? ` · ${new Date(t.lastRunAt).toLocaleString()}` : ''}</div>
              {/if}
            </div>
            <div class="ractions">
              <button type="button" class="btn btn-sm" onclick={() => t.id != null && runTaskNow(t.id)}><Icon name="play" /> Run now</button>
              <button type="button" class="btn btn-sm" onclick={() => toggleTask(t)}>{t.enabled ? 'Disable' : 'Enable'}</button>
              <button type="button" class="btn btn-sm btn-ghost btn-danger" onclick={() => t.id != null && deleteTask(t.id)}><Icon name="trash" /></button>
            </div>
          </div>
        {/each}
      </div>
    {/if}
  {:else if addonId === 'socket-proxy-check'}
    <div class="ractions">
      <button type="button" class="btn btn-primary" onclick={load} disabled={loading}>
        <Icon name="shield" /> {loading ? 'Scanning…' : 'Scan hosts'}
      </button>
      <div class="sel-wrap">
        <Select
          label="Socket proxy mode"
          compact={true}
          value={proxyMode}
          items={[
            { value: 'ro', label: 'Read-only socket proxy' },
            { value: 'write', label: 'Read-write socket proxy' },
          ]}
          onchange={(v) => {
            proxyMode = v as 'ro' | 'write';
            void load();
          }}
        />
      </div>
      <button type="button" class="btn" onclick={() => copy(proxySnippet, 'sock')}>
        <Icon name="copy" /> {copied === 'sock' ? 'Copied' : 'Copy socket-proxy snippet'}
      </button>
    </div>

    {#if findings.length === 0 && !loading}
      <p class="ok-msg"><Icon name="check" /> No exposed Docker sockets or TCP :2375 found across connected stacks.</p>
    {:else if findings.length > 0}
      <div class="rows">
        {#each findings as f, idx (`${f.hostId}-${f.stack}-${f.service}-${idx}`)}
          <div class="row-item">
            <div>
              <div class="rtitle">
                <span class="badge {f.severity === 'critical' ? 'down' : 'degraded'}">{f.severity}</span>
                <strong>{f.hostName}/{f.stack}/{f.service}</strong>
                <span>{f.issue}</span>
              </div>
              <div class="meta mono">{f.detail}</div>
            </div>
          </div>
        {/each}
      </div>
    {/if}

    {#if proxySnippet}
      <pre class="code mono">{proxySnippet}</pre>
    {/if}
  {:else if addonId === 'registry-webhooks'}
    <form class="grid-form" onsubmit={createWebhook}>
      <input class="field" bind:value={whName} placeholder="Webhook name (e.g. GHCR Deploy)" required />
      <div class="sel-wrap">
        <Select
          label="Webhook registry provider"
          value={whProvider}
          items={[
            { value: 'ghcr', label: 'GitHub Container Registry (GHCR)' },
            { value: 'dockerhub', label: 'Docker Hub' },
            { value: 'gitea', label: 'Gitea / Harbor' },
            { value: 'generic', label: 'Generic' },
          ]}
          onchange={(v) => (whProvider = v)}
        />
      </div>
      <input class="field mono" type="password" bind:value={whSecret} placeholder="Optional HMAC secret (X-Hub-Signature-256)" />
      <label class="check">
        <input type="checkbox" bind:checked={whApplyNow} />
        <span class="meta">Apply Auto-policy immediately (outside window)</span>
      </label>
      <button type="submit" class="btn btn-primary">Create webhook</button>
    </form>

    {#if createdWebhookUrl}
      <div class="secret-box">
        <span class="meta">Webhook URL (shown once — copy now):</span>
        <div class="ractions">
          <code class="mono">{createdWebhookUrl}</code>
          <button type="button" class="btn btn-sm" onclick={() => copy(createdWebhookUrl, 'wh')}>
            <Icon name="copy" /> {copied === 'wh' ? 'Copied' : 'Copy URL'}
          </button>
        </div>
      </div>
    {/if}

    <div class="rows">
      {#each webhooks as w (w.id)}
        <div class="row-item">
          <div>
            <div class="rtitle">
              <strong>{w.name}</strong>
              <span class="badge">{w.provider}</span>
              {#if w.applyNow}<span class="badge info">Apply immediately</span>{/if}
              {#if w.secretSet}<span class="badge">HMAC</span>{/if}
            </div>
            {#if w.lastHitAt}
              <div class="meta mono">Last hit: {new Date(w.lastHitAt).toLocaleString()} · {w.lastResult}</div>
            {/if}
          </div>
          <button type="button" class="btn btn-sm btn-ghost btn-danger" onclick={() => deleteWebhook(w.id)}><Icon name="trash" /></button>
        </div>
      {/each}
    </div>
  {:else if addonId === 'mcp-server'}
    <div class="secret-box">
      <span class="meta">Streamable HTTP MCP endpoint:</span>
      <div class="ractions">
        <code class="mono">{fullEndpoint}</code>
        <button type="button" class="btn btn-sm" onclick={() => copy(fullEndpoint, 'mcpep')}>
          <Icon name="copy" /> {copied === 'mcpep' ? 'Copied' : 'Copy URL'}
        </button>
      </div>
    </div>

    {#if pendingDrafts.length > 0}
      <div class="drafts-box" bind:this={draftsEl}>
        <h4 class="section-title">Pending approval drafts ({pendingDrafts.length})</h4>
        {#each pendingDrafts as d (d.id)}
          <div class="row-item">
            <div style="flex:1;min-width:0">
              <div class="rtitle">
                <span class="badge degraded">Pending</span>
                <strong>{d.tool}</strong>
                <span class="mono">{d.hostId}/{d.stackDisplayName || stackLabel(d.hostId, d.stack)}</span>
                <span class="meta">by mcp:{d.tokenName}</span>
              </div>
              {#if d.compose}
                <pre class="code mono" style="margin-top:6px">{d.compose}</pre>
              {/if}
            </div>
            <div class="ractions">
              <button type="button" class="btn btn-primary btn-sm" onclick={() => decideDraft(d.id, true)}>Approve &amp; Deploy</button>
              <button type="button" class="btn btn-sm btn-ghost btn-danger" onclick={() => decideDraft(d.id, false)}>Reject</button>
            </div>
          </div>
        {/each}
      </div>
    {/if}

    <form class="grid-form" onsubmit={createToken}>
      <input class="field" bind:value={mcpName} placeholder="Client name (e.g. Claude Desktop)" required />
      <div class="sel-wrap">
        <Select
          label="MCP ACL Host"
          value={mcpHost}
          items={[{ value: '*', label: 'All hosts (*)' }, ...app.hosts.map((h) => ({ value: h.id, label: h.name }))]}
          onchange={(v) => (mcpHost = v)}
        />
      </div>
      <input class="field mono" bind:value={mcpStackPat} placeholder="Stack glob (* or media-*)" />
      <div class="sel-wrap">
        <Select
          label="MCP ACL Scope"
          value={mcpScope}
          items={[
            { value: 'read', label: 'Read only' },
            { value: 'operate', label: 'Operate (start/stop/restart/pull)' },
            { value: 'deploy', label: 'Deploy (create/update/deploy)' },
          ]}
          onchange={(v) => (mcpScope = v as typeof mcpScope)}
        />
      </div>
      <input class="field" type="number" min="0" bind:value={mcpExpiresDays} placeholder="Expiry days (0 = never)" />
      <label class="check">
        <input type="checkbox" bind:checked={mcpRequireApproval} />
        <span class="meta">Require human approval before deploy</span>
      </label>
      <button type="submit" class="btn btn-primary">Generate token</button>
    </form>

    {#if createdMCPSecret}
      <div class="secret-box">
        <span class="meta">Bearer token (shown once — copy into your AI client config):</span>
        <div class="ractions">
          <code class="mono">{createdMCPSecret}</code>
          <button type="button" class="btn btn-sm" onclick={() => copy(createdMCPSecret, 'mcptok')}>
            <Icon name="copy" /> {copied === 'mcptok' ? 'Copied' : 'Copy token'}
          </button>
        </div>
      </div>
    {/if}

    <div class="secret-box client-cfg">
      <span class="meta">Client config file{createdMCPSecret ? ' – with the new token filled in' : ` – generate a token above to fill it in, or replace PASTE-YOUR-MOORAGE-MCP-TOKEN`}:</span>
      <div class="tabs" role="tablist">
        {#each clients as c (c.id)}
          <button type="button" class="tab" role="tab" aria-selected={mcpClient === c.id} onclick={() => (mcpClient = c.id)}>{c.label}</button>
        {/each}
      </div>
      <ul class="where meta">
        {#each client.where as w (w)}<li>{w}</li>{/each}
      </ul>
      <pre class="code mono">{client.json}</pre>
      <div class="ractions">
        <button type="button" class="btn btn-sm" onclick={() => copy(client.json, 'mcpcfg')}>
          <Icon name="copy" /> {copied === 'mcpcfg' ? 'Copied' : 'Copy JSON'}
        </button>
        <button type="button" class="btn btn-sm" onclick={downloadConfig}><Icon name="download" /> Download {client.file}</button>
      </div>
      <p class="meta">
        Already have this file? Add only the <code>moorage</code> entry to its <code>{mcpClient === 'vscode' ? 'servers' : 'mcpServers'}</code>.
        The file holds the token: keep it out of git (a project <code>.mcp.json</code> or <code>.vscode/mcp.json</code> is often committed).
      </p>
    </div>

    <div class="rows">
      {#each mcpTokens as tok (tok.id)}
        <div class="row-item">
          <div>
            <div class="rtitle">
              <strong>{tok.name}</strong>
              {#if tok.requireApproval}<span class="badge info">Approval required</span>{/if}
              {#each tok.acl as rule, i (i)}
                <span class="badge mono">{rule.hostId}/{rule.stackPattern}:{rule.scope}</span>
              {/each}
            </div>
            <div class="meta">
              Created by {tok.createdBy}{#if tok.lastUsedAt} · Last used {new Date(tok.lastUsedAt).toLocaleString()}{/if}
            </div>
          </div>
          <button type="button" class="btn btn-sm btn-ghost btn-danger" onclick={() => deleteToken(tok.id)}><Icon name="trash" /></button>
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .bpanel {
    margin-top: 12px;
    padding-top: 12px;
    border-top: 1px solid var(--rope);
    display: grid;
    gap: 10px;
  }
  .grid-form {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    align-items: center;
  }
  .grid-form .field {
    flex: 1 1 160px;
  }
  .rows,
  .drafts-box {
    display: grid;
    gap: 8px;
  }
  .row-item {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 10px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    flex-wrap: wrap;
  }
  .rtitle {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .ractions {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .secret-box {
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--info-wash);
    box-shadow: inset 0 0 0 1px var(--info);
    display: grid;
    gap: 6px;
  }
  .client-cfg {
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    min-width: 0;
  }
  .client-cfg pre {
    margin: 0;
    max-height: 260px;
    overflow: auto;
    white-space: pre;
  }
  .where {
    margin: 0;
    padding-left: 18px;
    overflow-wrap: anywhere;
  }
  .ok-msg {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--afloat-text);
  }
  .code {
    margin: 0;
    padding: 10px 12px;
    max-height: 220px;
    overflow: auto;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    font-size: 12px;
    white-space: pre;
  }
</style>
