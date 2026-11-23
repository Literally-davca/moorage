<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Check } from '../lib/api';
  import { isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Strip from '../lib/Strip.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import { ago, duration, kindName, multiLocation, pct, quorumText, statusText, toInput, voteText } from '../lib/checks';
  import type { LocationResult, Quorum } from '../lib/api';
  import { untilText } from '../lib/maintenance';

  let { id }: { id: number } = $props();

  let check = $state<Check | null>(null);
  let error = $state('');
  let confirmDelete = $state(false);
  let busy = $state(false);

  async function load() {
    try {
      check = await api.check(id);
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(load);
    const t = setInterval(() => !document.hidden && load(), 10000);
    return () => clearInterval(t);
  });

  async function setEnabled(on: boolean) {
    if (!check) return;
    busy = true;
    try {
      await api.saveCheck(check.id, { ...toInput(check), enabled: on });
      await load();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function del() {
    try {
      await api.deleteCheck(id);
      location.hash = '#/checks';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  const timeFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
  // One row per configured location: its last answer (if any) and its recent results.
  const locRows = $derived.by(() => {
    if (!check || !multiLocation(check)) return [];
    const last = new Map((check.state.locations ?? []).map((l) => [l.location, l] as [string, LocationResult]));
    return check.locations.map((id) => ({
      id,
      last: last.get(id),
      name: last.get(id)?.name ?? (id === 'hub' ? 'hub' : id),
      ticks: check!.locationTicks?.[id] ?? [],
    }));
  });
  const stackHref = $derived(check?.stack && check.hostId ? `#/h/${encodeURIComponent(check.hostId)}/stacks/${encodeURIComponent(check.stack)}` : '');
</script>

{#if error && !check}
  <section class="card card-pad"><p class="error-text" role="alert">{error}</p></section>
{:else if !check}
  <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:22px;width:220px"></div></section>
{:else}
  <div class="page">
    <section class="card card-pad head">
      <div class="head-row">
        <div class="ident">
          <a class="back meta" href="#/checks"><Icon name="chevronLeft" /> Checks</a>
          <h1 class="title">{check.name}</h1>
          <div class="sub">
            <span class="badge {check.state.status === 'up' ? 'up' : check.state.status === 'down' ? 'down' : check.state.status === 'pending' ? 'degraded' : check.state.status === 'maintenance' ? 'maintenance' : ''}">
              <span class="dot {check.state.status}"></span>{statusText(check)}
            </span>
            {#if check.state.since && check.state.status !== 'unknown'}<span class="meta">for {duration(check.state.since)}</span>{/if}
            {#if check.maintenance}
              <a class="badge maintenance" href={`#/maintenance/${check.maintenance.id}`} title={check.maintenance.title}>
                <Icon name="tool" /> Maintenance until {untilText(check.maintenance.until)}
              </a>
            {/if}
            <span class="meta">
              {kindName[check.kind]} · <span class="mono">{check.kind === 'log' ? check.settings.pattern : check.target}</span>
              {#if check.kind === 'log' && check.target}<span>in <span class="mono">{check.target}</span></span>{/if}
            </span>
            {#if stackHref}<a class="meta" href={stackHref}>Stack {check.stack}{check.hostName ? ` on ${check.hostName}` : ''}</a>{/if}
          </div>
        </div>
        {#if isAdmin()}
          <div class="actions">
            <button class="btn" onclick={() => setEnabled(!check!.enabled)} disabled={busy}>
              <Icon name={check.enabled ? 'pause' : 'play'} /> {check.enabled ? 'Pause' : 'Resume'}
            </button>
            <a class="btn" href={`#/checks/${check.id}/edit`}>Edit</a>
            <button class="btn btn-ghost danger" onclick={() => (confirmDelete = true)} aria-label="Delete check"><Icon name="trash" /></button>
          </div>
        {/if}
      </div>
      {#if error}<p class="error-text" role="alert">{error}</p>{/if}
    </section>

    <section class="card card-pad sounding">
      <div class="hero">
        <span class="hero-label">Uptime 24 h</span>
        <span class="hero-num num">{pct(check.uptime24h)}</span>
        <span class="meta num">7 d {pct(check.uptime7d)} · 30 d {pct(check.uptime30d)}</span>
      </div>
      <div class="strip-col">
        <Strip ticks={check.ticks} anchors={check.anchors ?? []} legend />
        {#if check.state.last}
          <p class="last meta">
            Last check {ago(check.state.last.at)}: <span class:bad={check.state.last.status === 'down'}>{check.state.last.msg}</span>
            · {check.state.last.latencyMs} ms · every {check.interval} s, alerts after {check.retries} {check.retries === 1 ? 'failure' : 'failures in a row'}
          </p>
        {:else if !check.enabled}
          <p class="last meta">Paused. Resume to run the check again.</p>
        {/if}
      </div>
    </section>

    {#if locRows.length}
      <section class="card locations">
        <h2 class="section-title pad">
          Locations
          {#if check.locations.length >= 2}<span class="meta q">down when: {quorumText[(check.quorum || 'majority') as Quorum].toLowerCase()}</span>{/if}
        </h2>
        {#each locRows as r (r.id)}
          <div class="loc">
            <div class="loc-head">
              <span class="dot {r.last?.vote === 'up' ? 'up' : r.last?.vote === 'down' ? 'down' : 'unknown'}"></span>
              <span class="loc-name">{r.name}</span>
              <span class:bad={r.last?.vote === 'down'}>{r.last ? voteText(r.last) : 'No result yet'}</span>
              {#if r.last && r.last.vote !== 'none'}<span class="meta num">{r.last.latencyMs} ms</span>{/if}
            </div>
            {#if r.last?.msg}<p class="meta loc-msg">{r.last.msg}</p>{/if}
            {#if r.ticks.length}<Strip ticks={r.ticks} mini />{/if}
          </div>
        {/each}
      </section>
    {/if}

    <section class="card incidents">
      <h2 class="section-title pad">Incidents</h2>
      {#if !check.incidents?.length}
        <p class="meta pad-b">No incidents. Nice and calm.</p>
      {:else}
        {#each check.incidents as inc (inc.id)}
          <article class="inc">
            <div class="inc-head">
              <span class="dot {inc.endedAt ? 'up' : 'down'}"></span>
              <span class="inc-time">{timeFmt.format(new Date(inc.startedAt))}</span>
              <span class="meta">{inc.endedAt ? `lasted ${duration(inc.startedAt, inc.endedAt)}` : `ongoing for ${duration(inc.startedAt)}`}</span>
              {#if stackHref}
                <a class="meta before-link" href={`${stackHref}/history?at=${Math.floor(new Date(inc.startedAt).getTime() / 1000)}`}>
                  What changed before
                </a>
              {/if}
            </div>
            <p class="cause">{inc.cause}</p>
            {#if inc.context}
              {#each inc.context.containers ?? [] as c (c.id)}
                {#if (c.exitCode !== undefined && c.exitCode !== 0) || c.health === 'unhealthy' || c.state !== 'running'}
                  <p class="meta ctr">{c.name}: {c.status}</p>
                {/if}
              {/each}
              {#if inc.context.note}<p class="meta">{inc.context.note}</p>{/if}
              {#if inc.context.locations?.length}
                <ul class="inc-locs">
                  {#each inc.context.locations as l (l.location)}
                    <li class="meta"><span class="loc-name">{l.name}</span>: {voteText(l)} · {l.msg}{l.vote !== 'none' ? ` · ${l.latencyMs} ms` : ''}</li>
                  {/each}
                </ul>
              {/if}
              {#if inc.context.matches?.length}
                <details open>
                  <summary class="meta">Matched log lines ({inc.context.matches.length})</summary>
                  <pre class="logs">{#each inc.context.matches as l, i (i)}<span class:err={l.s === 'stderr'}>{l.c} | {l.m}</span>{'\n'}{/each}</pre>
                </details>
              {/if}
              {#if inc.context.logs?.length}
                <details>
                  <summary class="meta">Log lines at the time of the alert ({inc.context.logs.length})</summary>
                  <pre class="logs">{#each inc.context.logs as l, i (i)}<span class:err={l.s === 'stderr'}>{l.c} | {l.m}</span>{'\n'}{/each}</pre>
                </details>
              {/if}
            {/if}
          </article>
        {/each}
      {/if}
    </section>
  </div>

  <Confirm bind:open={confirmDelete} title="Delete {check.name}?" danger confirmLabel="Delete check"
    body="The check, its results and incidents are removed. Alerts for it stop." onConfirm={del} />
{/if}

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 16px;
    flex-wrap: wrap;
  }
  .back {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    margin-bottom: 6px;
    font-size: 12px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
    word-break: break-word;
  }
  .sub {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-top: 8px;
    flex-wrap: wrap;
  }
  .badge .dot {
    width: 6px;
    height: 6px;
  }
  .actions {
    display: flex;
    gap: 8px;
  }
  .danger {
    color: var(--adrift-text);
  }
  .sounding {
    display: grid;
    grid-template-columns: 160px minmax(0, 1fr);
    gap: 24px;
    align-items: center;
  }
  .hero {
    display: grid;
    gap: 2px;
  }
  .hero-label {
    font-size: 11px;
    font-weight: 500;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    color: var(--haze);
  }
  .hero-num {
    font-size: 28px;
    font-weight: 600;
    letter-spacing: -0.02em;
    color: var(--foam);
  }
  .hero .meta {
    font-size: 12px;
  }
  .strip-col {
    min-width: 0;
  }
  .last {
    margin-top: 10px;
    font-size: 12px;
  }
  .bad {
    color: var(--adrift-text);
  }
  .before-link {
    margin-left: auto;
    color: var(--buoy-text);
  }
  .incidents,
  .locations {
    padding-bottom: 4px;
  }
  .q {
    font-weight: 400;
    font-size: 12px;
    margin-left: 8px;
  }
  .loc {
    padding: 10px 20px;
    border-top: 1px solid var(--rope);
    display: grid;
    gap: 4px;
    min-width: 0;
  }
  .loc-head {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    font-size: 13px;
  }
  .loc-name {
    font-weight: 600;
    color: var(--foam);
  }
  .loc-msg {
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  .inc-locs {
    margin: 2px 0 0;
    padding: 0;
    list-style: none;
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  .pad {
    padding: 18px 20px 10px;
  }
  .pad-b {
    padding: 0 20px 18px;
  }
  .inc {
    padding: 12px 20px;
    border-top: 1px solid var(--rope);
    display: grid;
    gap: 4px;
  }
  .inc-head {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 10px;
  }
  .inc-time {
    font: 12px var(--mono);
    color: var(--mist);
  }
  .cause {
    font-size: 13px;
    color: var(--foam);
  }
  .ctr {
    font-size: 12px;
  }
  details summary {
    cursor: pointer;
    font-size: 12px;
    margin-top: 2px;
  }
  .logs {
    margin: 8px 0 0;
    padding: 10px 12px;
    background: var(--well);
    border-radius: var(--r-md);
    font: 12px/1.7 var(--mono);
    color: var(--mist);
    overflow-x: auto;
    white-space: pre;
  }
  .logs .err {
    color: var(--adrift-text);
  }

  @media (max-width: 767px) {
    .sounding {
      grid-template-columns: minmax(0, 1fr);
      gap: 14px;
    }
  }
</style>
