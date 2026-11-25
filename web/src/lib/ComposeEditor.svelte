<!--
  The compose editor of a stack: the code editor with a lossless "Comments / Clean" view
  switch (remembered), the linter's findings with one-click quick fixes, and a slot for the
  Save buttons next to the switch.
-->
<script lang="ts">
  import type { Snippet } from 'svelte';
  import { api, type LintIssue } from './api';
  import { userPrefs } from './state.svelte';
  import CodeEditor from './CodeEditor.svelte';
  import ComposeForm from './ComposeForm.svelte';
  import { autoIndentCompose } from './composeIndent';
  import Icon from './Icon.svelte';
  import { hasComments, mergeClean, toClean } from './yamlcomments';

  let {
    value = $bindable(''),
    env = $bindable(''),
    name = '',
    issues = [],
    readonly = false,
    title = 'compose.yaml',
    actions,
  }: {
    value?: string;
    env?: string;
    name?: string;
    issues?: LintIssue[];
    readonly?: boolean;
    title?: string;
    actions?: Snippet;
  } = $props();

  const VIEW_KEY = 'moorage-compose-view';
  type ViewTab = 'form' | 'comments' | 'clean';
  function prefToTab(): ViewTab {
    const p = userPrefs().composeView;
    if (p === 'form') return 'form';
    if (p === 'yaml-clean') return 'clean';
    return 'comments';
  }
  let view = $state<ViewTab>(prefToTab());
  let userPickedView = false;
  try {
    const saved = localStorage.getItem(VIEW_KEY);
    if (saved === 'form' || saved === 'comments' || saved === 'clean') {
      // Only override if no explicit server preference differs from default
      if (userPrefs().mode === 'custom' || !userPrefs().onboardingDone) {
        view = saved;
      }
    }
  } catch {
    /* private mode */
  }
  $effect(() => {
    const preferred = prefToTab();
    if (!userPickedView) {
      view = preferred;
      if (preferred === 'clean') openClean(value);
    }
  });

  // In the clean view the editor shows cleanText; each edit is merged into base (the file
  // as it was when the view opened), so untouched lines keep their comments.
  let base = $state('');
  let cleanText = $state('');
  let lastMerged = $state<string | null>(null);
  let editorRef = $state<CodeEditor | null>(null);
  let autoIndented = $state(false);

  function openClean(v: string) {
    base = v;
    cleanText = toClean(v);
    lastMerged = v;
  }
  function setView(v: ViewTab) {
    userPickedView = true;
    view = v;
    autoIndented = false;
    if (v === 'clean') openClean(value);
    try {
      localStorage.setItem(VIEW_KEY, v);
    } catch {
      /* ignore */
    }
  }
  // the file changed from outside (load, discard, quick fix): reopen the clean view on it
  $effect(() => {
    const v = value;
    if (view === 'clean' && v !== lastMerged) openClean(v);
  });
  function onCleanInput(next: string) {
    cleanText = next;
    const merged = mergeClean(base, next);
    lastMerged = merged;
    value = merged;
  }
  const commented = $derived(hasComments(value));
  function stripPermanently() {
    value = toClean(value);
    openClean(value);
  }

  function fixIndentNow() {
    if (view === 'clean') {
      const res = autoIndentCompose(cleanText);
      if (res.changed) onCleanInput(res.text);
      return;
    }
    const res = autoIndentCompose(value);
    if (res.changed) {
      value = res.text;
      autoIndented = true;
    }
  }

  // ---- linter ----
  const order = { error: 0, deploy_error: 1, warning: 2, info: 3 } as const;
  const sorted = $derived([...issues].sort((a, b) => order[a.severity] - order[b.severity] || (a.line ?? 0) - (b.line ?? 0)));
  const counts = $derived({
    error: issues.filter((i) => i.severity === 'error').length,
    deploy: issues.filter((i) => i.severity === 'deploy_error').length,
    warning: issues.filter((i) => i.severity === 'warning').length,
    info: issues.filter((i) => i.severity === 'info').length,
  });
  let showInfo = $state(userPrefs().linterShowTips);
  let infoToggled = false;
  $effect(() => {
    const def = userPrefs().linterShowTips;
    if (!infoToggled) showInfo = def;
  });
  const visible = $derived(sorted.filter((i) => showInfo || i.severity !== 'info'));
  let fixing = $state('');
  let fixError = $state('');
  async function applyFix(i: LintIssue) {
    if (!i.fix) return;
    fixing = i.code + (i.service ?? '') + (i.line ?? '');
    fixError = '';
    try {
      const r = await api.lintFix(name, value, env, i.fix);
      if (r.changed) {
        value = r.compose;
        env = r.env;
      }
    } catch (e) {
      fixError = (e as Error).message;
    } finally {
      fixing = '';
    }
  }
  const sevLabel: Record<string, string> = { error: 'Error', deploy_error: 'Blocks deploy', warning: 'Warning', info: 'Tip' };
</script>

