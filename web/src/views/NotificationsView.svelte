<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Channel, type ChannelConfig, type ChannelKind } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';

  type Draft = { id?: number; name: string; kind: ChannelKind; config: ChannelConfig; enabled: boolean };

  let channels = $state<Channel[] | null>(null);
  let error = $state('');
  let draft = $state<Draft | null>(null);
  let saving = $state(false);
  let testing = $state(false);
  let formMsg = $state<{ ok: boolean; text: string } | null>(null);
  let removing = $state<Channel | null>(null);
  let confirmRemove = $state(false);

  const kinds: { k: ChannelKind; name: string; hint: string }[] = [
    { k: 'ntfy', name: 'ntfy', hint: 'Push to your phone, self-hosted or ntfy.sh' },
    { k: 'telegram', name: 'Telegram', hint: 'A bot writes to you or a group' },
    { k: 'discord', name: 'Discord', hint: 'A channel webhook' },
    { k: 'smtp', name: 'E-mail', hint: 'Through your SMTP server' },
    { k: 'webhook', name: 'Webhook', hint: 'JSON to any URL' },
  ];
  const kindName = Object.fromEntries(kinds.map((k) => [k.k, k.name])) as Record<ChannelKind, string>;

  async function load() {
    try {
      channels = await api.channels();
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }
  $effect(() => {
    untrack(load);
  });

  function startNew(k: ChannelKind) {
    formMsg = null;
    draft = { name: kindName[k], kind: k, config: k === 'smtp' ? { port: 587, security: 'starttls' } : {}, enabled: true };
  }

  function edit(c: Channel) {
    formMsg = null;
    draft = { id: c.id, name: c.name, kind: c.kind, config: { ...c.config }, enabled: c.enabled };
  }

  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (!draft) return;
    saving = true;
    formMsg = null;
    try {
      await api.saveChannel($state.snapshot(draft) as Draft);
      draft = null;
      await load();
    } catch (err) {
      formMsg = { ok: false, text: (err as Error).message };
    } finally {
      saving = false;
    }
  }

  async function sendTest() {
    if (!draft) return;
    testing = true;
    formMsg = null;
    try {
      await api.testChannel($state.snapshot(draft) as Draft);
      formMsg = { ok: true, text: 'Test message sent. Check that it arrived.' };
    } catch (err) {
      formMsg = { ok: false, text: (err as Error).message };
    } finally {
      testing = false;
    }
  }

  async function toggle(c: Channel) {
    try {
      await api.saveChannel({ ...c, enabled: !c.enabled });
      await load();
    } catch (err) {
      error = (err as Error).message;
    }
  }

  async function remove() {
    if (!removing) return;
    try {
      await api.deleteChannel(removing.id);
      if (draft?.id === removing.id) draft = null;
      await load();
    } catch (err) {
      error = (err as Error).message;
    }
  }

  function summary(c: Channel): string {
    switch (c.kind) {
      case 'smtp':
        return `${c.config.to} via ${c.config.host}`;
      case 'telegram':
        return `chat ${c.config.chatId}`;
      case 'discord':
        return 'webhook';
      default:
        return c.config.url ?? '';
    }
  }
</script>

