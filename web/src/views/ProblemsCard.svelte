<script lang="ts">
  import { api, type Problem, type ProblemFix, type StackProblems } from '../lib/api';
  import { go, isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';

  // The Problems card (M18F): what is wrong with the stack's containers, the evidence and
  // fixes that are existing actions with a preview. It adds no alerts.
  let {
    name,
    compose,
    env,
    editable,
    onTab,
    onSaved,
    onCount,
  }: {
    name: string;
    compose: string;
    env: string;
    editable: boolean;
    onTab: (tab: 'compose' | 'logs' | 'deps' | 'history') => void;
    onSaved: () => void;
    onCount?: (byContainer: Record<string, number>) => void;
  } = $props();

  let data = $state<StackProblems | null>(null);
  let error = $state('');
  let busy = $state(false);
  let showIgnored = $state(false);
  // a compose change waiting for confirmation
  let preview = $state<{ fix: ProblemFix; compose: string; env: string; before: string[]; after: string[] } | null>(null);

  export async function refresh() {
    try {
      data = await api.stackProblems(name);
      error = '';
      const by: Record<string, number> = {};
      for (const p of data.problems) if (p.rule !== 'starting') by[p.container] = (by[p.container] ?? 0) + 1;
      onCount?.(by);
    } catch (e) {
      error = (e as Error).message;
    }
  }
  $effect(() => {
    void name;
    refresh();
  });

  const lintKinds = new Set(['set-mem-limit', 'set-restart-no', 'use-free-port', 'add-env-var', 'add-healthcheck']);

  function diffLines(a: string, b: string) {
    const al = a.split('\n');
    const bl = b.split('\n');
    const before: string[] = [];
    const after: string[] = [];
    const max = Math.max(al.length, bl.length);
    let i = 0;
    while (i < max && al[i] === bl[i]) i++;
    let ea = al.length - 1;
    let eb = bl.length - 1;
    while (ea >= i && eb >= i && al[ea] === bl[eb]) {
      ea--;
      eb--;
    }
    for (let k = i; k <= ea; k++) before.push(al[k]);
    for (let k = i; k <= eb; k++) after.push(bl[k]);
    return { before, after };
  }

  async function runFix(p: Problem, f: ProblemFix) {
    error = '';
    if (lintKinds.has(f.kind)) {
      busy = true;
      try {
        const keys = f.kind === 'use-free-port' || f.kind === 'add-env-var' ? [f.value ?? ''] : undefined;
        const r = await api.lintFix(name, compose, env, { kind: f.kind, label: f.label, service: f.service ?? p.service, value: f.value, keys });
        if (!r.changed) {
          error = 'Nothing to change.';
          return;
        }
        const d = diffLines(compose, r.compose);
        const de = diffLines(env, r.env);
        preview = { fix: f, compose: r.compose, env: r.env, before: [...d.before, ...de.before], after: [...d.after, ...de.after] };
      } catch (e) {
        error = (e as Error).message;
      } finally {
        busy = false;
      }
      return;
    }
    switch (f.kind) {
      case 'restart':
        busy = true;
        try {
          await api.action(name, 'restart');
          onSaved();
        } catch (e) {
          error = (e as Error).message;
        } finally {
          busy = false;
        }
        return;
      case 'rollback':
        onTab('history');
        return;
      case 'open-logs':
      case 'open-terminal':
        onTab('logs');
        return;
      case 'open-compose':
      case 'open-images':
        onTab('compose');
        return;
      case 'open-deps':
        onTab('deps');
        return;
      case 'open-ports':
      case 'open-disk':
        go('settings/hosts');
        return;
      case 'open-registries':
        go('settings/general');
        return;
    }
  }

  async function apply(deploy: boolean) {
    if (!preview) return;
    busy = true;
    error = '';
    try {
      await api.save(name, preview.compose, preview.env);
      if (deploy) await api.action(name, 'up');
      preview = null;
      onSaved();
      setTimeout(refresh, 3000);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function ignore(p: Problem, ignored: boolean) {
    try {
      await api.ignoreProblem(name, p.service || p.container, p.rule, ignored);
      await refresh();
    } catch (e) {
      error = (e as Error).message;
    }
  }

  const fixAllowed = (f: ProblemFix) => editable || !(lintKinds.has(f.kind) || f.kind === 'restart');
</script>

{#if error}<p class="error-text" role="alert">{error}</p>{/if}
{#if data && (data.problems.length > 0 || data.ignored.length > 0)}
  <section class="card problems" aria-label="Problems">
    <header class="phead">
      <h3 class="section-title">Problems</h3>
      {#if data.basic}<span class="meta">Update the agent for a detailed diagnosis.</span>{/if}
      {#if data.ignored.length}
        <button type="button" class="btn btn-ghost btn-sm" onclick={() => (showIgnored = !showIgnored)}>
          {showIgnored ? 'Hide' : 'Show'} {data.ignored.length} ignored
        </button>
      {/if}
    </header>
    {#each data.problems as p (p.container + p.rule)}
      <article class="problem {p.severity}">
        <div class="ptitle">
          <span class="dot {p.severity === 'error' ? 'down' : p.severity === 'warning' ? 'degraded' : ''}"></span>
          <strong>{p.title}</strong>
          <span class="meta mono">{p.service || p.container}</span>
          {#if p.duringMaintenance}<span class="badge maintenance">during maintenance</span>{/if}
        </div>
        <p class="meta">{p.detail}</p>
        {#if p.evidence?.length}
          <ul class="evidence">
            {#each p.evidence as ev (ev)}<li class="mono">{ev}</li>{/each}
          </ul>
        {/if}
        {#if isAdmin() && (p.fixes?.length || p.rule !== 'starting')}
          <div class="pactions">
            {#each p.fixes ?? [] as f (f.kind + f.label)}
              <button
                type="button"
                class="btn btn-sm"
                onclick={() => runFix(p, f)}
                disabled={busy || !fixAllowed(f)}
                title={fixAllowed(f) ? '' : 'Read-only host: changes are not possible here'}>{f.label}</button>
            {/each}
            {#if p.rule !== 'starting'}
              <button type="button" class="btn btn-ghost btn-sm" onclick={() => ignore(p, true)}>Ignore</button>
            {/if}
          </div>
        {/if}
      </article>
    {/each}
    {#if showIgnored}
      {#each data.ignored as p (p.container + p.rule)}
        <article class="problem ignored">
          <div class="ptitle">
            <strong>{p.title}</strong> <span class="meta mono">{p.service || p.container}</span>
            {#if isAdmin()}<button type="button" class="btn btn-ghost btn-sm" onclick={() => ignore(p, false)}>Stop ignoring</button>{/if}
          </div>
        </article>
      {/each}
    {/if}

    {#if preview}
      <div class="preview" role="dialog" aria-label="Preview change">
        <p class="section-title">{preview.fix.label}</p>
        <div class="diff mono">
          {#each preview.before as l, i (i)}<div class="del">- {l}</div>{/each}
          {#each preview.after as l, i (i)}<div class="add">+ {l}</div>{/each}
        </div>
        <div class="pactions">
          <button type="button" class="btn btn-primary" onclick={() => apply(true)} disabled={busy}>Save &amp; redeploy</button>
          <button type="button" class="btn" onclick={() => apply(false)} disabled={busy}>Save only</button>
          <button type="button" class="btn btn-ghost" onclick={() => (preview = null)} disabled={busy}>Cancel</button>
        </div>
      </div>
    {/if}
  </section>
{/if}

<style>
  .problems {
    padding: 12px 14px;
    display: grid;
    gap: 10px;
    margin-bottom: 12px;
  }
  .phead {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .phead h3 {
    margin: 0;
  }
  .problem {
    display: grid;
    gap: 6px;
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .problem.ignored {
    opacity: 0.7;
  }
  .ptitle {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .problem p {
    margin: 0;
  }
  .evidence {
    margin: 0;
    padding-left: 18px;
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  .pactions {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .preview {
    display: grid;
    gap: 8px;
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--info-wash);
    box-shadow: inset 0 0 0 1px var(--info);
  }
  .diff {
    font-size: 12px;
    max-height: 240px;
    overflow: auto;
    white-space: pre;
  }
  .del {
    color: var(--down-text, var(--danger));
  }
  .add {
    color: var(--afloat-text);
  }
</style>
