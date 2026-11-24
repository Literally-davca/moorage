<!--
  Soundings strip: the Moorage signature. One tick per check result (height =
  latency, colour = status) with deploy anchors between them, so "what changed
  before it broke" is visible at a glance.
-->
<script lang="ts">
  import type { Anchor, Tick } from './api';

  let {
    ticks,
    anchors = [],
    mini = false,
    legend = false,
  }: { ticks: Tick[]; anchors?: Anchor[]; mini?: boolean; legend?: boolean } = $props();

  const anchorLabel: Record<string, string> = {
    deploy: 'Deploy', update: 'Update images', restart: 'Restart', rollback: 'Roll back', stop: 'Stop', down: 'Down',
  };
  const statusLabel: Record<string, string> = {
    up: 'Up', down: 'Down', silenced: 'During deploy', paused: 'Stopped on purpose', maintenance: 'Failed during planned maintenance', unknown: 'No location could check', degraded: 'Partially down',
  };
  const timeFmt = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });

  type Item = { kind: 'tick'; t: Tick; h: number } | { kind: 'anchor'; a: Anchor };

  const items = $derived.by((): Item[] => {
    // Partially down ticks are drawn at full latency height like up ones (a deploy window
    // or maintenance gets the low height).
    const lat = ticks.filter((t) => t.status === 'up' || t.status === 'degraded').map((t) => t.latencyMs);
    const lo = Math.min(...lat);
    const hi = Math.max(...lat);
    const [minH, maxH] = mini ? [6, 12] : [14, 22];
    const height = (t: Tick) => {
      if (t.status === 'down') return maxH;
      if (t.status !== 'up' && t.status !== 'degraded') return mini ? 4 : 10;
      if (!isFinite(lo) || hi === lo) return Math.round((minH + maxH) / 2);
      return Math.round(minH + ((t.latencyMs - lo) / (hi - lo)) * (maxH - minH));
    };
    const out: Item[] = [];
    const first = ticks[0] ? Date.parse(ticks[0].at) : Infinity;
    const as = mini ? [] : anchors.filter((a) => Date.parse(a.at) >= first).sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
    let ai = 0;
    for (const t of ticks) {
      const at = Date.parse(t.at);
      while (ai < as.length && Date.parse(as[ai].at) <= at) out.push({ kind: 'anchor', a: as[ai++] });
      out.push({ kind: 'tick', t, h: height(t) });
    }
    while (ai < as.length) out.push({ kind: 'anchor', a: as[ai++] });
    return out;
  });

  const summary = $derived.by(() => {
    const down = ticks.filter((t) => t.status === 'down').length;
    const up = ticks.filter((t) => t.status === 'up').length;
    const part = ticks.filter((t) => t.status === 'degraded').length;
    return `Last ${ticks.length} checks: ${up} up, ${part ? `${part} partially down, ` : ''}${down} down${anchors.length ? `, ${anchors.length} deploys` : ''}`;
  });
  const hasQuiet = $derived(ticks.some((t) => t.status === 'silenced' || t.status === 'paused'));
  const hasMaint = $derived(ticks.some((t) => t.status === 'maintenance'));
  const hasDegraded = $derived(ticks.some((t) => t.status === 'degraded'));
  const hasUnknown = $derived(ticks.some((t) => t.status === 'unknown'));
</script>

<div class="strip-wrap">
  <div class="strip" class:mini role="img" aria-label={summary}>
    {#if ticks.length === 0}
      <span class="none">No results yet</span>
    {/if}
    {#each items as it, i (i)}
      {#if it.kind === 'tick'}
        <span class="tick {it.t.status}" style="height:{it.h}px"
          title={`${timeFmt.format(new Date(it.t.at))} · ${statusLabel[it.t.status] ?? it.t.status} · ${it.t.latencyMs} ms${it.t.msg ? ' · ' + it.t.msg : ''}`}></span>
      {:else}
        <span class="anchor"
          title={`${anchorLabel[it.a.kind] ?? it.a.kind} by ${it.a.actor || 'Moorage'} · ${timeFmt.format(new Date(it.a.at))}${it.a.detail ? ' · ' + it.a.detail : ''}`}></span>
      {/if}
    {/each}
  </div>
  {#if legend && !mini}
    <div class="legend">
      <span><i class="k up"></i>Up</span>
      {#if hasDegraded}<span><i class="k degraded"></i>Partially down</span>{/if}
      <span><i class="k down"></i>Down</span>
      {#if hasQuiet}<span><i class="k silenced"></i>Deploy window</span>{/if}
      {#if hasMaint}<span><i class="k maintenance"></i>Maintenance</span>{/if}
      {#if hasUnknown}<span><i class="k unknown"></i>No answer</span>{/if}
      <span><i class="k anchor-k"></i>Deploy</span>
    </div>
  {/if}
</div>

<style>
  .strip-wrap {
    min-width: 0;
  }
  .strip {
    display: flex;
    align-items: flex-end;
    justify-content: flex-end;
    gap: 3px;
    height: 26px;
    overflow: hidden;
  }
  .strip.mini {
    gap: 2px;
    height: 12px;
  }
  .tick {
    flex: none;
    width: 5px;
    border-radius: 3px;
    background: var(--afloat);
  }
  .mini .tick {
    width: 3px;
    border-radius: 2px;
  }
  .tick.down {
    background: var(--adrift);
  }
  .tick.silenced {
    background: var(--signal);
  }
  .tick.degraded {
    background: var(--signal);
  }
  .tick.paused {
    background: var(--becalmed);
  }
  /* planned maintenance: the Maintenance colour, hollow so it reads apart from a deploy window */
  .tick.maintenance {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px var(--signal);
  }
  /* no location could run the check (or this location didn't answer): hollow and grey */
  .tick.unknown {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px var(--becalmed);
  }
  .anchor {
    flex: none;
    width: 3px;
    height: 26px;
    border-radius: 2px;
    background: var(--buoy);
  }
  .none {
    font-size: 12px;
    color: var(--haze);
    align-self: center;
    margin-right: auto;
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 14px;
    justify-content: flex-end;
    margin-top: 6px;
    font-size: 11px;
    color: var(--haze);
  }
  .legend span {
    display: inline-flex;
    align-items: center;
    gap: 5px;
  }
  .k {
    display: inline-block;
    width: 5px;
    height: 10px;
    border-radius: 2px;
    background: var(--afloat);
  }
  .k.down {
    background: var(--adrift);
  }
  .k.silenced,
  .k.degraded {
    background: var(--signal);
  }
  .k.maintenance {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px var(--signal);
  }
  .k.unknown {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px var(--becalmed);
  }
  .k.anchor-k {
    width: 3px;
    height: 12px;
    background: var(--buoy);
  }
</style>
