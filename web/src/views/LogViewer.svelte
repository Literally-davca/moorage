<script lang="ts">
  import { hostPath, wsURL, type Container } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';

  let { stack, containers }: { stack: string; containers: Container[] } = $props();

  interface Line {
    k: number;
    t: string;
    c: string;
    s: string;
    m: string;
  }
  const MAX = 5000;

  let lines = $state<Line[]>([]);
  // Chosen container names; empty = all of the stack.
  let picked = $state<string[]>([]);
  const container = $derived([...picked].sort().join(','));
  let query = $state('');
  let useRegex = $state(true);
  let wrap = $state(true);
  let follow = $state(true);
  let ended = $state('');
  let connected = $state(false);
  let box: HTMLDivElement;
  let seq = 0;

  const multi = $derived(new Set(containers.map((c) => c.name)).size > 1 && picked.length !== 1);

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
    if (!matcher || matcher === 'invalid') return lines;
    const re = matcher;
    return lines.filter((l) => {
      re.lastIndex = 0;
      return re.test(l.m);
    });
  });

  function parts(m: string): { s: string; hit: boolean }[] {
    if (!matcher || matcher === 'invalid') return [{ s: m, hit: false }];
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

  function time(ts: string): string {
    // 2026-11-20T14:05:40.123456789Z -> 14:05:40 in local time
    const d = new Date(ts);
    return isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour12: false });
  }

  // A derived string compares by value, so parent re-renders (5 s polling) don't reconnect the stream.
  const target = $derived(JSON.stringify([stack, container]));

  $effect(() => {
    const [stackName, ctr] = JSON.parse(target) as [string, string];
    lines = [];
    ended = '';
    const params = new URLSearchParams({ stack: stackName, tail: '300' });
    if (ctr) params.set('container', ctr);
    const ws = new WebSocket(wsURL(hostPath('/ws/logs?' + params)));
    ws.onopen = () => (connected = true);
    ws.onclose = () => (connected = false);
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data) as { lines?: Omit<Line, 'k'>[]; end?: string };
      if (msg.lines) {
        const add = msg.lines.map((l) => ({ ...l, k: seq++ }));
        const next = lines.concat(add);
        lines = next.length > MAX ? next.slice(next.length - MAX) : next;
      }
      if (msg.end) ended = msg.end;
    };
    return () => ws.close();
  });

  $effect(() => {
    visible;
    if (follow) queueMicrotask(() => box && (box.scrollTop = box.scrollHeight));
  });

  function onScroll() {
    const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
    if (follow !== atBottom) follow = atBottom;
  }

  function jump() {
    follow = true;
    box.scrollTop = box.scrollHeight;
  }
</script>

<div class="viewer">
  <div class="bar">
    <div class="sel">
      <MultiSelect label="Containers" placeholder="All containers" bind:selected={picked}
        items={containers.map((c) => ({ value: c.name, label: c.service || c.name, meta: c.service ? c.name : undefined }))} />
    </div>
    <label class="find" class:bad={matcher === 'invalid'}>
      <Icon name="search" />
      <span class="sr-only">Search logs</span>
      <input bind:value={query} placeholder={useRegex ? 'Search with a regex, e.g. error|fatal' : 'Search text'} spellcheck="false" />
      <button class="re" class:on={useRegex} onclick={() => (useRegex = !useRegex)} title="Regular expression" aria-pressed={useRegex}>.*</button>
    </label>
    <button class="btn btn-ghost btn-icon" class:on={wrap} onclick={() => (wrap = !wrap)} title="Wrap lines" aria-pressed={wrap}><Icon name="wrap" /></button>
    <button class="btn btn-ghost btn-icon" onclick={() => (lines = [])} title="Clear" aria-label="Clear"><Icon name="eraser" /></button>
  </div>

  <div class="log mono" class:wrap bind:this={box} onscroll={onScroll} role="log" aria-label="Container logs">
    {#each visible as l (l.k)}
      <div class="ln" class:err={l.s === 'stderr'}>
        <span class="ts">{time(l.t)}</span>{#if multi}<span class="ctr">{l.c}</span>{/if}<span class="msg">{#each parts(l.m) as p}{#if p.hit}<mark>{p.s}</mark>{:else}{p.s}{/if}{/each}</span>
      </div>
    {/each}
    {#if lines.length === 0}
      <p class="quiet">{ended || (connected ? 'Waiting for log lines…' : 'Connecting…')}</p>
    {:else if visible.length === 0}
      <p class="quiet">No line matches “{query}”.</p>
    {/if}
  </div>

  <div class="status">
    <span class="meta num">{matcher ? `${visible.length} of ${lines.length}` : lines.length} lines{lines.length >= MAX ? ` · showing the last ${MAX}` : ''}</span>
    {#if ended && lines.length}<span class="meta">{ended}</span>{/if}
    {#if !follow}
      <button class="btn jump" onclick={jump}><Icon name="arrowDown" /> Jump to latest</button>
    {/if}
  </div>
</div>

<style>
  .viewer {
    display: grid;
    gap: 10px;
  }
  .bar {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .sel {
    width: 200px;
    max-width: 100%;
  }
  .sel :global(.trigger) {
    height: 32px;
    font-size: 13px;
  }
  .find {
    flex: 1;
    min-width: 200px;
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
  .find:focus-within {
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
  .log {
    height: min(62vh, 640px);
    overflow: auto;
    padding: 10px 0;
    border-radius: var(--r-card);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    font-size: 12px;
    line-height: 1.7;
  }
  .ln {
    display: flex;
    gap: 12px;
    padding: 0 16px;
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
  }
  .ctr {
    color: var(--haze);
    flex: none;
    max-width: 160px;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .msg {
    color: var(--foam);
    min-width: 0;
  }
  .err .msg {
    color: var(--adrift-text);
  }
  mark {
    background: var(--buoy-wash);
    color: var(--buoy-text);
    border-radius: 3px;
    box-shadow: 0 0 0 1px var(--buoy-wash);
  }
  .quiet {
    padding: 8px 16px;
    color: var(--murk);
    font-family: var(--font);
  }
  .status {
    display: flex;
    align-items: center;
    gap: 12px;
    min-height: 32px;
  }
  .jump {
    margin-left: auto;
  }
</style>
