<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type DepNode, type HostDeps, type StackDep } from '../lib/api';
  import { app, currentHost, isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import { detectionText, edgeKind, edgeText, layout, nodeClass, nodeText, refKey } from '../lib/deps';

  let data = $state<HostDeps | null>(null);
  let error = $state('');
  let detecting = $state(false);

  const hostId = $derived(app.host);

  async function load() {
    const want = app.host;
    try {
      const d = await api.deps(want);
      if (want !== app.host) return;
      data = d;
      error = '';
    } catch (e) {
      if (want !== app.host) return;
      error = (e as Error).message;
    }
  }

  // reload when the host changes (a primitive key); poll every 15 s
  $effect(() => {
    void hostId;
    data = null;
    untrack(() => void load());
    const t = setInterval(() => !document.hidden && void load(), 15000);
    return () => clearInterval(t);
  });

  async function detect() {
    detecting = true;
    try {
      await api.detectDeps();
      setTimeout(() => void load(), 1500);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      detecting = false;
    }
  }

  // graph geometry
  const W = 168;
  const H = 46;
  const GX = 28;
  const GY = 56;
  const PAD = 8;

  const nodeByKey = $derived(new Map((data?.nodes ?? []).map((n) => [refKey(n), n])));
  // Only stacks that have an edge go into the drawing; the rest are listed below it.
  const linked = $derived.by(() => {
    const keys = new Set<string>();
    for (const e of data?.edges ?? []) {
      keys.add(refKey(e.from));
      keys.add(refKey(e.to));
    }
    return (data?.nodes ?? []).filter((n) => keys.has(refKey(n)));
  });
  const alone = $derived((data?.nodes ?? []).filter((n) => !n.external && !linked.some((l) => refKey(l) === refKey(n))));
  const graph = $derived(layout(linked, data?.edges ?? [], W, H, GX, GY));
  const pos = $derived(new Map(graph.placed.map((p) => [refKey(p.node), p])));

  function edgePath(e: StackDep): string {
    const a = pos.get(refKey(e.from));
    const b = pos.get(refKey(e.to));
    if (!a || !b) return '';
    // from the dependent's top edge to the dependency's bottom edge (the arrow points at
    // what is depended on); same-row edges bend below the boxes
    const x1 = a.x + W / 2 + PAD;
    const x2 = b.x + W / 2 + PAD;
    if (a.y === b.y) {
      const y = a.y + H + PAD;
      return `M ${x1} ${y} C ${x1} ${y + 30}, ${x2} ${y + 30}, ${x2} ${y + 4}`;
    }
    const up = a.y > b.y;
    const y1 = up ? a.y + PAD : a.y + H + PAD;
    const y2 = up ? b.y + H + PAD + 4 : b.y + PAD - 4;
    const my = (y1 + y2) / 2;
    return `M ${x1} ${y1} C ${x1} ${my}, ${x2} ${my}, ${x2} ${y2}`;
  }

  function href(n: DepNode): string {
    return `#/h/${encodeURIComponent(n.hostId)}/stacks/${encodeURIComponent(n.stack)}`;
  }

  function label(n: DepNode): string {
    return n.external ? `${n.stack} (${n.hostName || n.hostId})` : n.stack;
  }

  function clip(s: string, max = 20): string {
    return s.length > max ? s.slice(0, max - 1) + '…' : s;
  }

  const sortedEdges = $derived(
    [...(data?.edges ?? [])].sort(
      (a, b) => Number(a.ignored) - Number(b.ignored) || a.from.stack.localeCompare(b.from.stack) || a.to.stack.localeCompare(b.to.stack),
    ),
  );
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Dependencies</h1>
      <p class="meta">
        {currentHost()?.name ?? app.host} · which stacks need which. When a stack goes down, alerts of the stacks that
        depend on it are folded into one message that names the cause.
      </p>
    </div>
    {#if isAdmin() && currentHost()?.connected}
      <button class="btn" onclick={detect} disabled={detecting || data?.detection.running}>
        <Icon name="refresh" /> {data?.detection.running ? 'Detecting…' : 'Detect now'}
      </button>
    {/if}
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if data}
    <p class="meta note" class:warn={data.detection.unsupported || !!data.detection.error}>
      {#if data.detection.unsupported || data.detection.error}<Icon name="alert" />{/if}
      {detectionText(data.detection)}
      {#if data.stacksError}<span> {data.stacksError}</span>{/if}
    </p>

    {#if data.cycles.length}
      <section class="card card-pad loop" role="alert">
        <Icon name="alert" />
        <div>
          <p class="section-title">Dependency loop</p>
          {#each data.cycles as c, i (i)}
            <p class="meta">{c.map((r) => r.stack).join(' ↔ ')} depend on each other. Their alerts are not grouped; remove or ignore one of the dependencies.</p>
          {/each}
        </div>
      </section>
    {/if}

    {#if linked.length === 0}
      <section class="card card-pad empty">
        <p class="section-title">No dependencies yet</p>
        <p class="meta">
          A stack that uses another stack's network as <code>external</code>, or talks to its services by name, is linked
          automatically. Add other dependencies on a stack's page.
        </p>
      </section>
    {:else}
      <section class="card card-pad graph-card" aria-label="Dependency graph">
        <div class="legend meta">
          <span><svg width="28" height="8" aria-hidden="true"><line x1="0" y1="4" x2="28" y2="4" class="e manual" /></svg> manual</span>
          <span><svg width="28" height="8" aria-hidden="true"><line x1="0" y1="4" x2="28" y2="4" class="e detected" /></svg> detected</span>
          <span><svg width="28" height="8" aria-hidden="true"><line x1="0" y1="4" x2="28" y2="4" class="e ignored" /></svg> ignored</span>
          <span>arrows point at the stack that is needed</span>
        </div>
        <div class="graph-scroll">
          <svg
            class="graph"
            width={graph.width + PAD * 2}
            height={graph.height + PAD * 2 + 34}
            role="img"
            aria-label="Dependency graph; the list below has the same content"
          >
            <defs>
              <marker id="dep-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M0,0 L8,4 L0,8 z" class="arrow" />
              </marker>
            </defs>
            {#each data.edges as e (e.id)}
              {@const d = edgePath(e)}
              {#if d}<path {d} class="e {edgeKind(e)}" marker-end="url(#dep-arrow)"><title>{e.from.stack} depends on {e.to.stack} · {edgeText(e)}</title></path>{/if}
            {/each}
            {#each graph.placed as p (refKey(p.node))}
              {@const cls = nodeClass(p.node)}
              <a href={href(p.node)} class="node">
                <title>{label(p.node)}: {nodeText(p.node)}{p.node.inCycle ? ' · in a dependency loop' : ''}</title>
                <rect x={p.x + PAD} y={p.y + PAD} width={W} height={H} rx="8" class="box {cls}" class:loop={p.node.inCycle} />
                <circle cx={p.x + PAD + 14} cy={p.y + PAD + 16} r="4" class="st {cls}" />
                <text x={p.x + PAD + 24} y={p.y + PAD + 20} class="nname">{clip(label(p.node))}</text>
                <text x={p.x + PAD + 24} y={p.y + PAD + 36} class="nstat">{clip(nodeText(p.node) + (p.node.inCycle ? ' · loop' : ''), 24)}</text>
              </a>
            {/each}
          </svg>
        </div>
      </section>
    {/if}

    {#if data.edges.length}
      <section class="card list" aria-label="Dependencies as a list">
        {#each sortedEdges as e (e.id)}
          {@const from = nodeByKey.get(refKey(e.from))}
          {@const to = nodeByKey.get(refKey(e.to))}
          <div class="row" class:dim={e.ignored}>
            <span class="pair">
              <a href={from ? href(from) : '#'}>{from ? label(from) : e.from.stack}</a>
              <span class="badge {nodeClass(from)}">{nodeText(from)}</span>
              <span class="meta">depends on</span>
              <a href={to ? href(to) : '#'}>{to ? label(to) : e.to.stack}</a>
              <span class="badge {nodeClass(to)}">{nodeText(to)}</span>
            </span>
            <span class="meta sub">{edgeText(e)}{e.detail ? ` · ${e.detail}` : ''}</span>
          </div>
        {/each}
      </section>
    {/if}

    {#if alone.length && linked.length}
      <p class="meta note">Without dependencies: {alone.map((n) => n.stack).join(', ')}</p>
    {/if}
  {:else if !error}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:50%"></div></section>
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
    max-width: 70ch;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .note {
    display: flex;
    gap: 6px;
    align-items: baseline;
    flex-wrap: wrap;
    padding: 0 4px;
  }
  .note.warn {
    color: var(--signal-text);
  }
  .loop {
    display: flex;
    gap: 10px;
    color: var(--signal-text);
  }
  .empty {
    display: grid;
    gap: 8px;
  }
  .empty .meta {
    max-width: 62ch;
  }
  .graph-card {
    display: grid;
    gap: 10px;
  }
  .legend {
    display: flex;
    gap: 16px;
    flex-wrap: wrap;
    align-items: center;
    font-size: 12px;
  }
  .legend span {
    display: inline-flex;
    gap: 6px;
    align-items: center;
  }
  .graph-scroll {
    overflow-x: auto;
  }
  .graph {
    display: block;
  }
  .e {
    fill: none;
    stroke-width: 1.5;
  }
  .e.manual {
    stroke: var(--mist);
    stroke-width: 2;
  }
  .e.detected {
    stroke: var(--haze);
    stroke-dasharray: 6 4;
  }
  .e.ignored {
    stroke: var(--murk);
    stroke-dasharray: 2 4;
    opacity: 0.7;
  }
  .arrow {
    fill: var(--haze);
  }
  .box {
    fill: var(--pier-raised);
    stroke: var(--rope-stronger);
    stroke-width: 1;
  }
  .box.down {
    stroke: var(--adrift);
  }
  .box.degraded,
  .box.maintenance {
    stroke: var(--signal);
  }
  .box.loop {
    stroke-dasharray: 4 3;
  }
  .node:hover .box,
  .node:focus-visible .box {
    stroke: var(--buoy);
  }
  .st {
    fill: var(--becalmed);
  }
  .st.up {
    fill: var(--afloat);
  }
  .st.down {
    fill: var(--adrift);
  }
  .st.degraded,
  .st.maintenance {
    fill: var(--signal);
  }
  .nname {
    fill: var(--foam);
    font: 500 13px var(--font);
  }
  .nstat {
    fill: var(--haze);
    font: 400 11px var(--font);
  }
  .list {
    padding: 4px 0;
  }
  .row {
    display: grid;
    gap: 4px;
    padding: 12px 20px;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .row.dim {
    opacity: 0.7;
  }
  .pair {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
    overflow-wrap: anywhere;
  }
  .sub {
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  /* phones: the list says the same as the drawing */
  @media (max-width: 640px) {
    .graph-card {
      display: none;
    }
    .row {
      padding: 12px 14px;
    }
  }
</style>
