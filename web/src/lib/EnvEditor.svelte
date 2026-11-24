<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type SharedVar } from './api';
  import { app } from './state.svelte';
  import CodeEditor from './CodeEditor.svelte';
  import Icon from './Icon.svelte';
  import { composeVars, looksSecret, parseEnv, removeVars, serializeEnv, unusedVars, validKey, type EnvLine, type EnvVar } from './dotenv';

  // .env editor with two views: Variables (a key/value table, like Coolify) and Raw
  // (the file as text). The text is the single source of truth; the table edits it
  // line by line, so comments and formatting of untouched lines survive.
  let {
    value = $bindable(''),
    compose = '',
    readonly = false,
    label = '.env file',
  }: { value?: string; compose?: string; readonly?: boolean; label?: string } = $props();

  const MODE_KEY = 'moorage-env-mode';
  let mode = $state<'vars' | 'raw'>('vars');
  try {
    if (localStorage.getItem(MODE_KEY) === 'raw') mode = 'raw';
  } catch {
    /* private mode */
  }
  function setMode(m: 'vars' | 'raw') {
    mode = m;
    try {
      localStorage.setItem(MODE_KEY, m);
    } catch {
      /* ignore */
    }
  }

  const lines = $derived(parseEnv(value));
  const vars = $derived(
    lines.map((l, i) => ({ l, i })).filter((x): x is { l: EnvVar; i: number } => x.l.kind === 'var'),
  );
  const keyCount = $derived.by(() => {
    const m = new Map<string, number>();
    for (const { l } of vars) m.set(l.key, (m.get(l.key) ?? 0) + 1);
    return m;
  });
  const used = $derived(composeVars(compose));
  const missing = $derived([...used.required].filter((k) => !keyCount.has(k)).sort());
  const comments = $derived(lines.length - vars.length);

  // Revealed secret rows, by key, for this view only.
  let revealed = $state<Record<string, boolean>>({});
  // Variables compose.yaml reads that .env doesn't define yet. They are offered, not
  // written on their own: opening a stack must not leave unsaved changes behind.
  const notInEnv = $derived([...used.all].filter((k) => !keyCount.has(k)).sort());
  function addAll() {
    // Empty values: `${VAR:-default}` still falls back to its default (shown as placeholder).
    write([...lines, ...notInEnv.map((k): EnvLine => ({ kind: 'var', key: k, value: '' }))]);
  }

  function write(next: EnvLine[]) {
    value = serializeEnv(next);
  }
  function update(i: number, patch: Partial<EnvVar>) {
    const next = [...lines];
    const cur = next[i] as EnvVar;
    next[i] = { ...cur, ...patch, raw: undefined };
    write(next);
  }
  function remove(i: number) {
    write(lines.filter((_, j) => j !== i));
  }
  // Rows added with "Add variable" live here until they get a name: an empty assignment
  // ("=") in the file would be invalid and invisible.
  let drafts = $state<{ id: number; value: string }[]>([]);
  let draftSeq = 0;
  function add(key = '', val = '') {
    if (!key) {
      drafts = [...drafts, { id: ++draftSeq, value: val }];
      return;
    }
    write([...lines, { kind: 'var', key, value: val }]);
  }
  function commitDraft(id: number, key: string) {
    const d = drafts.find((x) => x.id === id);
    if (!d || !key) return;
    drafts = drafts.filter((x) => x.id !== id);
    write([...lines, { kind: 'var', key, value: d.value }]);
  }
  function focusMe(el: HTMLInputElement) {
    queueMicrotask(() => el.focus());
  }

  // Keys of .env compose.yaml never references (none when it loads env_file: .env).
  const unusedKeys = $derived(unusedVars(value, compose));
  function dropUnused(keys: string[]) {
    value = removeVars(value, keys);
  }

  let sharedList = $state<SharedVar[]>([]);
  $effect(() => {
    if (readonly) return;
    untrack(() => {
      api.sharedVars()
        .then((r) => (sharedList = r.vars ?? []))
        .catch(() => (sharedList = []));
    });
  });

  const applicableShared = $derived.by(() => {
    const byKey = new Map<string, SharedVar>();
    for (const v of sharedList) {
      if (!v.isSet && !v.value) continue;
      if (v.hostId === '') byKey.set(v.key, v);
    }
    for (const v of sharedList) {
      if (!v.isSet && !v.value) continue;
      if (v.hostId === app.host) byKey.set(v.key, v);
    }
    return [...byKey.values()];
  });

  function insertShared(sv: SharedVar) {
    const insertedVal = sv.secret ? `__MOORAGE_SHARED_SECRET_${sv.key}__` : (sv.value ?? '');
    const idx = lines.findIndex((l) => l.kind === 'var' && l.key === sv.key);
    if (idx >= 0) {
      update(idx, { value: insertedVal });
    } else {
      add(sv.key, insertedVal);
    }
  }

  // Pasting several KEY=VALUE lines into a key field adds them as rows (Coolify does this).
  function onKeyPaste(i: number, e: ClipboardEvent) {
    const text = e.clipboardData?.getData('text') ?? '';
    if (!text.includes('=')) return;
    const pasted = parseEnv(text).filter((l): l is EnvVar => l.kind === 'var');
    if (!pasted.length) return;
    e.preventDefault();
    const next = [...lines];
    const cur = next[i] as EnvVar;
    const rest = pasted.map((p) => ({ kind: 'var' as const, key: p.key, value: p.value }));
    // An empty row is replaced by the first pasted pair.
    if (!cur.key && !cur.value) next.splice(i, 1, ...rest);
    else next.splice(i + 1, 0, ...rest);
    write(next);
  }

  function keyProblem(k: string): string {
    if (!k) return 'Name is empty';
    if (!validKey(k)) return 'Letters, digits, _ . - only, not starting with a digit';
    if ((keyCount.get(k) ?? 0) > 1) return 'Defined more than once: the last one wins';
    return '';
  }
  const unused = (k: string) => !!compose && !used.envFile && !used.all.has(k);
