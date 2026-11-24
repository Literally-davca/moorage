<script lang="ts">
  import { api, hostPath, wsURL, type Container, type ContainerInfo, type LogLine } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';
  import {
    ansiClass,
    parseAnsi,
    parseLogLine,
    sourceBadgeTone,
    stripAnsi,
    type ParsedLogLine,
  } from '../lib/logparse';

  let {
    stack = '',
    containers = [],
    hostContainers,
    paneLabel = '',
  }: {
    stack?: string;
    containers?: Container[];
    hostContainers?: ContainerInfo[];
    paneLabel?: string;
  } = $props();

  interface Line extends LogLine {
    k: number;
    parsed: ParsedLogLine;
  }

  type RangePreset = 'live' | '15m' | '1h' | '6h' | '24h' | 'custom';
  type LevelFilter = 'all' | 'error' | 'warn' | 'info' | 'debug';

  const MAX = 5000;

  let lines = $state<Line[]>([]);
  let picked = $state<string[]>([]);
  const container = $derived([...picked].sort().join(','));
  let query = $state('');
  let useRegex = $state(true);
  let wrap = $state(true);
  let follow = $state(true);
  let levelFilter = $state<LevelFilter>('all');
  let stderrOnly = $state(false);
  let rangePreset = $state<RangePreset>('live');
  let customFrom = $state('');
  let customTo = $state('');
  let ended = $state('');
  let connected = $state(false);
  let loadingOlder = $state(false);
  let noMoreOlder = $state(false);
  let expandedKeys = $state<Record<number, boolean>>({});
  let copiedKey = $state('');
  let box: HTMLDivElement;
  let seq = 0;

  const selectItems = $derived.by(() => {
    if (hostContainers) {
      return hostContainers.map((c) => {
        const grp = c.project || 'standalone';
        const svc = c.service || c.name;
        return {
          value: c.name,
          label: c.project ? `${c.project} / ${svc}` : svc,
          meta: `${grp} · ${c.state}`,
        };
      });
    }
    return containers.map((c) => ({
      value: c.name,
      label: c.service || c.name,
      meta: c.service ? c.name : undefined,
    }));
  });

  const totalSourceCount = $derived(hostContainers ? hostContainers.length : new Set(containers.map((c) => c.name)).size);
  const multi = $derived((totalSourceCount > 1 || !stack) && picked.length !== 1);

  const timeBounds = $derived.by(() => {
    if (rangePreset === 'live') {
      return { since: '', until: '', follow: true };
    }
    if (rangePreset === 'custom') {
      const since = customFrom ? new Date(customFrom).toISOString() : '';
      const until = customTo ? new Date(customTo).toISOString() : '';
      return { since, until, follow: !until };
    }
    const mins =
      rangePreset === '15m' ? 15 : rangePreset === '1h' ? 60 : rangePreset === '6h' ? 360 : 1440;
    const since = new Date(Date.now() - mins * 60_000).toISOString();
    return { since, until: '', follow: true };
  });

  const matcher = $derived.by(() => {
    const q = query.trim();
    if (!q) return null;
    if (useRegex) {
      try {
        return new RegExp(q, 'gi');
      } catch {
        return 'invalid' as const;
      }
    }
    return new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'gi');
  });

  const visible = $derived.by(() => {
    return lines.filter((l) => {
      if (stderrOnly && l.s !== 'stderr') return false;
      if (levelFilter !== 'all' && l.parsed.level !== levelFilter) return false;
      if (matcher && matcher !== 'invalid') {
        matcher.lastIndex = 0;
        const hay = l.parsed.isJson ? `${l.parsed.summary} ${stripAnsi(l.m)}` : stripAnsi(l.m);
        if (!matcher.test(hay)) return false;
      }
      return true;
    });
  });

  function parts(m: string): { s: string; hit: boolean }[] {
    if (!matcher || matcher === 'invalid' || !m) return m ? [{ s: m, hit: false }] : [];
    const out: { s: string; hit: boolean }[] = [];
    let last = 0;
    matcher.lastIndex = 0;
    for (const x of m.matchAll(matcher)) {
      if (x[0] === '') break;
      const i = x.index ?? 0;
      if (i > last) out.push({ s: m.slice(last, i), hit: false });
      out.push({ s: x[0], hit: true });
      last = i + x[0].length;
    }
    if (last < m.length) out.push({ s: m.slice(last), hit: false });
    return out;
  }

  function formatTime(ts: string): string {
    if (!ts) return '';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return '';
    if (rangePreset === '24h' || rangePreset === 'custom') {
      return d.toLocaleString([], { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
    }
    return d.toLocaleTimeString([], { hour12: false });
  }

  function sourceLabel(l: LogLine): string {
    if (!stack && l.p) {
      return `${l.p}/${l.svc || l.c}`;
    }
    if (l.svc && stack) {
      return l.svc;
    }
    return l.c;
  }

  function toLine(l: LogLine): Line {
    return {
      ...l,
      k: ++seq,
      parsed: parseLogLine(l.m, l.s),
    };
  }

  // Reconnect only when stack, selected containers, or time range change.
  const target = $derived(JSON.stringify([stack, container, timeBounds.since, timeBounds.until, timeBounds.follow]));

  $effect(() => {
    const [stackName, ctr, since, until, shouldFollow] = JSON.parse(target) as [
      string,
      string,
      string,
      string,
      boolean,
    ];
    lines = [];
    expandedKeys = {};
    ended = '';
    noMoreOlder = false;

    if (rangePreset === 'custom' && !since && !until) {
      connected = false;
      ended = 'Choose a start or end time above.';
      return;
    }

    const params = new URLSearchParams();
    if (stackName) params.set('stack', stackName);
    if (ctr) params.set('container', ctr);
    if (since) params.set('since', since);
    else params.set('tail', '300');
    if (until) params.set('until', until);
    if (!shouldFollow) params.set('follow', '0');

    follow = shouldFollow;
    const ws = new WebSocket(wsURL(hostPath('/ws/logs?' + params.toString())));
    ws.onopen = () => (connected = true);
    ws.onclose = () => (connected = false);
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data) as { lines?: LogLine[]; end?: string };
      if (msg.lines?.length) {
        const add = msg.lines.map(toLine);
        const next = lines.concat(add);
        lines = next.length > MAX ? next.slice(next.length - MAX) : next;
      }
      if (msg.end) ended = msg.end;
    };
    return () => ws.close();
  });

  $effect(() => {
    void visible.length;
    if (follow) queueMicrotask(() => box && (box.scrollTop = box.scrollHeight));
  });

  function onScroll() {
    if (!box) return;
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
    if (timeBounds.follow && follow !== atBottom) follow = atBottom;
  }

  function jump() {
    follow = true;
    if (box) box.scrollTop = box.scrollHeight;
  }

  async function loadOlderLines() {
    if (loadingOlder || lines.length === 0) return;
    const oldest = lines.find((l) => l.t)?.t;
    if (!oldest) return;
    loadingOlder = true;
    const prevHeight = box?.scrollHeight ?? 0;
    const prevTop = box?.scrollTop ?? 0;
    try {
      const d = new Date(oldest);
      const untilIso = !isNaN(d.getTime()) ? new Date(d.getTime() - 1).toISOString() : oldest;
      const res = await api.logsHistory({
        stack: stack || undefined,
        container: container || undefined,
        until: untilIso,
        tail: 300,
      });
      const existingSig = new Set(lines.slice(0, 400).map((l) => `${l.t}|${l.c}|${l.m}`));
      const fresh = (res.lines ?? []).filter((l) => !existingSig.has(`${l.t}|${l.c}|${l.m}`)).map(toLine);
      if (fresh.length === 0) {
        noMoreOlder = true;
      } else {
        follow = false;
        const combined = fresh.concat(lines);
        lines = combined.length > MAX ? combined.slice(0, MAX) : combined;
        queueMicrotask(() => {
          if (box) {
            box.scrollTop = prevTop + (box.scrollHeight - prevHeight);
          }
        });
      }
    } catch {
      noMoreOlder = true;
    } finally {
      loadingOlder = false;
    }
  }

  function toggleExpand(k: number) {
    expandedKeys = { ...expandedKeys, [k]: !expandedKeys[k] };
  }

  async function copyField(id: string, val: string) {
    try {
      await navigator.clipboard.writeText(val);
      copiedKey = id;
      setTimeout(() => {
        if (copiedKey === id) copiedKey = '';
      }, 1500);
    } catch {
      /* ignore */
    }
  }

  const downloadHref = $derived(
    api.logsDownloadUrl({
      stack: stack || undefined,
      container: container || undefined,
      since: timeBounds.since || undefined,
      until: timeBounds.until || undefined,
    }),
  );

  const levels: { id: LevelFilter; label: string }[] = [
    { id: 'all', label: 'All' },
    { id: 'error', label: 'Error' },
    { id: 'warn', label: 'Warn' },
    { id: 'info', label: 'Info' },
    { id: 'debug', label: 'Debug' },
  ];
