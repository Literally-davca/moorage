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
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';

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

  const pendingDrafts = $derived(mcpDrafts.filter((d) => d.status === 'pending'));
</script>

<div class="bpanel">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if addonId === 'stack-scheduler'}
    <form class="grid-form" onsubmit={createTask}>
      <input class="field" bind:value={taskName} placeholder="Task name (e.g. Nightly Nextcloud cron)" required />
      <select class="field" bind:value={taskHost}>
        {#each app.hosts as h (h.id)}
          <option value={h.id}>{h.name}</option>
        {/each}
      </select>
      <input class="field mono" bind:value={taskStack} placeholder="Stack name" required />
      <input class="field mono" bind:value={taskCron} placeholder="0 4 * * *" title="5-field cron: min hour day month weekday" required />
      <select class="field" bind:value={taskAction}>
        <option value="restart">Restart</option>
        <option value="pull-up">Pull + Up</option>
        <option value="start">Start</option>
        <option value="stop">Stop</option>
        <option value="exec">Container exec</option>
      </select>
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
                <span class="meta mono">{t.hostId}/{t.stack}</span>
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
      <select class="field" style="width:auto" bind:value={proxyMode} onchange={load}>
        <option value="ro">Read-only socket proxy</option>
        <option value="write">Read-write socket proxy</option>
      </select>
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
      <select class="field" bind:value={whProvider}>
        <option value="ghcr">GitHub Container Registry (GHCR)</option>
        <option value="dockerhub">Docker Hub</option>
        <option value="gitea">Gitea / Harbor</option>
        <option value="generic">Generic</option>
      </select>
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
        <code class="mono">{mcpEndpoint || '/mcp'}</code>
        <button type="button" class="btn btn-sm" onclick={() => copy(mcpEndpoint || '/mcp', 'mcpep')}>
          <Icon name="copy" /> {copied === 'mcpep' ? 'Copied' : 'Copy URL'}
        </button>
      </div>
    </div>

    {#if pendingDrafts.length > 0}
      <div class="drafts-box">
        <h4 class="section-title">Pending approval drafts ({pendingDrafts.length})</h4>
        {#each pendingDrafts as d (d.id)}
          <div class="row-item">
            <div style="flex:1;min-width:0">
              <div class="rtitle">
                <span class="badge degraded">Pending</span>
                <strong>{d.tool}</strong>
                <span class="mono">{d.hostId}/{d.stack}</span>
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
      <select class="field" bind:value={mcpHost}>
        <option value="*">All hosts (*)</option>
        {#each app.hosts as h (h.id)}
          <option value={h.id}>{h.name}</option>
        {/each}
      </select>
      <input class="field mono" bind:value={mcpStackPat} placeholder="Stack glob (* or media-*)" />
      <select class="field" bind:value={mcpScope}>
        <option value="read">Read only</option>
        <option value="operate">Operate (start/stop/restart/pull)</option>
        <option value="deploy">Deploy (create/update/deploy)</option>
      </select>
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
