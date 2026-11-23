<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type StatusPage } from '../lib/api';
  import { isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';

  let pages = $state<StatusPage[] | null>(null);
  let error = $state('');
  let copiedId = $state<number | null>(null);

  async function load() {
    try {
      pages = await api.statusPages();
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => {
      void load();
    });
    const t = setInterval(() => !document.hidden && void load(), 10000);
    return () => clearInterval(t);
  });

  function fullURL(p: StatusPage): string {
    if (p.publicUrl.startsWith('http://') || p.publicUrl.startsWith('https://')) {
      return p.publicUrl;
    }
    return `${location.origin}${p.publicUrl}`;
  }

  async function copyURL(e: MouseEvent, p: StatusPage) {
    e.preventDefault();
    e.stopPropagation();
    try {
      await navigator.clipboard.writeText(fullURL(p));
      copiedId = p.id;
      setTimeout(() => {
        if (copiedId === p.id) copiedId = null;
      }, 2000);
    } catch {
      /* clipboard may be blocked */
    }
  }

  function dotClass(st: StatusPage['status']): string {
    if (st === 'operational') return 'up';
    if (st === 'down') return 'down';
    if (st === 'maintenance') return 'degraded';
    return 'paused';
  }
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Status pages</h1>
      <p class="meta">
        {#if pages}
          {pages.length} {pages.length === 1 ? 'page' : 'pages'} · public uptime view without sign-in
        {:else}Loading…{/if}
      </p>
    </div>
    {#if isAdmin()}
      <a class="btn btn-primary" href="#/status-pages/new"><Icon name="plus" /> Add status page</a>
    {/if}
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if pages && pages.length === 0}
    <section class="card card-pad empty">
      <p class="section-title">No status pages yet</p>
      <p class="meta">
        Create a public status page for your users or friends. Pick which checks or stacks to show, give each one a
        public display name, and optionally serve the page on its own domain with an RSS feed.
      </p>
      {#if isAdmin()}<a class="btn btn-primary" href="#/status-pages/new"><Icon name="plus" /> Add the first status page</a>{/if}
    </section>
  {:else if pages}
    <section class="card list" aria-label="Status pages">
      {#each pages as p (p.id)}
        <div class="row">
          <span class="dot {dotClass(p.status)}" title={p.statusText}></span>
          <a class="who" href={`#/status-pages/${p.id}`}>
            <span class="name-row">
              <span class="name">{p.title}</span>
              <span class="tag {p.published ? 'pub' : 'draft'}">{p.published ? 'Published' : 'Draft'}</span>
            </span>
            <span class="meta sub">
              <span class="mono">{p.domain ? p.domain : `/status/${p.slug}`}</span>
              <span>· {p.items.length} {p.items.length === 1 ? 'item' : 'items'}</span>
              <span>· {p.statusText}</span>
            </span>
          </a>
          <div class="actions">
            <button type="button" class="btn btn-ghost" onclick={(e) => copyURL(e, p)} title="Copy public URL">
              <Icon name={copiedId === p.id ? 'check' : 'copy'} />
              <span>{copiedId === p.id ? 'Copied' : 'Copy URL'}</span>
            </button>
            {#if p.published}
              <a class="btn" href={`/status/${encodeURIComponent(p.slug)}`} target="_blank" rel="noopener">Open</a>
            {/if}
            <a class="btn" href={`#/status-pages/${p.id}`}>{isAdmin() ? 'Edit' : 'View'}</a>
          </div>
        </div>
      {/each}
    </section>
  {:else}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:50%"></div></section>
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 16px;
    flex-wrap: wrap;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .empty {
    display: grid;
    gap: 8px;
    justify-items: start;
  }
  .empty .meta {
    max-width: 62ch;
  }
  .list {
    padding: 4px 0;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 14px;
    padding: 14px 20px;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .who {
    flex: 1;
    min-width: 0;
    display: grid;
    gap: 3px;
    color: inherit;
    text-decoration: none;
  }
  .who:hover .name {
    text-decoration: underline;
  }
  .name-row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .name {
    font-weight: 500;
    color: var(--foam);
  }
  .tag {
    display: inline-flex;
    align-items: center;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .tag.pub {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .sub {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    font-size: 12px;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }

  @media (max-width: 767px) {
    .row {
      flex-wrap: wrap;
      padding: 12px 14px;
    }
    .actions {
      width: 100%;
      padding-left: 22px;
    }
  }
</style>
