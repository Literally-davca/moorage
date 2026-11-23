<script lang="ts">
  import { untrack } from 'svelte';
  import { api, engineLabel, statusLabel, type Check, type HostInfo, type StackSummary } from '../lib/api';
  import { app, canEdit, link, setHost } from '../lib/state.svelte';
  import { checkStatusLabel } from '../lib/checks';
  import Icon from '../lib/Icon.svelte';

  let { stacks, host }: { stacks: StackSummary[] | null; host: HostInfo | null } = $props();

  let checks = $state<Check[]>([]);
  let updateCount = $state(0);

  async function loadExtras() {
    try {
      checks = await api.checks();
    } catch {
      checks = [];
    }
    try {
      const ov = await api.updates();
      let n = 0;
      for (const st of ov.stacks ?? []) {
        for (const svc of st.services ?? []) {
          if (svc.status === 'update-available' || svc.status === 'pulled-not-applied' || svc.newerTag || svc.newerMajor) {
            n++;
          }
        }
      }
      updateCount = n;
    } catch {
      updateCount = 0;
    }
  }

  $effect(() => {
    untrack(() => void loadExtras());
    const t = setInterval(() => !document.hidden && void loadExtras(), 30000);
    return () => clearInterval(t);
  });

  const attentionStacks = $derived((stacks ?? []).filter((s) => s.status === 'down' || s.status === 'degraded'));
  const attentionChecks = $derived(checks.filter((c) => c.enabled && (c.state.status === 'down' || c.state.status === 'pending')));
  const serviceStacks = $derived((stacks ?? []).filter((s) => (s.urls?.length ?? 0) > 0));
  const runningCount = $derived((stacks ?? []).filter((s) => s.status === 'up').length);

  function shortUrl(raw: string): string {
    try {
      const u = new URL(raw);
      const p = u.pathname === '/' ? '' : u.pathname.replace(/\/+$/, '');
      return u.host + p;
    } catch {
      return raw.replace(/^https?:\/\//, '');
    }
  }

  function seen(iso?: string): string {
    if (!iso) return 'never connected';
    const s = (Date.now() - new Date(iso).getTime()) / 1000;
    if (s < 3600) return `seen ${Math.max(1, Math.round(s / 60))} min ago`;
    if (s < 86400) return `seen ${Math.round(s / 3600)} h ago`;
    return `seen ${Math.round(s / 86400)} d ago`;
  }
</script>

{#if stacks !== null && stacks.length === 0}
  <section class="card empty">
    <span class="mark"><Icon name="anchor" /></span>
    <h1>Add your first stack</h1>
    <p>
      A stack is a folder with a <code>compose.yaml</code> in
      <code>{host?.stacksDir ?? '/opt/stacks'}</code>. Create one here, or copy existing folders there and they show up on
      their own.
    </p>
    {#if canEdit()}<a class="btn btn-primary" href={link('new')}><Icon name="plus" /> Create stack</a>{/if}
  </section>
{:else if stacks !== null}
  <div class="home">
    <section class="card card-pad">
      <div class="head-row">
        <div>
          <h1 class="title">{host?.name ?? 'Host'}</h1>
          <p class="meta">
            {[`${stacks.length} stacks (${runningCount} up)`, engineLabel(host), host?.stacksDir].filter(Boolean).join(' · ')}
          </p>
        </div>
        {#if canEdit()}
          <a class="btn" href={link('new')}><Icon name="plus" /> New stack</a>
        {/if}
      </div>

      {#if attentionStacks.length || attentionChecks.length}
        <h2 class="section-title gap">Needs attention</h2>
        <ul class="rows">
          {#each attentionStacks as s (s.name)}
            <li>
              <a class="row-link" href={link('stacks/' + encodeURIComponent(s.name))}>
                <span class="dot {s.status}"></span>
                <span class="n">{s.name}</span>
                <span class="badge {s.status}">{statusLabel[s.status]}</span>
                <span class="meta num">{s.running}/{s.total} running</span>
              </a>
            </li>
          {/each}
          {#each attentionChecks as c (c.id)}
            <li>
              <a class="row-link" href={'#/checks/' + c.id}>
                <span class="dot {c.state.status}"></span>
                <span class="n">{c.name}</span>
                <span class="badge {c.state.status}">{checkStatusLabel[c.state.status]}</span>
                <span class="meta">{c.stack ? c.stack : c.kind}</span>
              </a>
            </li>
          {/each}
        </ul>
      {:else}
        <p class="calm"><Icon name="check" /> Every stack and check is healthy.</p>
      {/if}

      {#if updateCount > 0}
        <p class="upd-row meta">
          <Icon name="cloudDown" />
          <a href="#/updates">{updateCount} image {updateCount === 1 ? 'update' : 'updates'} available</a>
        </p>
      {/if}
    </section>

    {#if serviceStacks.length}
      <section class="card card-pad" aria-label="Services">
        <div class="sec-head">
          <h2 class="section-title">Services</h2>
          <span class="meta">Web links detected from labels</span>
        </div>
        <ul class="rows">
          {#each serviceStacks as s (s.name)}
            <li class="svc-row">
              <a class="svc-name" href={link('stacks/' + encodeURIComponent(s.name))}>
                <span class="dot {s.status}"></span>
                <span class="n">{s.name}</span>
              </a>
              <div class="svc-urls">
                {#each s.urls ?? [] as u (u)}
                  <a class="svc-url" href={u} target="_blank" rel="noopener noreferrer" title={u}>
                    <Icon name="external" /> <span>{shortUrl(u)}</span>
                  </a>
                {/each}
              </div>
            </li>
          {/each}
        </ul>
      </section>
    {/if}

    {#if app.hosts.length > 1}
      <section class="card card-pad" aria-label="Hosts">
        <div class="sec-head">
          <h2 class="section-title">Hosts</h2>
          <a class="meta" href="#/settings/hosts">Manage hosts</a>
        </div>
        <ul class="rows">
          {#each app.hosts as h (h.id)}
            <li>
              <button
                type="button"
                class="row-link host-row"
                class:current={h.id === app.host}
                onclick={() => setHost(h.id)}
              >
                <span class="dot {h.connected ? 'up' : 'offline'}"></span>
                <span class="n">{h.name}</span>
                <span class="badge">{h.kind === 'local' ? 'Hub' : 'Agent'}</span>
                {#if h.readOnly}<span class="meta"><Icon name="lock" /> read-only</span>{/if}
                <span class="meta">{h.connected ? 'online' : seen(h.lastSeen)}</span>
              </button>
            </li>
          {/each}
        </ul>
      </section>
    {/if}
  </div>
{/if}

<style>
  .home {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
    flex-wrap: wrap;
  }
  .sec-head {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 12px;
    margin-bottom: 8px;
  }
  .empty {
    display: grid;
    justify-items: start;
    gap: 12px;
    padding: 40px;
    max-width: 640px;
  }
  .mark {
    width: 40px;
    height: 40px;
    border-radius: 12px;
    background: var(--buoy-wash);
    color: var(--buoy-text);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 22px;
  }
  .empty h1,
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .empty p {
    color: var(--mist);
    max-width: 52ch;
  }
  .empty .btn {
    margin-top: 8px;
  }
  .gap {
    margin: 20px 0 8px;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .row-link {
    display: flex;
    align-items: center;
    gap: 12px;
    width: 100%;
    min-height: 42px;
    padding: 6px 12px;
    margin: 0 -12px;
    border: 0;
    background: transparent;
    border-radius: var(--r-md);
    color: var(--foam);
    font: inherit;
    text-align: left;
    text-decoration: none;
    cursor: pointer;
  }
  .row-link:hover {
    background: var(--rope);
    text-decoration: none;
  }
  .host-row.current {
    background: var(--buoy-wash);
  }
  .n {
    font-weight: 500;
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .calm {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 16px;
    color: var(--afloat-text);
  }
  .upd-row {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 12px;
    padding-top: 12px;
    border-top: 1px solid var(--rope);
  }
  .svc-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
    padding: 8px 0;
    border-top: 1px solid var(--rope);
  }
  .svc-row:first-child {
    border-top: 0;
  }
  .svc-name {
    display: inline-flex;
    align-items: center;
    gap: 10px;
    color: var(--foam);
    text-decoration: none;
    min-width: 0;
  }
  .svc-name:hover {
    text-decoration: underline;
  }
  .svc-urls {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 12px;
  }
  .svc-url {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: 13px;
    color: var(--buoy-text);
    text-decoration: none;
    max-width: 280px;
  }
  .svc-url span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .svc-url:hover {
    text-decoration: underline;
  }
</style>