<div class="page">
  <section class="card card-pad head">
    <a class="back meta" href="#/checks"><Icon name="chevronLeft" /> Checks</a>
    <h1 class="title">Notifications</h1>
    <p class="meta">
      Where alerts go. Each check picks its channels. Alerts of one stack within 10 seconds arrive as one message, with the
      stack's last log lines. Secrets are never shown again after saving.
    </p>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if channels?.length}
    <section class="card list" aria-label="Channels">
      {#each channels as c (c.id)}
        <div class="row">
          <span class="dot {c.enabled ? 'up' : 'paused'}"></span>
          <div class="info">
            <span class="name">{c.name} <span class="tag">{kindName[c.kind]}</span></span>
            <span class="meta mono sum">{summary(c)}</span>
          </div>
          <div class="row-actions">
            <button class="btn btn-ghost" onclick={() => toggle(c)}>{c.enabled ? 'Turn off' : 'Turn on'}</button>
            <button class="btn btn-ghost" onclick={() => edit(c)}>Edit</button>
            <button class="btn btn-ghost danger" onclick={() => { removing = c; confirmRemove = true; }} aria-label="Delete {c.name}"><Icon name="trash" /></button>
          </div>
        </div>
      {/each}
    </section>
  {/if}

  <section class="card card-pad">
    {#if !draft}
      <h2 class="section-title">Add a channel</h2>
      <div class="kinds">
        {#each kinds as k (k.k)}
          <button class="kind" onclick={() => startNew(k.k)}>
            <span class="kname">{k.name}</span>
            <span class="khint">{k.hint}</span>
          </button>
        {/each}
      </div>
    {:else}
      <form class="form" onsubmit={save}>
        <h2 class="section-title">{draft.id ? 'Edit' : 'New'} {kindName[draft.kind]} channel</h2>
        <div>
          <label class="label" for="cname">Name</label>
          <input id="cname" class="field" bind:value={draft.name} maxlength="60" required />
        </div>

        {#if draft.kind === 'ntfy'}
          <div>
            <label class="label" for="url">Topic URL</label>
            <input id="url" class="field mono" bind:value={draft.config.url} placeholder="https://ntfy.sh/moorage-a8f3k" required />
            <p class="hint">Pick a long, hard-to-guess topic name on public servers: anyone who knows it can read it.</p>
          </div>
          <div>
            <label class="label" for="tok">Access token <span class="opt">optional</span></label>
            <input id="tok" class="field mono" bind:value={draft.config.token} autocomplete="off" />
          </div>
        {:else if draft.kind === 'telegram'}
          <div class="two">
            <div>
              <label class="label" for="tok">Bot token</label>
              <input id="tok" class="field mono" bind:value={draft.config.token} placeholder="123456:ABC…" autocomplete="off" required />
            </div>
            <div>
              <label class="label" for="chat">Chat ID</label>
              <input id="chat" class="field mono" bind:value={draft.config.chatId} placeholder="-1001234567890" required />
            </div>
          </div>
          <p class="hint">Create a bot with @BotFather, send it a message, then open api.telegram.org/bot&lt;token&gt;/getUpdates to find the chat ID.</p>
        {:else if draft.kind === 'discord'}
          <div>
            <label class="label" for="url">Webhook URL</label>
            <input id="url" class="field mono" bind:value={draft.config.url} placeholder="https://discord.com/api/webhooks/…" autocomplete="off" required />
            <p class="hint">Channel settings → Integrations → Webhooks. Mentions in log lines are disabled, so a log can't ping @everyone.</p>
          </div>
        {:else if draft.kind === 'webhook'}
          <div>
            <label class="label" for="url">URL</label>
            <input id="url" class="field mono" bind:value={draft.config.url} placeholder="https://example.com/hooks/moorage" required />
          </div>
          <div>
            <label class="label" for="tok">Bearer token <span class="opt">optional</span></label>
            <input id="tok" class="field mono" bind:value={draft.config.token} autocomplete="off" />
            <p class="hint">Moorage POSTs JSON: event, title, text, logs, url and the check details.</p>
          </div>
        {:else if draft.kind === 'smtp'}
          <div class="three">
            <div class="wide">
              <label class="label" for="host">SMTP server</label>
              <input id="host" class="field mono" bind:value={draft.config.host} placeholder="smtp.example.com" required />
            </div>
            <div>
              <label class="label" for="port">Port</label>
              <input id="port" class="field mono" type="number" min="1" max="65535" bind:value={draft.config.port} required />
            </div>
            <div>
              <label class="label" for="sec">Encryption</label>
              <select id="sec" class="field" bind:value={draft.config.security}>
                <option value="starttls">STARTTLS (587)</option>
                <option value="tls">TLS (465)</option>
              </select>
            </div>
          </div>
          <div class="two">
            <div>
              <label class="label" for="user">Username <span class="opt">optional</span></label>
              <input id="user" class="field" bind:value={draft.config.username} autocomplete="off" />
            </div>
            <div>
              <label class="label" for="pass">Password</label>
              <input id="pass" class="field" type="password" bind:value={draft.config.password} autocomplete="new-password" />
            </div>
          </div>
          <div class="two">
            <div>
              <label class="label" for="from">From</label>
              <input id="from" class="field" bind:value={draft.config.from} placeholder="Moorage <moorage@example.com>" required />
            </div>
            <div>
              <label class="label" for="to">To</label>
              <input id="to" class="field" bind:value={draft.config.to} placeholder="you@example.com, team@example.com" required />
            </div>
          </div>
          <p class="hint">Moorage only sends over an encrypted connection.</p>
        {/if}

        {#if formMsg}
          <div class="msg {formMsg.ok ? 'ok' : 'bad'}" role="status"><Icon name={formMsg.ok ? 'check' : 'alert'} /><span>{formMsg.text}</span></div>
        {/if}
        <div class="buttons">
          <button type="button" class="btn" onclick={sendTest} disabled={testing} aria-busy={testing}>Send test</button>
          <span class="grow"></span>
          <button type="button" class="btn btn-ghost" onclick={() => (draft = null)}>Cancel</button>
          <button class="btn btn-primary" disabled={saving} aria-busy={saving}>Save</button>
        </div>
      </form>
    {/if}
  </section>
</div>

<Confirm bind:open={confirmRemove} title="Delete {removing?.name}?" danger confirmLabel="Delete channel"
  body="Checks stop sending alerts here. Other channels keep working." onConfirm={remove} />

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 820px;
  }
  .head {
    display: grid;
    gap: 6px;
  }
  .head .meta {
    max-width: 70ch;
  }
  .back {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    font-size: 12px;
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
    padding: 12px 20px;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .info {
    flex: 1;
    min-width: 0;
    display: grid;
    gap: 2px;
  }
  .name {
    font-weight: 500;
  }
  .tag {
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
    margin-left: 6px;
  }
  .sum {
    font-size: 12px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .row-actions {
    display: flex;
    gap: 2px;
  }
  .danger {
    color: var(--adrift-text);
  }
  .kinds {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
    gap: 8px;
    margin-top: 12px;
  }
  .kind {
    display: grid;
    gap: 2px;
    text-align: left;
    padding: 12px;
    border: 0;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    color: var(--foam);
    font: inherit;
    cursor: pointer;
  }
  .kind:hover {
    box-shadow: inset 0 0 0 1px var(--buoy);
  }
  .kind:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: 2px;
  }
  .kname {
    font-weight: 600;
    font-size: 13px;
  }
  .khint {
    font-size: 11px;
    color: var(--haze);
  }
  .form {
    display: grid;
    gap: 16px;
  }
  .two {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }
  .three {
    display: grid;
    grid-template-columns: 2fr 1fr 1.4fr;
    gap: 12px;
  }
  .opt {
    font-weight: 400;
    color: var(--haze);
    margin-left: 4px;
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
  .buttons {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .grow {
    flex: 1;
  }

  @media (max-width: 767px) {
    .two,
    .three {
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
