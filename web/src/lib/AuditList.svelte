<script lang="ts">
  import type { AuditEntry } from './api';
  import { stackLabel } from './state.svelte';
  import { auditLabel, auditTime } from './audit';

  // where=false hides host / stack (the stack's own Logbook already says which stack).
  let { entries, where = true }: { entries: AuditEntry[]; where?: boolean } = $props();

  function place(e: AuditEntry): string {
    const h = e.hostId ? e.hostName || e.hostId : '';
    const st = e.stack ? stackLabel(e.hostId, e.stack, e.stackDisplayName) : '';
    return [h, st].filter(Boolean).join(' / ');
  }
  const stackHref = (e: AuditEntry) => `#/h/${encodeURIComponent(e.hostId)}/stacks/${encodeURIComponent(e.stack)}`;
</script>

<ul class="audit" class:nowhere={!where}>
  <li class="hrow meta" aria-hidden="true">
    <span>Time</span><span>Who</span><span>Action</span>{#if where}<span>Host / stack</span>{/if}<span>Target</span><span>Result</span>
  </li>
  {#each entries as e (e.id)}
    <li class="arow">
      <time class="time mono" datetime={e.at} title={new Date(e.at).toLocaleString()}>{auditTime(e.at)}</time>
      <span class="who">
        <span class="actor" class:sys={e.actor === 'system'}>{e.actor}</span>
        {#if e.ip}<span class="meta mono ip">{e.ip}</span>{/if}
      </span>
      <span class="act" title={e.action}>{auditLabel(e.action)}</span>
      {#if where}
        <span class="where">
          {#if e.stack && e.hostId}<a href={stackHref(e)}>{place(e)}</a>{:else}{place(e) || '–'}{/if}
        </span>
      {/if}
      <span class="target mono">{e.target || ''}</span>
      <span class="res"><span class="tag {e.outcome}">{e.outcome === 'ok' ? 'OK' : 'Failed'}</span></span>
    </li>
  {/each}
</ul>

<style>
  .audit {
    list-style: none;
    margin: 0;
    padding: 0;
    font-size: 13px;
  }
  .hrow,
  .arow {
    display: grid;
    grid-template-columns: 112px minmax(110px, 1fr) minmax(140px, 1.3fr) minmax(120px, 1.2fr) minmax(80px, 1fr) 64px;
    gap: 12px;
    align-items: center;
    padding: 8px 16px;
  }
  .nowhere .hrow,
  .nowhere .arow {
    grid-template-columns: 112px minmax(110px, 1fr) minmax(140px, 1.3fr) minmax(80px, 1fr) 64px;
  }
  .hrow {
    font-size: 11px;
    font-weight: 500;
    letter-spacing: 0.04em;
  }
  .arow {
    border-top: 1px solid var(--rope);
  }
  .time {
    font-size: 12px;
    color: var(--murk);
  }
  .who {
    display: grid;
    min-width: 0;
  }
  .actor {
    color: var(--foam);
    font-weight: 500;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .actor.sys {
    color: var(--mist);
    font-style: italic;
  }
  .ip {
    font-size: 11px;
  }
  .act {
    color: var(--foam);
  }
  .where,
  .target {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--mist);
  }
  .target {
    font-size: 12px;
  }
  .tag {
    display: inline-flex;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .tag.failed {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }

  /* Phones and narrow panes: each entry becomes a small card of two or three lines. */
  @media (max-width: 899px) {
    .hrow {
      display: none;
    }
    .arow,
    .nowhere .arow {
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 2px 12px;
      padding: 10px 14px;
    }
    .act {
      grid-column: 1;
      grid-row: 1;
      font-weight: 500;
    }
    .res {
      grid-column: 2;
      grid-row: 1;
    }
    .who {
      grid-column: 1 / 3;
      display: flex;
      gap: 8px;
    }
    .time {
      grid-column: 1 / 3;
    }
    .where,
    .target {
      grid-column: 1 / 3;
    }
    .target:empty {
      display: none;
    }
  }
</style>