</script>

<div class="viewer">
  <div class="bar">
    {#if paneLabel}
      <span class="pane-tag">{paneLabel}</span>
    {/if}
    <div class="sel">
      <MultiSelect
        label="Containers"
        placeholder={stack ? 'All containers' : 'All host containers'}
        bind:selected={picked}
        items={selectItems}
      />
    </div>
    <label class="find" class:bad={matcher === 'invalid'}>
      <Icon name="search" />
      <span class="sr-only">Search logs</span>
      <input
        bind:value={query}
        placeholder={useRegex ? 'Filter regex, e.g. error|timeout' : 'Filter text'}
        spellcheck="false"
      />
      <button
        type="button"
        class="re"
        class:on={useRegex}
        onclick={() => (useRegex = !useRegex)}
        title="Regular expression"
        aria-pressed={useRegex}
      >.*</button>
    </label>

    <label class="range-sel">
      <Icon name="clock" />
      <span class="sr-only">Time range</span>
      <select bind:value={rangePreset} aria-label="Time range">
        <option value="live">Live (tail 300)</option>
        <option value="15m">Last 15m</option>
        <option value="1h">Last 1h</option>
        <option value="6h">Last 6h</option>
        <option value="24h">Last 24h</option>
        <option value="custom">From – To…</option>
      </select>
    </label>

    <button
      type="button"
      class="btn btn-ghost btn-icon"
      class:on={wrap}
      onclick={() => (wrap = !wrap)}
      title="Wrap lines"
      aria-pressed={wrap}
    ><Icon name="wrap" /></button>
    <a
      class="btn btn-ghost btn-icon"
      href={downloadHref}
      download
      title="Download .log (max 50 MB)"
      aria-label="Download logs"
    ><Icon name="download" /></a>
    <button
      type="button"
      class="btn btn-ghost btn-icon"
      onclick={() => {
        lines = [];
        expandedKeys = {};
      }}
      title="Clear view"
      aria-label="Clear view"
    ><Icon name="eraser" /></button>
  </div>

  <div class="subbar">
    <div class="lvl-pills" role="group" aria-label="Log level filter">
      {#each levels as lv (lv.id)}
        <button
          type="button"
          class="pill"
          class:active={levelFilter === lv.id}
          data-lv={lv.id}
          onclick={() => (levelFilter = lv.id)}
        >{lv.label}</button>
      {/each}
      <button
        type="button"
        class="pill stderr-pill"
        class:active={stderrOnly}
        aria-pressed={stderrOnly}
        onclick={() => (stderrOnly = !stderrOnly)}
      >stderr only</button>
    </div>

    {#if rangePreset === 'custom'}
      <div class="custom-range">
        <label class="meta">
          From
          <input type="datetime-local" class="dt-input mono" bind:value={customFrom} />
        </label>
        <label class="meta">
          To
          <input type="datetime-local" class="dt-input mono" bind:value={customTo} />
        </label>
      </div>
    {/if}
  </div>

  <div class="log mono" class:wrap bind:this={box} onscroll={onScroll} role="log" aria-label="Container logs">
    {#if lines.length > 0}
      <div class="older-bar">
        {#if noMoreOlder}
          <span class="meta">Beginning of available log history</span>
        {:else}
          <button type="button" class="btn btn-sm btn-ghost" onclick={loadOlderLines} disabled={loadingOlder}>
            <Icon name="arrowUp" /> {loadingOlder ? 'Loading older lines…' : 'Load older lines'}
          </button>
        {/if}
      </div>
    {/if}

    {#each visible as l (l.k)}
      {@const src = sourceLabel(l)}
      {@const open = !!expandedKeys[l.k]}
      <div class="ln-group">
        <div
          class="ln"
          class:err={l.s === 'stderr' || l.parsed.level === 'error'}
          class:warn={l.parsed.level === 'warn'}
          class:json-ln={l.parsed.isJson}
        >
          <span class="ts">{formatTime(l.t)}</span>
          {#if multi}
            <span class="ctr tone-{sourceBadgeTone(src)}" title={src}>{src}</span>
          {/if}
          {#if l.parsed.level !== 'other'}
            <span class="lvl lvl-{l.parsed.level}">{l.parsed.level.slice(0, 4).toUpperCase()}</span>
          {/if}
          {#if l.parsed.isJson}
            <button
              type="button"
              class="json-toggle"
              aria-expanded={open}
              onclick={() => toggleExpand(l.k)}
              title={open ? 'Hide JSON fields' : 'Inspect JSON fields'}
            >{open ? '▾ JSON' : '▸ JSON'}</button>
            <span class="msg">
              {#each parts(l.parsed.summary) as p}
                {#if p.hit}<mark>{p.s}</mark>{:else}{p.s}{/if}
              {/each}
            </span>
          {:else}
            <span class="msg">
              {#each parseAnsi(l.m) as seg}
                <span class={ansiClass(seg)}>{#each parts(seg.text) as p}{#if p.hit}<mark>{p.s}</mark>{:else}{p.s}{/if}{/each}</span>
              {/each}
            </span>
          {/if}
        </div>

        {#if l.parsed.isJson && open && l.parsed.fields}
          <div class="json-table">
            {#each l.parsed.fields as f (f.key)}
              {@const fid = `${l.k}:${f.key}`}
              <div class="jf-row">
                <span class="jf-key">{f.key}</span>
                <span class="jf-val">{f.value}</span>
                <button
                  type="button"
                  class="jf-copy"
                  onclick={() => copyField(fid, f.value)}
                  title="Copy value"
                >{copiedKey === fid ? 'Copied' : 'Copy'}</button>
              </div>
            {/each}
          </div>
        {/if}
      </div>
    {/each}

    {#if lines.length === 0}
      <p class="quiet">{ended || (connected ? 'Waiting for log lines…' : 'Connecting…')}</p>
    {:else if visible.length === 0}
      <p class="quiet">No log lines match the active filter.</p>
    {/if}
  </div>

  <div class="status">
    <span class="meta num">
      {visible.length !== lines.length ? `${visible.length} of ${lines.length}` : lines.length} lines{lines.length >= MAX ? ` · showing up to ${MAX}` : ''}
    </span>
    {#if connected && timeBounds.follow}
      <span class="live-ind"><span class="dot up"></span> Live</span>
    {:else if ended && lines.length}
      <span class="meta">{ended}</span>
    {/if}
    {#if !follow && timeBounds.follow && lines.length > 0}
      <button type="button" class="btn btn-sm jump" onclick={jump}><Icon name="arrowDown" /> Jump to latest</button>
    {/if}
  </div>
</div>

<style>
  .viewer {
    display: grid;
    gap: 8px;
    min-width: 0;
  }
  .bar {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    align-items: center;
  }
  .pane-tag {
    font: 600 11px var(--mono);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    padding: 4px 8px;
    border-radius: var(--r-sm);
    background: var(--buoy-wash);
    color: var(--buoy-text);
  }
  .sel {
    width: 220px;
    max-width: 100%;
  }
  .sel :global(.trigger) {
    height: 32px;
    font-size: 13px;
  }
  .find {
    flex: 1;
    min-width: 180px;
    display: flex;
    align-items: center;
    gap: 8px;
    height: 32px;
    padding: 0 4px 0 10px;
    border-radius: var(--r-md);
    background: var(--well);
    color: var(--murk);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .find:focus-within,
  .range-sel:focus-within {
    box-shadow:
      0 0 0 2px var(--buoy),
      0 0 0 5px var(--buoy-wash);
  }
  .find.bad {
    box-shadow: inset 0 0 0 1px var(--adrift);
  }
  .find input {
    flex: 1;
    min-width: 0;
    border: 0;
    background: transparent;
    color: var(--foam);
    font: 400 13px var(--mono);
    outline: none;
  }
  .range-sel {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 32px;
    padding: 0 8px;
    border-radius: var(--r-md);
    background: var(--well);
    color: var(--mist);
    box-shadow: inset 0 0 0 1px var(--rope);
    font-size: 12px;
  }
  .range-sel select {
    border: 0;
    background: transparent;
    color: var(--foam);
    font: 500 12px var(--font);
    outline: none;
    cursor: pointer;
  }
  .range-sel select option {
    background: var(--hull);
    color: var(--foam);
  }
  .re {
    height: 24px;
    padding: 0 8px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--haze);
    font: 500 12px var(--mono);
    cursor: pointer;
  }
  .re.on,
  .btn.on {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .subbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    flex-wrap: wrap;
  }
  .lvl-pills {
    display: flex;
    align-items: center;
    gap: 4px;
    flex-wrap: wrap;
  }
  .pill {
    height: 26px;
    padding: 0 10px;
    border: 0;
    border-radius: 999px;
    background: var(--well);
    color: var(--mist);
    font: 500 12px var(--font);
    box-shadow: inset 0 0 0 1px var(--rope);
    cursor: pointer;
  }
  .pill:hover {
    color: var(--foam);
    background: var(--rope);
  }
  .pill.active {
    background: var(--rope-strong);
    color: var(--foam);
    box-shadow: inset 0 0 0 1px var(--rope-stronger);
  }
  .pill.active[data-lv='error'],
  .stderr-pill.active {
    background: var(--adrift-wash);
    color: var(--adrift-text);
    box-shadow: inset 0 0 0 1px var(--adrift);
  }
  .pill.active[data-lv='warn'] {
    background: var(--signal-wash);
    color: var(--signal-text);
    box-shadow: inset 0 0 0 1px var(--signal);
  }
  .pill.active[data-lv='info'] {
    background: var(--info-wash);
    color: var(--info-text);
    box-shadow: inset 0 0 0 1px var(--info);
  }
  .custom-range {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .custom-range label {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .dt-input {
    height: 28px;
    padding: 0 8px;
    border-radius: var(--r-sm);
    border: 1px solid var(--rope);
    background: var(--well);
    color: var(--foam);
    font-size: 12px;
  }
  .log {
    height: min(64vh, 660px);
    overflow: auto;
    padding: 8px 0;
    border-radius: var(--r-card);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    font-size: 12px;
    line-height: 1.65;
  }
  .older-bar {
    display: flex;
    justify-content: center;
    padding: 4px 16px 8px;
    border-bottom: 1px solid var(--rope);
    margin-bottom: 4px;
    font-family: var(--font);
  }
  .ln-group {
    border-bottom: 1px solid transparent;
  }
  .ln {
    display: flex;
    align-items: baseline;
    gap: 8px;
    padding: 1px 14px;
    white-space: pre;
  }
  .wrap .ln {
    white-space: pre-wrap;
    word-break: break-word;
  }
  .ln:hover {
    background: var(--rope);
  }
  .ts {
    color: var(--murk);
    flex: none;
    font-size: 11px;
  }
  .ctr {
    flex: none;
    max-width: 180px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    padding: 0 6px;
    border-radius: 4px;
    font-size: 11px;
    font-weight: 500;
    background: var(--rope);
    color: var(--foam);
  }
  .ctr.tone-0 {
    background: var(--info-wash);
    color: var(--info-text);
  }
  .ctr.tone-1 {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .ctr.tone-2 {
    background: var(--buoy-wash);
    color: var(--buoy-text);
  }
  .ctr.tone-3 {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .ctr.tone-4 {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .ctr.tone-5 {
    background: var(--hull);
    color: var(--mist);
    box-shadow: inset 0 0 0 1px var(--rope-strong);
  }
  .lvl {
    flex: none;
    font-size: 10px;
    font-weight: 600;
    padding: 0 5px;
    border-radius: 4px;
    letter-spacing: 0.03em;
  }
  .lvl-error {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .lvl-warn {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .lvl-info {
    background: var(--info-wash);
    color: var(--info-text);
  }
  .lvl-debug {
    background: var(--rope);
    color: var(--haze);
  }
  .json-toggle {
    flex: none;
    border: 0;
    border-radius: 4px;
    padding: 0 6px;
    height: 18px;
    background: var(--rope);
    color: var(--mist);
    font: 500 10px var(--mono);
    cursor: pointer;
  }
  .json-toggle:hover {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .msg {
    color: var(--foam);
    min-width: 0;
    flex: 1;
  }
  .err .msg {
    color: var(--adrift-text);
  }
  .warn .msg {
    color: var(--signal-text);
  }
  /* Safe ANSI SGR token classes */
  .ansi-red {
    color: var(--adrift-text);
  }
  .ansi-green {
    color: var(--afloat-text);
  }
  .ansi-yellow {
    color: var(--signal-text);
  }
  .ansi-blue,
  .ansi-cyan {
    color: var(--info-text);
  }
  .ansi-magenta {
    color: var(--buoy-text);
  }
  .ansi-gray {
    color: var(--haze);
  }
  .ansi-white {
    color: var(--foam);
  }
  .ansi-bold {
    font-weight: 600;
  }
  .ansi-dim {
    opacity: 0.72;
  }
  .ansi-italic {
    font-style: italic;
  }
  .ansi-underline {
    text-decoration: underline;
  }

  .json-table {
    margin: 4px 14px 8px 36px;
    padding: 8px 12px;
    border-radius: var(--r-md);
    background: var(--hull);
    box-shadow: inset 0 0 0 1px var(--rope);
    display: grid;
    gap: 4px;
  }
  .jf-row {
    display: grid;
    grid-template-columns: minmax(110px, 180px) minmax(0, 1fr) auto;
    gap: 8px;
    align-items: baseline;
    padding: 2px 0;
    border-bottom: 1px solid var(--rope);
  }
  .jf-row:last-child {
    border-bottom: 0;
  }
  .jf-key {
    color: var(--haze);
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .jf-val {
    color: var(--foam);
    word-break: break-all;
    white-space: pre-wrap;
  }
  .jf-copy {
    border: 0;
    border-radius: var(--r-sm);
    padding: 2px 8px;
    background: var(--well);
    color: var(--mist);
    font: 500 11px var(--font);
    cursor: pointer;
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .jf-copy:hover {
    color: var(--foam);
    background: var(--rope);
  }

  mark {
    background: var(--buoy-wash);
    color: var(--buoy-text);
    border-radius: 3px;
    box-shadow: 0 0 0 1px var(--buoy-wash);
  }
  .quiet {
    padding: 12px 16px;
    color: var(--murk);
    font-family: var(--font);
  }
  .status {
    display: flex;
    align-items: center;
    gap: 12px;
    min-height: 28px;
  }
  .live-ind {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--afloat-text);
  }
  .jump {
    margin-left: auto;
  }
</style>