</script>

<div class="env">
  <div class="head">
    <div class="seg" role="tablist" aria-label="{label} view">
      <button type="button" role="tab" aria-selected={mode === 'vars'} class:on={mode === 'vars'} onclick={() => setMode('vars')}>Variables</button>
      <button type="button" role="tab" aria-selected={mode === 'raw'} class:on={mode === 'raw'} onclick={() => setMode('raw')}>Raw</button>
    </div>
    {#if mode === 'vars' && comments > 0}
      <span class="meta">{comments} comment {comments === 1 ? 'line' : 'lines'} kept, see Raw</span>
    {/if}
  </div>

  {#if notInEnv.length && !readonly}
    <div class="missing" class:soft={!missing.length} role="status">
      <Icon name={missing.length ? 'alert' : 'plus'} />
      <span>
        {notInEnv.length === 1 ? 'A variable' : `${notInEnv.length} variables`} from the compose file {notInEnv.length === 1 ? "isn't" : "aren't"} in .env{missing.length ? `, ${missing.length} without a default` : ''}:
      </span>
      {#each notInEnv as k (k)}
        <button type="button" class="chip mono" class:req={missing.includes(k)} onclick={() => add(k)}
          title={missing.includes(k) ? 'Required: no default in compose.yaml' : 'Has a default in compose.yaml'}><Icon name="plus" /> {k}</button>
      {/each}
      {#if notInEnv.length > 1}
        <button type="button" class="chip all" onclick={addAll}>Add all</button>
      {/if}
    </div>
  {/if}

  {#if unusedKeys.length && !readonly}
    <div class="missing soft" role="status">
      <Icon name="info" />
      <span>{unusedKeys.length} unused {unusedKeys.length === 1 ? 'variable' : 'variables'} in .env:</span>
      {#each unusedKeys as k (k)}
        <button type="button" class="chip mono" onclick={() => dropUnused([k])} title="Remove {k} from .env"><Icon name="x" /> {k}</button>
      {/each}
      {#if unusedKeys.length > 1}
        <button type="button" class="chip all" onclick={() => dropUnused(unusedKeys)}>Remove all unused</button>
      {/if}
    </div>
  {/if}

  {#if applicableShared.length > 0 && !readonly}
    <div class="missing soft" role="region" aria-label="Shared variables">
      <Icon name="key" />
      <span>Insert shared variable:</span>
      {#each applicableShared as sv (sv.key)}
        <button
          type="button"
          class="chip mono"
          onclick={() => insertShared(sv)}
          title={sv.secret ? `Insert shared secret ${sv.key}` : `${sv.key}=${sv.value ?? ''}`}
        >
          <Icon name="plus" /> {sv.key}{#if sv.secret} · secret{/if}
        </button>
      {/each}
    </div>
  {/if}

  {#if mode === 'raw'}
    <CodeEditor bind:value language="plain" {label} {readonly} minHeight={96} />
  {:else}
    <div class="rows" role="table" aria-label={label}>
      {#each vars as { l, i } (i)}
        {@const problem = keyProblem(l.key)}
        {@const secret = looksSecret(l.key)}
        {@const masked = secret && !revealed[l.key]}
        {@const svcs = used.servicesByVar.get(l.key) ?? []}
        {@const defVal = used.defaults.get(l.key)}
        <div class="row" role="row">
          <input class="field mono key" class:bad={!!problem && !!l.key} value={l.key} placeholder="NAME" {readonly}
            aria-label="Variable name" spellcheck="false" autocomplete="off" aria-invalid={!!problem}
            onchange={(e) => update(i, { key: e.currentTarget.value.trim() })} onpaste={(e) => onKeyPaste(i, e)} />
          <span class="eq" aria-hidden="true">=</span>
          <div class="val">
            {#if l.value.includes('\n')}
              <textarea class="field mono" rows={Math.min(6, l.value.split('\n').length)} value={l.value} {readonly}
                aria-label="Value of {l.key}" spellcheck="false" class:masked
                onchange={(e) => update(i, { value: e.currentTarget.value })}></textarea>
            {:else}
              <input class="field mono" type={masked ? 'password' : 'text'} value={l.value} placeholder={defVal ? `default: ${defVal}` : 'value'} {readonly}
                aria-label="Value of {l.key || 'variable'}" spellcheck="false" autocomplete="off"
                onchange={(e) => update(i, { value: e.currentTarget.value })} />
            {/if}
            {#if secret}
              <button type="button" class="btn btn-ghost btn-icon" aria-label={masked ? 'Show value' : 'Hide value'} title={masked ? 'Show value' : 'Hide value'}
                onclick={() => (revealed = { ...revealed, [l.key]: !revealed[l.key] })}><Icon name={masked ? 'eye' : 'eyeOff'} /></button>
            {/if}
          </div>
          {#if !readonly}
            <button type="button" class="btn btn-ghost btn-icon" aria-label="Remove {l.key || 'variable'}" title="Remove" onclick={() => remove(i)}><Icon name="trash" /></button>
          {/if}
          {#if svcs.length > 0 || (problem && l.key) || unused(l.key)}
            <div class="note-row">
              {#if svcs.length > 0}
                <span class="svc-chips">
                  {#each svcs as s (s)}<span class="svc-chip mono">{s}</span>{/each}
                </span>
              {/if}
              {#if (problem && l.key) || unused(l.key)}
                <span class="note" class:warnc={!!problem}>{problem || 'Not used in compose'}</span>
              {/if}
            </div>
          {/if}
        </div>
      {:else}
        {#if !drafts.length}<p class="meta empty">No variables.{readonly ? '' : ' Add one, or paste KEY=value lines into a name field.'}</p>{/if}
      {/each}
      {#each drafts as d (d.id)}
        <div class="row" role="row">
          <input class="field mono key" placeholder="NAME" aria-label="Variable name" spellcheck="false" autocomplete="off" use:focusMe
            onchange={(e) => commitDraft(d.id, e.currentTarget.value.trim())}
            onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), commitDraft(d.id, e.currentTarget.value.trim()))} />
          <span class="eq" aria-hidden="true">=</span>
          <div class="val">
            <input class="field mono" value={d.value} placeholder="value" aria-label="Value of the new variable" spellcheck="false" autocomplete="off"
              oninput={(e) => (d.value = e.currentTarget.value)} />
          </div>
          <button type="button" class="btn btn-ghost btn-icon" aria-label="Remove new variable" title="Remove"
            onclick={() => (drafts = drafts.filter((x) => x.id !== d.id))}><Icon name="trash" /></button>
          <div class="note-row"><span class="note">Not saved until it has a name</span></div>
        </div>
      {/each}
    </div>
    {#if !readonly}
      <div><button type="button" class="btn" onclick={() => add()}><Icon name="plus" /> Add variable</button></div>
    {/if}
  {/if}
</div>

<style>
  .env {
    display: grid;
    gap: 8px;
    min-width: 0;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 12px;
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
    height: 28px;
    padding: 0 12px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--mist);
    font: 500 12px var(--font);
    cursor: pointer;
  }
  .seg button.on {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .missing {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px 8px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    background: var(--signal-wash);
    color: var(--signal-text);
    font-size: 13px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    height: 24px;
    padding: 0 8px;
    border: 0;
    border-radius: 999px;
    background: var(--rope-strong);
    color: var(--foam);
    font-size: 12px;
    font-weight: 500;
    cursor: pointer;
  }
  .chip.req {
    background: var(--signal);
    color: var(--deep-water);
  }
  .chip.all {
    background: transparent;
    color: inherit;
    text-decoration: underline;
  }
  .missing.soft {
    background: var(--well);
    color: var(--haze);
  }
  .rows {
    display: grid;
    gap: 6px;
  }
  .row {
    display: grid;
    grid-template-columns: minmax(120px, 2fr) auto minmax(0, 3fr) auto;
    align-items: start;
    gap: 6px;
  }
  .eq {
    color: var(--haze);
    line-height: 34px;
  }
  .val {
    display: flex;
    gap: 4px;
    min-width: 0;
  }
  .val > :first-child {
    flex: 1;
    min-width: 0;
  }
  textarea.masked {
    -webkit-text-security: disc;
  }
  .key.bad {
    box-shadow: inset 0 0 0 1px var(--adrift);
  }
  .note-row {
    grid-column: 1 / -1;
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    margin: -2px 0 2px;
  }
  .svc-chips {
    display: inline-flex;
    gap: 4px;
    flex-wrap: wrap;
  }
  .svc-chip {
    font-size: 10px;
    padding: 1px 6px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .note {
    font-size: 12px;
    color: var(--haze);
  }
  .note.warnc {
    color: var(--adrift-text);
  }
  .empty {
    padding: 4px 0;
  }
  @media (max-width: 560px) {
    .row {
      grid-template-columns: minmax(0, 1fr) auto;
    }
    .eq {
      display: none;
    }
    .val {
      grid-column: 1 / 2;
    }
  }
</style>
