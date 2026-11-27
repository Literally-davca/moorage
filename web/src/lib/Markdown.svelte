<script lang="ts">
  import { parseMarkdown, type Inline, type LinkCheck } from './markdown';
  import Icon from './Icon.svelte';

  // Renders an assistant answer (M19E): Markdown as Svelte nodes, never HTML. Code blocks
  // show their language and a Copy button; wide blocks and tables scroll on their own.
  // plain: render outside content (logs) as text only.
  let { text, check, plain = false }: { text: string; check: LinkCheck; plain?: boolean } = $props();

  const blocks = $derived(plain ? [] : parseMarkdown(text, check));
  let copied = $state(-1);

  async function copy(t: string, i: number) {
    try {
      await navigator.clipboard.writeText(t);
      copied = i;
      setTimeout(() => (copied = -1), 1500);
    } catch {
      copied = -2; // no secure context: ask to select by hand
    }
  }
</script>

{#snippet inl(parts: Inline[])}{#each parts as p, j (j)}{#if p.t === 'text'}{p.text}{:else if p.t === 'code'}<code
        class="md-ic">{p.text}</code
      >{:else if p.t === 'b'}<strong>{p.text}</strong>{:else if p.t === 'i'}<em>{p.text}</em>{:else if p.t === 'a'}{#if p.external}<a
          href={p.href}
          target="_blank"
          rel="noopener noreferrer">{p.text}</a
        >{:else}<a href={p.href}>{p.text}</a>{/if}{/if}{/each}{/snippet}

{#if plain}
  <pre class="md-plain">{text}</pre>
{:else}
  <div class="md">
    {#each blocks as b, i (i)}
      {#if b.t === 'p'}
        <p>{@render inl(b.inl)}</p>
      {:else if b.t === 'h'}
        <p class="md-h md-h{b.level}"><strong>{@render inl(b.inl)}</strong></p>
      {:else if b.t === 'code'}
        <div class="md-code">
          <div class="md-code-bar">
            <span class="meta">{b.lang || 'text'}</span>
            <button type="button" class="btn btn-ghost btn-sm" onclick={() => copy(b.text, i)} aria-label="Copy code">
              <Icon name={copied === i ? 'check' : 'copy'} />
              {copied === i ? 'Copied' : 'Copy'}
            </button>
          </div>
          <pre class="mono">{b.text}</pre>
        </div>
      {:else if b.t === 'ul'}
        <ul>
          {#each b.items as it, j (j)}<li>{@render inl(it)}</li>{/each}
        </ul>
      {:else if b.t === 'ol'}
        <ol start={b.start}>
          {#each b.items as it, j (j)}<li>{@render inl(it)}</li>{/each}
        </ol>
      {:else if b.t === 'quote'}
        <blockquote>{@render inl(b.inl)}</blockquote>
      {:else if b.t === 'table'}
        <div class="md-table">
          <table>
            <thead><tr>{#each b.head as c, j (j)}<th>{@render inl(c)}</th>{/each}</tr></thead>
            <tbody>
              {#each b.rows as r, j (j)}<tr>{#each r as c, k (k)}<td>{@render inl(c)}</td>{/each}</tr>{/each}
            </tbody>
          </table>
        </div>
      {:else if b.t === 'hr'}
        <hr />
      {/if}
    {/each}
    {#if copied === -2}<p class="meta">Select the text to copy it.</p>{/if}
  </div>
{/if}

<style>
  .md {
    display: grid;
    gap: 8px;
    overflow-wrap: anywhere;
    min-width: 0;
  }
  .md :global(p),
  .md ul,
  .md ol,
  .md blockquote {
    margin: 0;
  }
  .md ul,
  .md ol {
    padding-left: 20px;
    display: grid;
    gap: 2px;
  }
  .md-h1,
  .md-h2 {
    font-size: 15px;
  }
  .md-ic {
    padding: 1px 4px;
    border-radius: var(--r-sm);
    background: var(--well);
    font-size: 12.5px;
  }
  .md-code {
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    min-width: 0;
  }
  .md-code-bar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 2px 4px 0 10px;
  }
  .md-code pre {
    margin: 0;
    padding: 6px 10px 10px;
    overflow-x: auto;
    font-size: 12.5px;
    white-space: pre;
  }
  .md blockquote {
    padding-left: 10px;
    border-left: 3px solid var(--rope-strong);
    color: var(--mist, inherit);
  }
  .md-table {
    overflow-x: auto;
  }
  .md table {
    border-collapse: collapse;
    font-size: 13px;
  }
  .md th,
  .md td {
    padding: 4px 8px;
    border-bottom: 1px solid var(--rope);
    text-align: left;
    vertical-align: top;
  }
  .md hr {
    border: 0;
    border-top: 1px solid var(--rope);
    width: 100%;
  }
  .md-plain {
    margin: 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-family: var(--mono);
    font-size: 12px;
  }
</style>
