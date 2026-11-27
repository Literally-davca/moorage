<script lang="ts">
  import { api, type StackSummary } from '../lib/api';
  import { app, isAdmin, link } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import AltUrls from '../lib/AltUrls.svelte';
  import QrLink from '../lib/QrLink.svelte';
  import type { AltUrl } from '../lib/hostnet';

  // Home → Services (M19F): every running service has a link, or a reason and a fix.
  // Links come from labels, the reverse proxy, published ports on the host's addresses,
  // declared ports the host listens on (host network) and links set in Moorage.
  let { stacks }: { stacks: StackSummary[] | null } = $props();

  type Link = {
    service: string;
    container?: string;
    url?: string;
    alternatives?: AltUrl[];
    source?: string;
    reason?: string;
    note?: string;
    ports?: string[];
  };
  type Row = { stack: string; status: string; managed: boolean; self?: boolean; links: Link[] };

  let rows = $state<Row[] | null>(null);
  let failed = $state(false);
  let openMissing = $state(false);
  let fixing = $state<{ stack: string; l: Link } | null>(null);
  let preview = $state<{ diff: string; mode: string } | null>(null);
  let setLinkUrl = $state('');
  let msg = $state('');
  let busy = $state(false);
  // M19J: Tailscale Serve on this host (admins; opt-in on the node)
  let ts = $state<{ available: boolean; reason?: string; dnsName?: string; entries: { port: number; url: string; target: string; moorage: boolean }[] } | null>(null);
  let tsConfirm = $state(false);

  async function loadTs() {
    if (!isAdmin()) return;
    try {
      ts = await api.tailscaleStatus();
    } catch {
      ts = null;
    }
  }
  function loopbackPort(l: Link): number {
    const p = l.ports?.[0] ?? '';
    const m = /:(\d+):\d+/.exec(p);
    return m ? Number(m[1]) : 0;
  }
  async function serveTs() {
    if (!fixing) return;
    busy = true;
    msg = '';
    try {
      const r = await api.tailscaleServe(loopbackPort(fixing.l));
      await api.setStackLinksOn(fixing.stack, fixing.l.service, [r.url]);
      msg = `Served on the tailnet: ${r.url}`;
      fixing = null;
      tsConfirm = false;
      void load();
      void loadTs();
    } catch (e) {
      msg = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  async function stopTs(port: number) {
    busy = true;
    try {
      await api.tailscaleStop(port);
      void loadTs();
    } catch (e) {
      msg = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function load() {
    try {
      rows = (await api.serviceLinks()).stacks as Row[];
      failed = false;
    } catch {
      failed = true;
    }
  }
  $effect(() => {
    void app.host;
    void stacks;
    void load();
  });
  $effect(() => {
    void app.host;
    void loadTs();
  });

  const linked = $derived((rows ?? []).filter((r) => r.links.some((l) => l.url)));
  const missing = $derived(
    (rows ?? []).flatMap((r) => r.links.filter((l) => !l.url && r.status !== 'stopped' && l.reason !== 'stopped').map((l) => ({ r, l }))),
  );
  const fallback = $derived((stacks ?? []).filter((s) => (s.urls?.length ?? 0) > 0));

  function shortUrl(u: string) {
    try {
      const p = new URL(u);
      return p.host + (p.pathname !== '/' ? p.pathname : '');
    } catch {
      return u;
    }
  }

  function openFix(stack: string, l: Link) {
    fixing = { stack, l };
    preview = null;
    setLinkUrl = '';
    msg = '';
  }
  async function previewFix(mode: 'vpn' | 'all') {
    if (!fixing?.l.ports?.length) return;
    busy = true;
    msg = '';
    try {
      const r = await api.fixPorts(fixing.stack, fixing.l.service, fixing.l.ports[0], mode, false);
      preview = { diff: r.diff, mode };
    } catch (e) {
      msg = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  async function confirmFix() {
    if (!fixing?.l.ports?.length || !preview) return;
    if (preview.mode === 'all' &&
      !confirm('Docker publishes past the host firewall (ufw): the service will be reachable from your whole network, and from the internet if the host is public. Publish it?'))
      return;
    busy = true;
    try {
      await api.fixPorts(fixing.stack, fixing.l.service, fixing.l.ports[0], preview.mode as 'vpn' | 'all', true);
      msg = 'Saved as a new version and deployed.';
      fixing = null;
      void load();
    } catch (e) {
      msg = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  async function saveLink() {
    if (!fixing) return;
    busy = true;
    try {
      await api.setStackLinksOn(fixing.stack, fixing.l.service, setLinkUrl.trim() ? [setLinkUrl.trim()] : []);
      fixing = null;
      void load();
    } catch (e) {
      msg = (e as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

{#if rows ? linked.length || missing.length : fallback.length}
  <section class="card card-pad" aria-label="Services">
    <div class="sec-head">
      <h2 class="section-title">Services</h2>
      <span class="meta">Links from labels, ports and your settings</span>
    </div>
    <ul class="rows">
      {#if rows && !failed}
        {#each linked as r (r.stack)}
          <li class="svc-row">
            <a class="svc-name" href={link('stacks/' + encodeURIComponent(r.stack))}>
              <span class="dot {r.status}"></span>
              <span class="n">{r.stack}</span>
            </a>
            <div class="svc-urls">
              {#each r.links.filter((l) => l.url) as l (l.service + l.url)}
                <a class="svc-url" href={l.url} target="_blank" rel="noopener noreferrer" title={`${l.service} · ${l.url} (${l.source === 'manual' ? 'set in Moorage' : l.source})`}>
                  <Icon name="external" /> <span>{shortUrl(l.url ?? '')}</span>
                </a>
                <QrLink url={l.url ?? ''} />
                <AltUrls urls={l.alternatives} compact />
              {/each}
            </div>
          </li>
        {/each}
      {:else}
        {#each fallback as s (s.name)}
          <li class="svc-row">
            <a class="svc-name" href={link('stacks/' + encodeURIComponent(s.name))}><span class="dot {s.status}"></span><span class="n">{s.name}</span></a>
            <div class="svc-urls">
              {#each s.urls ?? [] as u (u)}
                <a class="svc-url" href={u} target="_blank" rel="noopener noreferrer" title={u}><Icon name="external" /> <span>{shortUrl(u)}</span></a>
              {/each}
              <AltUrls urls={s.altUrls} compact />
            </div>
          </li>
        {/each}
      {/if}
    </ul>
    {#if missing.length}
      <details class="missing" bind:open={openMissing}>
        <summary class="meta">{missing.length} {missing.length === 1 ? 'service' : 'services'} without a link</summary>
        <ul class="rows">
          {#each missing as m (m.r.stack + '/' + m.l.service)}
            <li class="svc-row">
              <a class="svc-name" href={link('stacks/' + encodeURIComponent(m.r.stack))}>
                <span class="n">{m.r.stack}{m.r.links.length > 1 ? ' · ' + m.l.service : ''}</span>
              </a>
              <span class="meta">{m.l.note}</span>
              {#if isAdmin() && !m.r.self}
                <button type="button" class="btn btn-sm" onclick={() => openFix(m.r.stack, m.l)}>{m.l.reason === 'loopback' && m.r.managed ? 'Fix' : 'Set link'}</button>
              {/if}
            </li>
          {/each}
        </ul>
      </details>
    {/if}
    {#if fixing}
      <div class="fix card-inset" role="dialog" aria-label="Fix the link of {fixing.l.service}">
        <strong>{fixing.stack} · {fixing.l.service}</strong>
        {#if fixing.l.reason === 'loopback' && rows?.find((r) => r.stack === fixing?.stack)?.managed}
          <div class="acts">
            <button type="button" class="btn btn-sm" onclick={() => previewFix('vpn')} disabled={busy}>Publish on Tailscale / VPN address</button>
            <button type="button" class="btn btn-sm" onclick={() => previewFix('all')} disabled={busy}>Publish on all addresses</button>
          </div>
          {#if preview}
            <pre class="mono diff">{preview.diff}</pre>
            <div class="acts">
              <button type="button" class="btn btn-sm btn-primary" onclick={confirmFix} disabled={busy}>Save &amp; deploy</button>
              <button type="button" class="btn btn-sm btn-ghost" onclick={() => (preview = null)}>Cancel</button>
            </div>
          {/if}
        {/if}
        {#if fixing.l.reason === 'loopback' && loopbackPort(fixing.l)}
          {@const port = loopbackPort(fixing.l)}
          {#if ts?.available}
            {#if tsConfirm}
              <p class="meta">https://{ts.dnsName}:{port} → http://127.0.0.1:{port} – reachable only inside your tailnet (no Funnel).</p>
              <div class="acts">
                <button type="button" class="btn btn-sm btn-primary" onclick={serveTs} disabled={busy}>Serve on tailnet</button>
                <button type="button" class="btn btn-sm btn-ghost" onclick={() => (tsConfirm = false)}>Cancel</button>
              </div>
            {:else}
              <div class="acts"><button type="button" class="btn btn-sm" onclick={() => (tsConfirm = true)}>Serve on tailnet (HTTPS)…</button></div>
            {/if}
          {:else if ts?.reason}
            <p class="meta">Serve on tailnet: {ts.reason}</p>
          {/if}
          <details class="meta">
            <summary>NetBird</summary>
            <p>
              NetBird's <code>netbird expose</code> makes the service <strong>public on the internet</strong> while the command runs (beta). Moorage
              doesn't run it; on the host, run:
            </p>
            <pre class="mono diff">netbird expose {port} --with-pin</pre>
          </details>
        {/if}
        <form class="acts" onsubmit={(e) => { e.preventDefault(); void saveLink(); }}>
          <input class="field mono" placeholder="https://beszel.tail1234.ts.net" bind:value={setLinkUrl} aria-label="Link" />
          <button type="submit" class="btn btn-sm" disabled={busy}>Set link</button>
          <button type="button" class="btn btn-sm btn-ghost" onclick={() => (fixing = null)}>Close</button>
        </form>
        <p class="meta">Set link stores the address in Moorage (for example from tailscale serve); compose.yaml doesn't change.</p>
      </div>
    {/if}
    {#if isAdmin() && ts?.entries?.some((e) => e.moorage)}
      <details class="missing">
        <summary class="meta">Served on the tailnet by Moorage ({ts.entries.filter((e) => e.moorage).length})</summary>
        <ul class="rows">
          {#each ts.entries.filter((e) => e.moorage) as e (e.port)}
            <li class="svc-row">
              <a class="svc-url" href={e.url} target="_blank" rel="noopener noreferrer"><Icon name="external" /> <span>{e.url}</span></a>
              <span class="meta">→ {e.target}</span>
              <button type="button" class="btn btn-sm btn-ghost" onclick={() => stopTs(e.port)} disabled={busy}>Stop serving</button>
            </li>
          {/each}
        </ul>
      </details>
    {/if}
    {#if msg}<p class="meta" role="status">{msg}</p>{/if}
  </section>
{/if}

<style>
  .sec-head {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 12px;
    margin-bottom: 8px;
  }
  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .n {
    font-weight: 500;
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
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
  .missing summary {
    cursor: pointer;
    margin-top: 6px;
  }
  .fix {
    display: grid;
    gap: 8px;
    margin-top: 8px;
    padding: 10px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--rope-strong);
  }
  .acts {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
    align-items: center;
  }
  .acts input {
    flex: 1;
    min-width: 200px;
  }
  .diff {
    margin: 0;
    font-size: 12px;
    white-space: pre;
    overflow-x: auto;
  }
</style>
