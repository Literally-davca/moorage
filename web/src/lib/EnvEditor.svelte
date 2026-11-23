<script lang="ts">
  import CodeEditor from './CodeEditor.svelte';
  import Icon from './Icon.svelte';
  import { composeVars, looksSecret, parseEnv, serializeEnv, validKey, type EnvLine, type EnvVar } from './dotenv';

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
  function add(key = '', val = '') {
    write([...lines, { kind: 'var', key, value: val }]);
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

  {#if missing.length && !readonly}
    <div class="missing" role="status">
      <Icon name="alert" />
      <span>The compose file needs {missing.length === 1 ? 'a variable' : 'variables'} without a default:</span>
      {#each missing as k (k)}
        <button type="button" class="chip mono" onclick={() => add(k)}><Icon name="plus" /> {k}</button>
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
              <input class="field mono" type={masked ? 'password' : 'text'} value={l.value} placeholder="value" {readonly}
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
          {#if (problem && l.key) || unused(l.key)}
            <p class="note" class:warnc={!!problem}>{problem || 'Not used in the compose file'}</p>
          {/if}
        </div>
      {:else}
        <p class="meta empty">No variables.{readonly ? '' : ' Add one, or paste KEY=value lines into a name field.'}</p>
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
  .note {
    grid-column: 1 / -1;
    margin: -2px 0 2px;
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
