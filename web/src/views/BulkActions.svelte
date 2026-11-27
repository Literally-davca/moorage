<script lang="ts">
  import { api, type ImagePolicy, type StackSummary } from '../lib/api';
  import { isAdmin, canEdit, stackLabel, app } from '../lib/state.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';
  import Icon from '../lib/Icon.svelte';

  // Bulk actions (M19M): pick several stacks of this host and run one action on them, one
  // after another. Moorage's own stack is skipped (it updates through Moorage updates and
  // restarts only through Apply & restart); a stack whose action fails is listed with the
  // reason and the rest go on.
  let { stacks, onChanged }: { stacks: StackSummary[]; onChanged?: () => void } = $props();

  let selected = $state<string[]>([]);
  let running = $state(false);
  let current = $state('');
  let results = $state<{ stack: string; ok: boolean; msg: string }[]>([]);
  let open = $state(false);

  const items = $derived(
    stacks
      .filter((s) => s.managed || s.self)
      .map((s) => ({ value: s.name, label: stackLabel(app.host, s.name, s.displayName), meta: s.self ? 'Moorage itself – skipped' : s.status })),
  );

  async function waitIdle(name: string) {
    const until = Date.now() + 10 * 60 * 1000;
    while (Date.now() < until) {
      await new Promise((r) => setTimeout(r, 1500));
      try {
        const st = await api.stack(name);
        if (!st.busy) return st;
      } catch {
        /* keep waiting */
      }
    }
    return null;
  }

  async function run(kind: 'up' | 'restart' | 'stop' | 'update' | ImagePolicy) {
    if (!selected.length || running) return;
    const verb = { up: 'Start', restart: 'Restart', stop: 'Stop', update: 'Update images', off: 'Updates: off', notify: 'Updates: notify', auto: 'Updates: auto' }[kind];
    if ((kind === 'stop' || kind === 'restart') && !confirm(`${verb} ${selected.length} stacks?`)) return;
    running = true;
    results = [];
    for (const name of selected) {
      current = name;
      const s = stacks.find((x) => x.name === name);
      if (s?.self) {
        results.push({ stack: name, ok: false, msg: 'skipped: Moorage itself (use Apply & restart or Moorage updates)' });
        continue;
      }
      try {
        if (kind === 'off' || kind === 'notify' || kind === 'auto') {
          await api.saveStackImagePolicy(name, kind);
          results.push({ stack: name, ok: true, msg: verb });
          continue;
        }
        if (kind === 'update') await api.updateStackImages(name);
        else await api.action(name, kind);
        const st = await waitIdle(name);
        if (!st) results.push({ stack: name, ok: false, msg: 'still running after 10 minutes' });
        else if (kind !== 'stop' && (st.status === 'down' || st.status === 'degraded')) results.push({ stack: name, ok: false, msg: `${verb} finished, but the stack is ${st.status}` });
        else results.push({ stack: name, ok: true, msg: `${verb} done` });
      } catch (e) {
        results.push({ stack: name, ok: false, msg: (e as Error).message });
      }
    }
    current = '';
    running = false;
    onChanged?.();
  }
</script>

{#if isAdmin() && canEdit() && items.length > 1}
  <section class="card card-pad" aria-label="Bulk actions">
    <button type="button" class="head" onclick={() => (open = !open)} aria-expanded={open}>
      <h2 class="section-title">Bulk actions</h2>
      <span class="meta">Run one action on several stacks</span>
    </button>
    {#if open}
      <div class="body">
        <MultiSelect label="Stacks" placeholder="Pick stacks" {items} bind:selected />
        <div class="acts">
          <button type="button" class="btn btn-sm" disabled={running || !selected.length} onclick={() => run('up')}><Icon name="play" /> Start</button>
          <button type="button" class="btn btn-sm" disabled={running || !selected.length} onclick={() => run('restart')}><Icon name="refresh" /> Restart</button>
          <button type="button" class="btn btn-sm" disabled={running || !selected.length} onclick={() => run('stop')}><Icon name="stop" /> Stop</button>
          <button type="button" class="btn btn-sm" disabled={running || !selected.length} onclick={() => run('update')}><Icon name="cloudDown" /> Update images</button>
          <span class="meta">Image updates:</span>
          <button type="button" class="btn btn-sm btn-ghost" disabled={running || !selected.length} onclick={() => run('off')}>Off</button>
          <button type="button" class="btn btn-sm btn-ghost" disabled={running || !selected.length} onclick={() => run('notify')}>Notify</button>
          <button type="button" class="btn btn-sm btn-ghost" disabled={running || !selected.length} onclick={() => run('auto')}>Auto</button>
        </div>
        {#if running}<p class="meta" role="status">Working on {current}… ({results.length + 1} of {selected.length})</p>{/if}
        {#if results.length}
          <ul class="res">
            {#each results as r (r.stack)}
              <li class:bad={!r.ok}><Icon name={r.ok ? 'check' : 'alert'} /> <strong>{r.stack}</strong> – {r.msg}</li>
            {/each}
          </ul>
          {#if !running}<p class="meta">{results.filter((r) => r.ok).length} done, {results.filter((r) => !r.ok).length} skipped or failed.</p>{/if}
        {/if}
      </div>
    {/if}
  </section>
{/if}

<style>
  .head {
    all: unset;
    cursor: pointer;
    display: flex;
    gap: 12px;
    align-items: baseline;
    justify-content: space-between;
    width: 100%;
  }
  .body {
    display: grid;
    gap: 8px;
    margin-top: 8px;
  }
  .acts {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .res {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .res li {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .res li.bad {
    color: var(--adrift-text);
  }
</style>
