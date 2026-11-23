<script lang="ts">
  import { hostPath, wsURL, type Op } from '../lib/api';
  import Icon from '../lib/Icon.svelte';

  let { op, onDone, onClose }: { op: Op; onDone: (ok: boolean) => void; onClose: () => void } = $props();

  let lines = $state<string[]>([]);
  let done = $state(false);
  let error = $state('');
  let box: HTMLDivElement;

  const titles: Record<string, string> = {
    up: 'Deploy', down: 'Down', stop: 'Stop', restart: 'Restart', pull: 'Pull images', update: 'Update images',
  };

  $effect(() => {
    const ws = new WebSocket(wsURL(hostPath(`/ws/ops/${encodeURIComponent(op.id)}`)));
    ws.onmessage = (ev) => {
      const m = JSON.parse(ev.data) as { lines: string[]; done: boolean; error?: string };
      if (m.lines?.length) lines = [...lines, ...m.lines].slice(-2000);
      if (m.done) {
        done = true;
        error = m.error ?? '';
        onDone(!m.error);
      }
    };
    ws.onerror = () => {
      if (!done) error = 'Lost the connection to the operation. It keeps running on the server.';
    };
    return () => ws.close();
  });

  $effect(() => {
    lines;
    queueMicrotask(() => box && (box.scrollTop = box.scrollHeight));
  });
</script>

<section class="card op" aria-live="polite">
  <header>
    <span class="state" class:ok={done && !error} class:fail={done && !!error}>
      {#if !done}<span class="spin"></span>{:else if error}<Icon name="x" />{:else}<Icon name="check" />{/if}
    </span>
    <span class="t">{titles[op.action] ?? op.action}</span>
    <span class="meta">{done ? (error ? 'Failed' : 'Finished') : 'Running…'}</span>
    <button class="btn btn-ghost btn-icon close" onclick={onClose} aria-label="Close output"><Icon name="x" /></button>
  </header>
  <div class="out mono" bind:this={box}>
    {#each lines as l, i (i)}<div>{l}</div>{/each}
    {#if !lines.length && !done}<div class="muted">Waiting for Docker…</div>{/if}
  </div>
</section>

<style>
  .op {
    overflow: hidden;
  }
  header {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 10px 12px 10px 18px;
  }
  .t {
    font-weight: 600;
    font-size: 13px;
  }
  .close {
    margin-left: auto;
  }
  .state {
    display: inline-flex;
    color: var(--haze);
  }
  .state.ok {
    color: var(--afloat);
  }
  .state.fail {
    color: var(--adrift);
  }
  .spin {
    width: 12px;
    height: 12px;
    border-radius: 50%;
    border: 2px solid var(--buoy-wash);
    border-top-color: var(--buoy);
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .out {
    max-height: 240px;
    overflow: auto;
    padding: 10px 18px 14px;
    background: var(--well);
    font-size: 12px;
    line-height: 1.7;
    color: var(--mist);
    white-space: pre-wrap;
    word-break: break-word;
  }
  .muted {
    color: var(--murk);
  }
</style>
