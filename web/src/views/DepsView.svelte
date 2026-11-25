<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type AutoRestartMode, type DepNode, type HostDeps, type StackDep } from '../lib/api';
  import { app, currentHost, isAdmin, stackLabel } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Select, { type SelectItem } from '../lib/Select.svelte';
  import {
    autoRestartLabels,
    computeSubtree,
    detectionText,
    edgeKind,
    edgeText,
    layoutByHost,
    nodeClass,
    nodeText,
    refKey,
    wouldCreateCycle,
  } from '../lib/deps';

  let data = $state<HostDeps | null>(null);
  let error = $state('');
  let detecting = $state(false);
  let busy = $state(false);
  let scope = $state<'current' | 'all'>('current');

  // Filters
  let onlyUnhealthy = $state(false);
  let hideExternal = $state(false);
  let showStandalone = $state(true);
  let hideIgnored = $state(false);

  // Selection & hover (blast radius)
  let hoverKey = $state<string | null>(null);
  let selectedKey = $state<string | null>(null);
  let selectedEdgeId = $state<number | null>(null);

  // Add dependency form
  let showAdd = $state(false);
  let fromHost = $state(app.host);
  let fromStack = $state('');
  let toHost = $state(app.host);
  let toStack = $state('');
  let fromOptions = $state<SelectItem[]>([]);
  let toOptions = $state<SelectItem[]>([]);

  // Zoom & pan state
  let zoom = $state(1);
  let panX = $state(0);
  let panY = $state(0);
  let panning = $state(false);
  let dragMoved = false;
  let dragStart = { x: 0, y: 0, px: 0, py: 0 };
  let viewportEl = $state<HTMLDivElement | null>(null);

  const hostId = $derived(app.host);

  async function load() {
    const wantHost = app.host;
    const wantScope = scope;
    try {
      const d = wantScope === 'all' ? await api.allDeps() : await api.deps(wantHost);
      if (wantScope !== scope || (wantScope === 'current' && wantHost !== app.host)) return;
      data = d;
      error = '';
    } catch (e) {
      if (wantScope !== scope || (wantScope === 'current' && wantHost !== app.host)) return;
      error = (e as Error).message;
    }
  }

  $effect(() => {
    void hostId;
    void scope;
    data = null;
    selectedKey = null;
    selectedEdgeId = null;
    untrack(() => void load());
    const t = setInterval(() => !document.hidden && void load(), 15000);
    return () => clearInterval(t);
  });

  $effect(() => {
    if (!showAdd) return;
    const fh = fromHost;
    untrack(() => {
      fromOptions = [];
      api.stacksOf(fh)
        .then((r) => {
          if (fh === fromHost) {
            fromOptions = r.stacks.map((s) => {
              const disp = stackLabel(fh, s.name, s.displayName);
              return { value: s.name, label: disp, meta: disp !== s.name ? s.name : undefined };
            });
          }
        })
        .catch(() => {});
    });
  });

  $effect(() => {
    if (!showAdd) return;
    const th = toHost;
    untrack(() => {
      toOptions = [];
      api.stacksOf(th)
        .then((r) => {
          if (th === toHost) {
            toOptions = r.stacks.map((s) => {
              const disp = stackLabel(th, s.name, s.displayName);
              return { value: s.name, label: disp, meta: disp !== s.name ? s.name : undefined };
            });
          }
        })
        .catch(() => {});
    });
  });

  const addValidation = $derived.by(() => {
    const fs = fromStack.trim();
    const ts = toStack.trim();
    if (!fs || !ts) return '';
    const fk = `${fromHost}/${fs}`;
    const tk = `${toHost}/${ts}`;
    if (fk === tk) return "A stack can't depend on itself.";
    const existing = (data?.edges ?? []).find((e) => !e.ignored && refKey(e.from) === fk && refKey(e.to) === tk);
    if (existing) return 'This dependency already exists.';
    if (wouldCreateCycle(fk, tk, data?.edges ?? [])) {
      return `${ts} already depends on ${fs}: this edge would create a dependency loop.`;
    }
    return '';
  });

  async function act(fn: () => Promise<unknown>) {
    busy = true;
    error = '';
    try {
      await fn();
      await load();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  function openAdd() {
    fromHost = app.host;
    toHost = app.host;
    fromStack = selectedNode ? selectedNode.stack : '';
    if (selectedNode) fromHost = selectedNode.hostId;
    toStack = '';
    showAdd = true;
  }

  function submitAdd(ev: SubmitEvent) {
    ev.preventDefault();
    if (addValidation || !fromStack.trim() || !toStack.trim()) return;
    void act(async () => {
      await api.addDep(fromStack.trim(), { hostId: toHost, stack: toStack.trim() }, fromHost);
      toStack = '';
      showAdd = false;
    });
  }

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

  // Graph geometry
  const W = 172;
  const H = 48;
  const GX = 28;
  const GY = 56;
  const PAD = 12;

  const nodeByKey = $derived(new Map((data?.nodes ?? []).map((n) => [refKey(n), n])));

  function isNodeUnhealthy(n: DepNode | undefined): boolean {
    if (!n) return false;
    const cls = nodeClass(n);
    return cls === 'down' || cls === 'degraded';
  }

  const filteredEdges = $derived.by(() => {
    let edges = data?.edges ?? [];
    if (hideIgnored) edges = edges.filter((e) => !e.ignored);
    if (hideExternal && scope === 'current') {
      edges = edges.filter((e) => e.from.hostId === app.host && e.to.hostId === app.host);
    }
    if (onlyUnhealthy) {
      edges = edges.filter((e) => isNodeUnhealthy(nodeByKey.get(refKey(e.from))) || isNodeUnhealthy(nodeByKey.get(refKey(e.to))));
    }
    return edges;
  });

  const graphNodes = $derived.by(() => {
    const linkedKeys = new Set<string>();
    for (const e of filteredEdges) {
      linkedKeys.add(refKey(e.from));
      linkedKeys.add(refKey(e.to));
    }
    return (data?.nodes ?? []).filter((n) => {
      if (hideExternal && scope === 'current' && n.external) return false;
      const isLinked = linkedKeys.has(refKey(n));
      if (!isLinked && !showStandalone) return false;
      if (onlyUnhealthy && !isLinked && !isNodeUnhealthy(n)) return false;
      return true;
    });
  });

  const alone = $derived.by(() => {
    const linkedAll = new Set<string>();
    for (const e of data?.edges ?? []) {
      if (hideIgnored && e.ignored) continue;
      linkedAll.add(refKey(e.from));
      linkedAll.add(refKey(e.to));
    }
    return (data?.nodes ?? []).filter((n) => !n.external && !linkedAll.has(refKey(n)));
  });

  const aloneText = $derived.by(() => {
    if (!alone.length) return '';
    if (scope !== 'all') return alone.map((n) => stackLabel(n.hostId, n.stack, n.displayName)).join(', ');
    const byHost = new Map<string, string[]>();
    for (const n of alone) {
      const hlabel = n.hostName || n.hostId;
      const list = byHost.get(hlabel) ?? [];
      list.push(stackLabel(n.hostId, n.stack, n.displayName));
      byHost.set(hlabel, list);
    }
    return [...byHost.entries()].map(([h, s]) => `${h}: ${s.join(', ')}`).join(' · ');
  });

  const graph = $derived(
    layoutByHost(graphNodes, filteredEdges, data?.hosts ?? [], {
      nodeW: W,
      nodeH: H,
      gapX: GX,
      gapY: GY,
    }),
  );
  const pos = $derived(new Map(graph.placed.map((p) => [refKey(p.node), p])));

  const activeFocusKey = $derived(hoverKey ?? selectedKey);
  const subtree = $derived(activeFocusKey ? computeSubtree(activeFocusKey, filteredEdges, 3) : null);
  const pinnedSubtree = $derived(selectedKey ? computeSubtree(selectedKey, data?.edges ?? [], 3) : null);
  const selectedNode = $derived(selectedKey ? (nodeByKey.get(selectedKey) ?? null) : null);
  const selectedEdge = $derived(selectedEdgeId !== null ? ((data?.edges ?? []).find((e) => e.id === selectedEdgeId) ?? null) : null);

  function edgePath(e: StackDep): string {
    const a = pos.get(refKey(e.from));
    const b = pos.get(refKey(e.to));
    if (!a || !b) return '';
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

  function href(n: { hostId: string; stack: string }): string {
    return `#/h/${encodeURIComponent(n.hostId)}/stacks/${encodeURIComponent(n.stack)}`;
  }

  function label(n: DepNode): string {
    const disp = stackLabel(n.hostId, n.stack, n.displayName);
    return n.external || scope === 'all' ? `${disp} (${n.hostName || n.hostId})` : disp;
  }

  function refDisplay(r: { hostId: string; stack: string }): string {
    const n = nodeByKey.get(refKey(r));
    if (n) return stackLabel(n.hostId, n.stack, n.displayName);
    return stackLabel(r.hostId, r.stack);
  }

  function keyLabel(k: string): string {
    const n = nodeByKey.get(k);
    if (n) return label(n);
    const [h, s] = k.split('/');
    const disp = stackLabel(h, s);
    return h === app.host && scope !== 'all' ? disp : `${disp} (${h})`;
  }

  function clip(s: string, max = 20): string {
    return s.length > max ? s.slice(0, max - 1) + '…' : s;
  }

  const sortedEdges = $derived(
    [...filteredEdges].sort(
      (a, b) => Number(a.ignored) - Number(b.ignored) || a.from.stack.localeCompare(b.from.stack) || a.to.stack.localeCompare(b.to.stack),
    ),
  );

  // Zoom & Pan controls
  function clampZoom(z: number): number {
    return Math.min(2.0, Math.max(0.4, Math.round(z * 100) / 100));
  }

  function stepZoom(delta: number) {
    zoom = clampZoom(zoom + delta);
  }

  function fitGraph() {
    if (!viewportEl || graph.width <= 0) {
      zoom = 1;
      panX = 0;
      panY = 0;
      return;
    }
    const vw = viewportEl.clientWidth - 24;
    const gw = graph.width + PAD * 2;
    const nextZ = gw > vw && vw > 120 ? clampZoom(vw / gw) : 1;
    zoom = nextZ;
    panX = 0;
    panY = 0;
  }

  function onWheel(e: WheelEvent) {
    if (!viewportEl) return;
    e.preventDefault();
    const rect = viewportEl.getBoundingClientRect();
    const cx = e.clientX - rect.left;
    const cy = e.clientY - rect.top;
    const factor = e.deltaY < 0 ? 1.1 : 0.9;
    const next = clampZoom(zoom * factor);
    if (next === zoom) return;
    const scaleRatio = next / zoom;
    panX = cx - (cx - panX) * scaleRatio;
    panY = cy - (cy - panY) * scaleRatio;
    zoom = next;
  }

  function onPointerDown(e: PointerEvent) {
    if (e.button !== 0) return;
    const target = e.target as Element | null;
    if (target?.closest('.graph-toolbar') || target?.closest('.floating-panel')) return;
    panning = true;
    dragMoved = false;
    dragStart = { x: e.clientX, y: e.clientY, px: panX, py: panY };
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
  }

  function onPointerMove(e: PointerEvent) {
    if (!panning) return;
    const dx = e.clientX - dragStart.x;
    const dy = e.clientY - dragStart.y;
    if (Math.abs(dx) > 4 || Math.abs(dy) > 4) dragMoved = true;
    panX = dragStart.px + dx;
    panY = dragStart.py + dy;
  }

  function onPointerUp(e: PointerEvent) {
    if (!panning) return;
    panning = false;
    const target = e.target as Element | null;
    if (!dragMoved && !target?.closest('.node') && !target?.closest('.edge-hit')) {
      selectedKey = null;
      selectedEdgeId = null;
    }
  }

  function selectNode(k: string, e: MouseEvent | KeyboardEvent) {
    e.preventDefault();
    e.stopPropagation();
    if (dragMoved) return;
    selectedEdgeId = null;
    selectedKey = selectedKey === k ? null : k;
  }

  function selectEdge(id: number, e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    if (dragMoved) return;
    selectedKey = null;
    selectedEdgeId = selectedEdgeId === id ? null : id;
  }
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Dependencies</h1>
      <p class="meta">
        {scope === 'all' ? 'All hosts' : (currentHost()?.name ?? app.host)} · operational dependency map, blast radius and auto-restart rules.
      </p>
    </div>
    <div class="head-actions">
      {#if app.hosts.length > 1}
        <div class="seg" role="group" aria-label="Dependency scope">
          <button type="button" class:active={scope === 'current'} onclick={() => (scope = 'current')}>Current host</button>
          <button type="button" class:active={scope === 'all'} onclick={() => (scope = 'all')}>All hosts</button>
        </div>
      {/if}
      {#if scope === 'current' && isAdmin() && currentHost()?.connected}
        <button class="btn" onclick={detect} disabled={detecting || data?.detection.running}>
          <Icon name="refresh" /> {data?.detection.running ? 'Detecting…' : 'Detect now'}
        </button>
      {/if}
      {#if isAdmin()}
        <button class="btn btn-primary" type="button" onclick={openAdd}>
          <Icon name="plus" /> Add dependency
        </button>
      {/if}
    </div>
  </section>

  {#if showAdd && isAdmin()}
    <form class="card card-pad add-card" onsubmit={submitAdd}>
      <div class="add-head">
        <span class="section-title">Add manual dependency</span>
        <button type="button" class="btn btn-sm btn-ghost" onclick={() => (showAdd = false)} aria-label="Close"><Icon name="x" /></button>
      </div>
      <div class="add-grid">
        <div class="endpoint">
          <span class="meta ep-lbl">Dependent stack</span>
          <div class="ep-fields">
            <div class="ep-host">
              <Select
                label="Dependent host"
                value={fromHost}
                items={app.hosts.map((h) => ({ value: h.id, label: h.name }))}
                onchange={(v) => (fromHost = v)}
              />
            </div>
            <div class="grow">
              <Select
                label="Dependent stack"
                items={fromOptions}
                bind:value={fromStack}
                allowCustom={true}
                placeholder="Stack name (e.g. app)"
                customPlaceholder="Filter or type stack name…"
              />
            </div>
          </div>
        </div>
        <span class="meta arrow-word">depends on →</span>
        <div class="endpoint">
          <span class="meta ep-lbl">Upstream stack</span>
          <div class="ep-fields">
            <div class="ep-host">
              <Select
                label="Upstream host"
                value={toHost}
                items={app.hosts.map((h) => ({ value: h.id, label: h.name }))}
                onchange={(v) => (toHost = v)}
              />
            </div>
            <div class="grow">
              <Select
                label="Upstream stack"
                items={toOptions}
                bind:value={toStack}
                allowCustom={true}
                placeholder="Stack it needs (e.g. postgres)"
                customPlaceholder="Filter or type stack name…"
              />
            </div>
          </div>
        </div>
      </div>
      {#if addValidation}
        <p class="error-text" role="alert">{addValidation}</p>
      {/if}
      <div class="add-actions">
        <button class="btn btn-primary" type="submit" disabled={busy || !!addValidation || !fromStack.trim() || !toStack.trim()}>
          <Icon name="plus" /> Add dependency
        </button>
        <button class="btn btn-ghost" type="button" onclick={() => (showAdd = false)}>Cancel</button>
      </div>
    </form>
  {/if}

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if data}
    {#if scope === 'current' || data.stacksError}
      <p class="meta note" class:warn={data.detection?.unsupported || !!data.detection?.error}>
        {#if data.detection?.unsupported || data.detection?.error}<Icon name="alert" />{/if}
        {#if scope === 'current'}{detectionText(data.detection)}{/if}
        {#if data.stacksError}<span> {data.stacksError}</span>{/if}
      </p>
    {/if}

    {#if data.cycles.length}
      <section class="card card-pad loop" role="alert">
        <Icon name="alert" />
        <div>
          <p class="section-title">Dependency loop</p>
          {#each data.cycles as c, i (i)}
            <p class="meta">{c.map((r) => r.stack).join(' ↔ ')} depend on each other. Their alerts and auto-restarts are skipped; remove or ignore one of the dependencies.</p>
          {/each}
        </div>
      </section>
    {/if}

    <section class="card card-pad graph-card" aria-label="Dependency graph">
      <div class="toolbar-row">
        <div class="filters" role="group" aria-label="Graph filters">
          <button type="button" class="chip" class:on={onlyUnhealthy} onclick={() => (onlyUnhealthy = !onlyUnhealthy)}>
            Only unhealthy
          </button>
          {#if scope === 'current'}
            <button type="button" class="chip" class:on={hideExternal} onclick={() => (hideExternal = !hideExternal)}>
              Hide external
            </button>
          {/if}
          <button type="button" class="chip" class:on={showStandalone} onclick={() => (showStandalone = !showStandalone)}>
            Show standalone
          </button>
          <button type="button" class="chip" class:on={hideIgnored} onclick={() => (hideIgnored = !hideIgnored)}>
            Hide ignored
          </button>
        </div>
        <div class="legend meta">
          <span><svg width="24" height="8" aria-hidden="true"><line x1="0" y1="4" x2="24" y2="4" class="e manual" /></svg> manual</span>
          <span><svg width="24" height="8" aria-hidden="true"><line x1="0" y1="4" x2="24" y2="4" class="e detected" /></svg> detected</span>
          {#if !hideIgnored}
            <span><svg width="24" height="8" aria-hidden="true"><line x1="0" y1="4" x2="24" y2="4" class="e ignored" /></svg> ignored</span>
          {/if}
          <span>arrows point at needed stack</span>
        </div>
      </div>

      {#if graphNodes.length === 0}
        <div class="empty">
          <p class="section-title">{data.edges.length === 0 ? 'No dependencies yet' : 'No stacks match the active filters'}</p>
          <p class="meta">
            {#if data.edges.length === 0}
              Stacks sharing an external Compose network or service hostname are linked automatically. Enable <strong>Show standalone</strong> to view unlinked stacks or click <strong>Add dependency</strong>.
            {:else}
              Clear one of the filters above to see the full dependency map.
            {/if}
          </p>
        </div>
      {:else}
        <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
        <div
          class="graph-viewport"
          class:panning
          bind:this={viewportEl}
          onwheel={onWheel}
          onpointerdown={onPointerDown}
          onpointermove={onPointerMove}
          onpointerup={onPointerUp}
          onpointercancel={onPointerUp}
          role="application"
          aria-label="Interactive dependency graph; pan by dragging, zoom with wheel or buttons"
        >
          <div class="graph-toolbar">
            <button type="button" class="btn btn-sm btn-icon" onclick={() => stepZoom(0.2)} title="Zoom in" aria-label="Zoom in"><Icon name="zoomIn" /></button>
            <button type="button" class="btn btn-sm btn-icon" onclick={() => stepZoom(-0.2)} title="Zoom out" aria-label="Zoom out"><Icon name="zoomOut" /></button>
            <button type="button" class="btn btn-sm" onclick={fitGraph} title="Fit graph"><Icon name="fit" /> <span class="num">{Math.round(zoom * 100)}%</span></button>
          </div>

          <svg
            class="graph"
            width={Math.max(graph.width + PAD * 2, 320)}
            height={Math.max(graph.height + PAD * 2 + 36, 220)}
          >
            <defs>
              <marker id="dep-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M0,0 L8,4 L0,8 z" class="arrow" />
              </marker>
              <marker id="dep-arrow-up" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M0,0 L8,4 L0,8 z" class="arrow-up" />
              </marker>
              <marker id="dep-arrow-down" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M0,0 L8,4 L0,8 z" class="arrow-down" />
              </marker>
            </defs>
            <g transform="translate({panX}, {panY}) scale({zoom})">
              {#each graph.boxes as b (b.host.id)}
                {@const roleTag = b.host.kind === 'local' ? 'Hub' : b.host.connected ? 'Agent' : 'Agent · offline'}
                <g class="host-box" class:offline={!b.host.connected}>
                  <rect x={b.x + PAD} y={b.y + PAD} width={b.width} height={b.height} rx="12" class="hbox" />
                  <circle cx={b.x + PAD + 16} cy={b.y + PAD + 18} r="4" class="hst" class:up={b.host.connected} />
                  <text x={b.x + PAD + 26} y={b.y + PAD + 22} class="hname">{clip(b.host.name || b.host.id, 22)}</text>
                  <text x={b.x + PAD + b.width - 14} y={b.y + PAD + 22} text-anchor="end" class="hkind">{roleTag}</text>
                </g>
              {/each}

              {#each filteredEdges as e (e.id)}
                {@const d = edgePath(e)}
                {#if d}
                  {@const inSub = subtree ? subtree.edges.has(e.id) : true}
                  {@const isUpstreamEdge = subtree ? (refKey(e.from) === subtree.focus || subtree.upstreamDirect.has(refKey(e.from)) || subtree.upstreamTransitive.has(refKey(e.from))) && (subtree.upstreamDirect.has(refKey(e.to)) || subtree.upstreamTransitive.has(refKey(e.to))) : false}
                  {@const isDownstreamEdge = subtree && inSub && !isUpstreamEdge}
                  {@const marker = !subtree || !inSub ? 'url(#dep-arrow)' : isUpstreamEdge ? 'url(#dep-arrow-up)' : 'url(#dep-arrow-down)'}
                  <g
                    class="edge-group"
                    class:faded={!!subtree && !inSub && selectedEdgeId !== e.id}
                  >
                    <path
                      {d}
                      class="e {edgeKind(e)}"
                      class:hi-up={isUpstreamEdge}
                      class:hi-down={isDownstreamEdge}
                      class:selected={selectedEdgeId === e.id}
                      marker-end={marker}
                    />
                    <!-- Wide invisible hit target so clicking an edge is effortless -->
                    <path
                      {d}
                      class="edge-hit"
                      onclick={(ev) => selectEdge(e.id, ev)}
                      role="button"
                      tabindex="0"
                      aria-label="{refDisplay(e.from)} depends on {refDisplay(e.to)}"
                      onkeydown={(ev) => { if (ev.key === 'Enter' || ev.key === ' ') selectEdge(e.id, ev as unknown as MouseEvent); }}
                    >
                      <title>{refDisplay(e.from)} depends on {refDisplay(e.to)} · {edgeText(e)}{e.autoRestart && e.autoRestart !== 'off' ? ` · ${autoRestartLabels[e.autoRestart]}` : ''}</title>
                    </path>
                  </g>
                {/if}
              {/each}

              {#each graph.placed as p (refKey(p.node))}
                {@const k = refKey(p.node)}
                {@const cls = nodeClass(p.node)}
                {@const inSub = subtree ? subtree.nodes.has(k) : true}
                {@const isPinned = selectedKey === k}
                {@const isBlast = subtree ? subtree.downstreamDirect.has(k) || subtree.downstreamTransitive.has(k) : false}
                {@const isUp = subtree ? subtree.upstreamDirect.has(k) || subtree.upstreamTransitive.has(k) : false}
                <g
                  class="node"
                  class:faded={!!subtree && !inSub}
                  class:pinned={isPinned}
                  class:blast={isBlast}
                  class:upstream={isUp}
                  role="button"
                  tabindex="0"
                  aria-pressed={isPinned}
                  aria-label="{label(p.node)}: {nodeText(p.node)}"
                  onmouseenter={() => (hoverKey = k)}
                  onmouseleave={() => { if (hoverKey === k) hoverKey = null; }}
                  onclick={(ev) => selectNode(k, ev)}
                  onkeydown={(ev) => { if (ev.key === 'Enter' || ev.key === ' ') selectNode(k, ev); }}
                >
                  <rect x={p.x + PAD} y={p.y + PAD} width={W} height={H} rx="8" class="box {cls}" class:loop={p.node.inCycle} />
                  <circle cx={p.x + PAD + 14} cy={p.y + PAD + 16} r="4" class="st {cls}" />
                  <text x={p.x + PAD + 24} y={p.y + PAD + 20} class="nname">{clip(stackLabel(p.node.hostId, p.node.stack, p.node.displayName))}</text>
                  <text x={p.x + PAD + 24} y={p.y + PAD + 36} class="nstat">
                    {clip(nodeText(p.node) + (p.node.total ? ` · ${p.node.running ?? 0}/${p.node.total}` : '') + (p.node.inCycle ? ' · loop' : ''), 25)}
                  </text>
                </g>
              {/each}
            </g>
          </svg>

          {#if selectedNode && pinnedSubtree}
            {@const blastCount = pinnedSubtree.downstreamDirect.size + pinnedSubtree.downstreamTransitive.size}
            {@const upCount = pinnedSubtree.upstreamDirect.size + pinnedSubtree.upstreamTransitive.size}
            <aside class="floating-panel" aria-label="Selected stack details">
              <div class="fp-head">
                <div>
                  <div class="fp-title">{label(selectedNode)}</div>
                  <div class="fp-sub">
                    <span class="badge {nodeClass(selectedNode)}">{nodeText(selectedNode)}</span>
                    {#if selectedNode.total !== undefined && selectedNode.total > 0}
                      <span class="meta num">{selectedNode.running ?? 0}/{selectedNode.total} containers</span>
                    {/if}
                  </div>
                </div>
                <button type="button" class="btn btn-sm btn-ghost btn-icon" onclick={() => (selectedKey = null)} aria-label="Close"><Icon name="x" /></button>
              </div>

              <div class="fp-section">
                <div class="fp-sec-lbl">Depends on (upstream · {upCount})</div>
                {#if upCount === 0}
                  <p class="meta">None</p>
                {:else}
                  <div class="fp-chips">
                    {#each [...pinnedSubtree.upstreamDirect] as k (k)}
                      <button type="button" class="fp-chip direct" onclick={() => (selectedKey = k)} title="Direct dependency">{keyLabel(k)}</button>
                    {/each}
                    {#each [...pinnedSubtree.upstreamTransitive] as k (k)}
                      <button type="button" class="fp-chip transitive" onclick={() => (selectedKey = k)} title="Transitive dependency">{keyLabel(k)} · transitive</button>
                    {/each}
                  </div>
                {/if}
              </div>

              <div class="fp-section">
                <div class="fp-sec-lbl">Used by / Blast radius ({blastCount})</div>
                {#if blastCount === 0}
                  <p class="meta">No dependent stacks impacted</p>
                {:else}
                  <div class="fp-chips">
                    {#each [...pinnedSubtree.downstreamDirect] as k (k)}
                      <button type="button" class="fp-chip blast-direct" onclick={() => (selectedKey = k)} title="Directly impacted">{keyLabel(k)}</button>
                    {/each}
                    {#each [...pinnedSubtree.downstreamTransitive] as k (k)}
                      <button type="button" class="fp-chip transitive" onclick={() => (selectedKey = k)} title="Transitively impacted (depth ≤ 3)">{keyLabel(k)} · transitive</button>
                    {/each}
                  </div>
                {/if}
              </div>

              <div class="fp-foot">
                <a class="btn btn-sm btn-primary" href={href(selectedNode)}>Open stack →</a>
                {#if isAdmin()}
                  <button type="button" class="btn btn-sm" onclick={openAdd}><Icon name="plus" /> Add dep</button>
                {/if}
              </div>
            </aside>
          {:else if selectedEdge}
            <aside class="floating-panel" aria-label="Selected dependency edge">
              <div class="fp-head">
                <div>
                  <div class="fp-title">{refDisplay(selectedEdge.from)} → {refDisplay(selectedEdge.to)}</div>
                  <div class="meta">{edgeText(selectedEdge)}{selectedEdge.detail ? ` · ${selectedEdge.detail}` : ''}</div>
                </div>
                <button type="button" class="btn btn-sm btn-ghost btn-icon" onclick={() => (selectedEdgeId = null)} aria-label="Close"><Icon name="x" /></button>
              </div>
              {#if isAdmin()}
                {#if !selectedEdge.ignored}
                  <div class="fp-field">
                    <span class="meta">Auto-restart dependent stack</span>
                    <Select
                      label="Auto-restart dependent stack"
                      value={selectedEdge.autoRestart ?? 'off'}
                      disabled={busy}
                      items={Object.entries(autoRestartLabels).map(([val, txt]) => ({ value: val, label: txt }))}
                      onchange={(v) => act(() => api.setDepAutoRestart(selectedEdge.from.stack, selectedEdge.id, v as AutoRestartMode, selectedEdge.from.hostId))}
                    />
                  </div>
                {/if}
                <div class="fp-foot">
                  {#if selectedEdge.source === 'manual'}
                    <button
                      type="button"
                      class="btn btn-sm danger-btn"
                      disabled={busy}
                      onclick={() => act(async () => { await api.removeDep(selectedEdge.from.stack, selectedEdge.id, selectedEdge.from.hostId); selectedEdgeId = null; })}
                    >
                      <Icon name="trash" /> Delete
                    </button>
                  {:else}
                    <button
                      type="button"
                      class="btn btn-sm"
                      disabled={busy}
                      onclick={() => act(() => api.ignoreDep(selectedEdge.from.stack, selectedEdge.id, !selectedEdge.ignored, selectedEdge.from.hostId))}
                    >
                      {selectedEdge.ignored ? 'Unignore' : 'Ignore'}
                    </button>
                  {/if}
                </div>
              {:else if selectedEdge.autoRestart && selectedEdge.autoRestart !== 'off'}
                <p class="meta">Auto-restart: {autoRestartLabels[selectedEdge.autoRestart]}</p>
              {/if}
            </aside>
          {/if}
        </div>
      {/if}
    </section>

    {#if sortedEdges.length}
      <section class="card list" aria-label="Dependencies list">
        {#each sortedEdges as e (e.id)}
          {@const from = nodeByKey.get(refKey(e.from))}
          {@const to = nodeByKey.get(refKey(e.to))}
          <div class="row" class:dim={e.ignored}>
            <div class="row-main">
              <span class="pair">
                <a href={href(e.from)}>{from ? label(from) : e.from.stack}</a>
                <span class="badge {nodeClass(from)}">{nodeText(from)}</span>
                <span class="meta">depends on</span>
                <a href={href(e.to)}>{to ? label(to) : e.to.stack}</a>
                <span class="badge {nodeClass(to)}">{nodeText(to)}</span>
              </span>
              <span class="meta sub">
                {edgeText(e)}{e.detail ? ` · ${e.detail}` : ''}
                {#if !isAdmin() && e.autoRestart && e.autoRestart !== 'off'}
                  · {autoRestartLabels[e.autoRestart]}
                {/if}
              </span>
            </div>
            {#if isAdmin()}
              <div class="row-actions">
                {#if !e.ignored}
                  <div class="auto-sel">
                    <Select
                      label="Auto-restart policy for {refDisplay(e.from)}"
                      compact={true}
                      value={e.autoRestart ?? 'off'}
                      disabled={busy}
                      items={Object.entries(autoRestartLabels).map(([val, txt]) => ({ value: val, label: txt }))}
                      onchange={(v) => act(() => api.setDepAutoRestart(e.from.stack, e.id, v as AutoRestartMode, e.from.hostId))}
                    />
                  </div>
                {/if}
                {#if e.source === 'manual'}
                  <button
                    type="button"
                    class="btn btn-sm danger-btn"
                    disabled={busy}
                    onclick={() => act(() => api.removeDep(e.from.stack, e.id, e.from.hostId))}
                  >
                    Delete
                  </button>
                {:else if e.ignored}
                  <button
                    type="button"
                    class="btn btn-sm"
                    disabled={busy}
                    onclick={() => act(() => api.ignoreDep(e.from.stack, e.id, false, e.from.hostId))}
                  >
                    Unignore
                  </button>
                {:else}
                  <button
                    type="button"
                    class="btn btn-sm btn-ghost"
                    disabled={busy}
                    onclick={() => act(() => api.ignoreDep(e.from.stack, e.id, true, e.from.hostId))}
                  >
                    Ignore
                  </button>
                {/if}
              </div>
            {/if}
          </div>
        {/each}
      </section>
    {/if}

    {#if alone.length && !showStandalone}
      <p class="meta note">Standalone stacks without dependencies: {aloneText}</p>
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
  .head-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .seg {
    display: inline-flex;
    padding: 2px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .seg button {
    border: 0;
    background: transparent;
    color: var(--mist);
    font: 500 12px var(--font);
    padding: 5px 10px;
    border-radius: 6px;
    cursor: pointer;
  }
  .seg button.active {
    background: var(--pier-raised);
    color: var(--foam);
    box-shadow: 0 0 0 1px var(--rope-strong);
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .add-card {
    display: grid;
    gap: 12px;
  }
  .add-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .add-grid {
    display: flex;
    align-items: flex-end;
    gap: 12px;
    flex-wrap: wrap;
  }
  .endpoint {
    flex: 1 1 260px;
    display: grid;
    gap: 4px;
  }
  .ep-lbl {
    font-size: 12px;
  }
  .ep-fields {
    display: flex;
    gap: 8px;
  }
  .ep-fields .ep-host {
    min-width: 130px;
  }
  .ep-fields .grow {
    flex: 1;
    min-width: 140px;
  }
  .arrow-word {
    padding-bottom: 8px;
    font-weight: 500;
  }
  .add-actions {
    display: flex;
    gap: 8px;
    align-items: center;
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
    padding: 16px 4px;
  }
  .empty .meta {
    max-width: 64ch;
  }
  .graph-card {
    display: grid;
    gap: 12px;
  }
  .toolbar-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .filters {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .chip {
    height: 28px;
    padding: 0 10px;
    border-radius: 6px;
    border: 0;
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope-strong);
    color: var(--mist);
    font: 500 12px var(--font);
    cursor: pointer;
  }
  .chip:hover {
    color: var(--foam);
    box-shadow: inset 0 0 0 1px var(--rope-stronger);
  }
  .chip.on {
    background: var(--buoy-wash);
    color: var(--foam);
    box-shadow: inset 0 0 0 1px var(--buoy);
  }
  .legend {
    display: flex;
    gap: 14px;
    flex-wrap: wrap;
    align-items: center;
    font-size: 12px;
  }
  .legend span {
    display: inline-flex;
    gap: 6px;
    align-items: center;
  }
  .graph-viewport {
    position: relative;
    overflow: hidden;
    min-height: 280px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    cursor: grab;
    touch-action: none;
    user-select: none;
  }
  .graph-viewport.panning {
    cursor: grabbing;
  }
  .graph-toolbar {
    position: absolute;
    top: 10px;
    right: 10px;
    z-index: 3;
    display: inline-flex;
    gap: 4px;
    padding: 4px;
    border-radius: var(--r-md);
    background: var(--pier-raised);
    box-shadow: 0 0 0 1px var(--rope-strong);
  }
  .graph {
    display: block;
    min-width: 100%;
  }
  .e {
    fill: none;
    stroke-width: 1.5;
    pointer-events: none;
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
  .e.hi-up {
    stroke: var(--buoy);
    stroke-width: 2.25;
  }
  .e.hi-down {
    stroke: var(--signal);
    stroke-width: 2.25;
  }
  .e.selected {
    stroke: var(--buoy);
    stroke-width: 2.5;
  }
  .edge-hit {
    fill: none;
    stroke: transparent;
    stroke-width: 14;
    cursor: pointer;
  }
  .arrow {
    fill: var(--haze);
  }
  .arrow-up {
    fill: var(--buoy);
  }
  .arrow-down {
    fill: var(--signal);
  }
  .node {
    cursor: pointer;
    transition: opacity 150ms cubic-bezier(0.23, 1, 0.32, 1);
  }
  .edge-group {
    transition: opacity 150ms cubic-bezier(0.23, 1, 0.32, 1);
  }
  .node.faded,
  .edge-group.faded {
    opacity: 0.2;
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
  .node:focus-visible .box,
  .node.pinned .box {
    stroke: var(--buoy);
    stroke-width: 2;
  }
  .node.blast .box {
    stroke: var(--signal);
    stroke-width: 1.75;
  }
  .node.upstream .box {
    stroke: var(--buoy-text);
    stroke-width: 1.75;
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
    font-variant-numeric: tabular-nums;
  }
  .hbox {
    fill: var(--pier);
    stroke: var(--rope-strong);
    stroke-width: 1;
  }
  .host-box.offline .hbox {
    stroke-dasharray: 4 3;
    opacity: 0.75;
  }
  .hst {
    fill: var(--becalmed);
  }
  .hst.up {
    fill: var(--afloat);
  }
  .hname {
    fill: var(--foam);
    font: 600 12px var(--font);
  }
  .hkind {
    fill: var(--haze);
    font: 500 11px var(--font);
  }
  .floating-panel {
    position: absolute;
    left: 12px;
    bottom: 12px;
    z-index: 4;
    width: min(340px, calc(100% - 24px));
    padding: 14px 16px;
    border-radius: var(--r-lg);
    background: var(--pier-raised);
    box-shadow: 0 0 0 1px var(--rope-stronger);
    display: grid;
    gap: 10px;
    cursor: default;
  }
  .fp-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 8px;
  }
  .fp-title {
    font-size: 14px;
    font-weight: 600;
    color: var(--foam);
  }
  .fp-sub {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-top: 4px;
    flex-wrap: wrap;
  }
  .fp-section {
    display: grid;
    gap: 6px;
    padding-top: 8px;
    border-top: 1px solid var(--rope);
  }
  .fp-sec-lbl {
    font-size: 11px;
    font-weight: 600;
    color: var(--haze);
    font-variant-numeric: tabular-nums;
  }
  .fp-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .fp-chip {
    border: 0;
    border-radius: 6px;
    padding: 3px 8px;
    font: 500 12px var(--font);
    background: var(--well);
    color: var(--foam);
    box-shadow: inset 0 0 0 1px var(--rope-strong);
    cursor: pointer;
  }
  .fp-chip.direct {
    box-shadow: inset 0 0 0 1px var(--buoy);
  }
  .fp-chip.blast-direct {
    box-shadow: inset 0 0 0 1px var(--signal);
  }
  .fp-chip.transitive {
    color: var(--mist);
  }
  .fp-field {
    display: grid;
    gap: 4px;
  }
  .fp-foot {
    display: flex;
    gap: 8px;
    align-items: center;
    padding-top: 4px;
  }
  .list {
    padding: 4px 0;
  }
  .row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
    padding: 12px 20px;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .row.dim {
    opacity: 0.68;
  }
  .row-main {
    display: grid;
    gap: 4px;
    min-width: 0;
    flex: 1 1 260px;
  }
  .row-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .auto-sel {
    width: auto;
    height: 28px;
    padding: 0 8px;
    font-size: 12px;
  }
  .danger-btn {
    color: var(--adrift-text, var(--adrift));
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
  @media (prefers-reduced-motion: reduce) {
    .node,
    .edge-group {
      transition: none;
    }
  }
  @media (max-width: 640px) {
    .graph-card {
      display: none;
    }
    .row {
      padding: 12px 14px;
    }
  }
</style>