<div class="ce">
  <div class="ed-head">
    <span class="section-title">{title}</span>
    <div class="right">
      <div class="seg" role="tablist" aria-label="Compose view">
        <button type="button" role="tab" aria-selected={view === 'form'} class:on={view === 'form'} onclick={() => setView('form')}>Form</button>
        <button type="button" role="tab" aria-selected={view === 'comments'} class:on={view === 'comments'} onclick={() => setView('comments')}>Comments</button>
        <button
          type="button"
          role="tab"
          aria-selected={view === 'clean'}
          class:on={view === 'clean'}
          onclick={() => setView('clean')}
          title="Hide comments and blank lines while editing; untouched lines keep their comments when you save"
        >Clean</button>
      </div>
      {#if !readonly && view !== 'form'}
        <button type="button" class="btn btn-ghost" onclick={fixIndentNow} title="Reconstruct 2-space Compose indentation">
          <Icon name="wand" /> Fix indent
        </button>
      {/if}
      {@render actions?.()}
    </div>
  </div>
  {#if autoIndented && view !== 'form'}
    <div class="indent-bar" role="status">
      <span>Auto-indented pasted YAML</span>
      <div class="indent-acts">
        <button type="button" class="link-btn" onclick={() => { editorRef?.undo(); autoIndented = false; }}>Undo</button>
        <button type="button" class="link-btn" onclick={() => (autoIndented = false)}>Dismiss</button>
      </div>
    </div>
  {/if}
  {#if view === 'form'}
    <ComposeForm bind:value {readonly} onSwitchYaml={() => setView('comments')} />
  {:else if view === 'clean'}
    <CodeEditor
      bind:this={editorRef}
      bind:value={() => cleanText, onCleanInput}
      label="Compose file without comments or blank lines"
      {readonly}
      minHeight={360}
      autoIndentCompose={true}
      onAutoIndent={() => (autoIndented = true)}
    />
    <p class="meta clean-note">
      Comments and original blank lines are removed here to save space; untouched lines keep their comments when you save. Any blank lines you type here are kept.
      {#if !readonly && commented}<button type="button" class="link-btn" onclick={stripPermanently}>Strip comments permanently</button>{/if}
    </p>
  {:else}
    <CodeEditor
      bind:this={editorRef}
      bind:value
      label="Compose file"
      {readonly}
      minHeight={360}
      autoIndentCompose={true}
      onAutoIndent={() => (autoIndented = true)}
    />
  {/if}

  {#if issues.length}
    <div class="lint" aria-label="Linter">
      <div class="lint-head">
        <span class="label">Linter</span>
        {#if counts.error}<span class="pill bad">{counts.error} {counts.error === 1 ? 'error' : 'errors'}</span>{/if}
        {#if counts.deploy}<span class="pill bad soft">{counts.deploy} blocking deploy</span>{/if}
        {#if counts.warning}<span class="pill warn">{counts.warning} {counts.warning === 1 ? 'warning' : 'warnings'}</span>{/if}
        {#if counts.info}
          <button
            type="button"
            class="pill info"
            aria-pressed={showInfo}
            onclick={() => {
              infoToggled = true;
              showInfo = !showInfo;
            }}
          >{counts.info} {counts.info === 1 ? 'tip' : 'tips'} {showInfo ? '▴' : '▾'}</button>
        {/if}
      </div>
      {#each visible as i, k (k)}
        <div class="issue {i.severity}">
          <span class="sev">{sevLabel[i.severity]}</span>
          <span class="msg">{i.message}{#if i.line}<span class="meta"> · line {i.line}</span>{/if}</span>
          {#if i.fix && !readonly}
            <button type="button" class="btn btn-ghost fix" onclick={() => applyFix(i)} disabled={!!fixing}>
              <Icon name="wand" /> {i.fix.label}
            </button>
          {/if}
        </div>
      {/each}
      {#if fixError}<p class="error-text" role="alert">{fixError}</p>{/if}
    </div>
  {/if}
</div>

<style>
  .ce {
    display: grid;
    gap: 10px;
    min-width: 0;
  }
  .ed-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
  }
  .right {
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
    height: 28px;
    padding: 0 12px;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: var(--mist);
    font: inherit;
    font-size: 13px;
    cursor: pointer;
  }
  .seg button.on {
    background: var(--pier-raised);
    color: var(--foam);
    box-shadow: var(--lift);
  }
  .clean-note {
    font-size: 12px;
    margin: -4px 0 0;
  }
  .indent-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 6px 10px;
    border-radius: var(--r-sm);
    background: var(--buoy-wash);
    color: var(--foam);
    font-size: 12.5px;
  }
  .indent-acts {
    display: flex;
    gap: 10px;
  }
  .link-btn {
    border: 0;
    background: none;
    padding: 0;
    color: var(--buoy-text);
    font: inherit;
    cursor: pointer;
    margin-left: 6px;
  }
  .lint {
    display: grid;
    gap: 4px;
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .lint-head {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
    margin-bottom: 4px;
  }
  .lint-head .label {
    margin: 0 4px 0 0;
  }
  .pill {
    height: 20px;
    padding: 0 8px;
    border-radius: 999px;
    font-size: 11px;
    font-weight: 600;
    display: inline-flex;
    align-items: center;
    border: 0;
    font-family: inherit;
  }
  .pill.bad {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .pill.bad.soft {
    font-weight: 500;
  }
  .pill.warn {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .pill.info {
    background: var(--info-wash);
    color: var(--info-text);
    cursor: pointer;
  }
  .issue {
    display: grid;
    grid-template-columns: 96px minmax(0, 1fr) auto;
    align-items: center;
    gap: 10px;
    font-size: 13px;
    padding: 4px 0;
    border-top: 1px solid var(--rope);
  }
  .sev {
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .issue.error .sev,
  .issue.deploy_error .sev {
    color: var(--adrift-text);
  }
  .issue.warning .sev {
    color: var(--signal-text);
  }
  .issue.info .sev {
    color: var(--info-text);
  }
  .msg {
    overflow-wrap: anywhere;
  }
  .fix {
    height: 28px;
    font-size: 12px;
  }
  @media (max-width: 640px) {
    .issue {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
  }
</style>
