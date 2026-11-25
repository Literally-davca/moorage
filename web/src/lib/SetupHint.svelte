<script lang="ts">
  import Icon from './Icon.svelte';

  let {
    title,
    detail,
    actionLabel = '',
    actionHref = '',
    onaction,
    snippet = '',
    tone = 'info',
  }: {
    title: string;
    detail: string;
    actionLabel?: string;
    actionHref?: string;
    onaction?: () => void;
    snippet?: string;
    tone?: 'info' | 'warn';
  } = $props();

  let copied = $state(false);
  function copySnippet() {
    if (!snippet) return;
    navigator.clipboard?.writeText(snippet);
    copied = true;
    setTimeout(() => (copied = false), 1500);
  }
</script>

<div class="hint card" class:warn={tone === 'warn'}>
  <div class="hint-head">
    <span class="hint-icon"><Icon name={tone === 'warn' ? 'alert' : 'info'} /></span>
    <div class="hint-copy">
      <strong class="hint-title">{title}</strong>
      <p class="hint-detail">{detail}</p>
    </div>
    {#if actionHref && actionLabel}
      <a class="btn btn-primary hint-act" href={actionHref}>{actionLabel}</a>
    {:else if onaction && actionLabel}
      <button type="button" class="btn btn-primary hint-act" onclick={onaction}>{actionLabel}</button>
    {/if}
  </div>
  {#if snippet}
    <div class="hint-code">
      <pre>{snippet}</pre>
      <button type="button" class="btn btn-ghost" onclick={copySnippet} aria-label="Copy command or env snippet">
        <Icon name={copied ? 'check' : 'copy'} />
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  {/if}
</div>

<style>
  .hint {
    padding: 12px 14px;
    display: grid;
    gap: 10px;
    border-left: 3px solid var(--buoy);
  }
  .hint.warn {
    border-left-color: var(--warn);
  }
  .hint-head {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    flex-wrap: wrap;
  }
  .hint-icon {
    color: var(--buoy-text, var(--buoy));
    margin-top: 2px;
    flex-shrink: 0;
  }
  .hint.warn .hint-icon {
    color: var(--warn);
  }
  .hint-copy {
    flex: 1;
    min-width: 200px;
    display: grid;
    gap: 2px;
  }
  .hint-title {
    color: var(--foam);
    font-size: 13px;
    font-weight: 600;
  }
  .hint-detail {
    margin: 0;
    color: var(--mist);
    font-size: 12.5px;
    line-height: 1.45;
  }
  .hint-act {
    flex-shrink: 0;
  }
  .hint-code {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 10px;
    border-radius: var(--r-sm);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .hint-code pre {
    flex: 1;
    margin: 0;
    font: 12px/1.45 var(--mono);
    color: var(--foam);
    overflow-x: auto;
    white-space: pre-wrap;
    word-break: break-all;
  }
</style>
