<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type TemplateCatalog, type TemplateEntry } from '../lib/api';
  import { isAdmin, link } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import AddonGate from '../lib/AddonGate.svelte';
  import SetupHint from '../lib/SetupHint.svelte';

  let catalog = $state<TemplateCatalog | null>(null);
  let error = $state('');
  let query = $state('');
  let category = $state('');

  async function load() {
    try {
      catalog = await api.templates();
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }
  $effect(() => {
    untrack(() => void load());
  });

  const categories = $derived(
    [...new Set((catalog?.templates ?? []).filter((t) => !t.error && t.category).map((t) => t.category))].sort(),
  );
  const shown = $derived.by(() => {
    const q = query.trim().toLowerCase();
    return (catalog?.templates ?? []).filter((t) => {
      if (category && t.category !== category) return false;
      if (!q) return true;
      return [t.name, t.id, t.description, t.category, t.sourceLabel].some((s) => s?.toLowerCase().includes(q));
    });
  });
  const problems = $derived((catalog?.sources ?? []).filter((s) => s.error));

  const href = (t: TemplateEntry) => link(`templates/${encodeURIComponent(t.source)}/${encodeURIComponent(t.id)}`);
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Templates</h1>
      <p class="meta">
        Pick an app, fill in a few values and review the compose file before the stack is created.
      </p>
    </div>
    <div class="head-actions">
      {#if isAdmin()}
        <a class="btn" href="#/settings/templates"><Icon name="world" /> Template sources</a>
      {/if}
      <a class="btn btn-ghost" href={link('new')}><Icon name="plus" /> Blank stack</a>
    </div>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if problems.length}
    <section class="problems-wrap" aria-label="Source problems">
      {#each problems as p (p.key)}
        {#if p.key === 'local'}
          <!-- Moorage creates the folder at start and an empty one is normal; this only
               shows when it exists but can't be read (permissions, a file in the way). -->
          <SetupHint
            tone="warn"
            title="{p.label}{catalog?.localDir ? `: ${catalog.localDir}` : ''}"
            detail="{p.error} Check that Moorage's user can read this folder (or set MOORAGE_TEMPLATES_DIR)."
            actionLabel={isAdmin() ? 'Template sources' : ''}
            actionHref={isAdmin() ? '#/settings/templates' : ''}
          />
        {:else}
          <div class="card card-pad problem">
            <Icon name="alert" />
            <span><strong>{p.label}</strong>{p.name ? ` (${p.name})` : ''}: {p.error}</span>
          </div>
        {/if}
      {/each}
    </section>
  {/if}

  {#if catalog && !catalog.sources.some((s) => s.key === 'official')}
    <AddonGate
      addonId="official-templates"
      featureTitle="Official Moorage templates"
      featureDescription="Enable curated self-hosted app templates alongside any custom local or remote template sources."
      allowManualOverride={false}
      onchanged={load}
    >
      <span></span>
    </AddonGate>
  {/if}

  <div class="filters">
    <label class="search field">
      <Icon name="search" />
      <span class="sr-only">Search templates</span>
      <input bind:value={query} placeholder="Search templates" spellcheck="false" />
    </label>
    {#if categories.length > 1}
      <div class="chips" role="group" aria-label="Category">
        <button type="button" class="chip" aria-pressed={category === ''} onclick={() => (category = '')}>All</button>
        {#each categories as c (c)}
          <button type="button" class="chip" aria-pressed={category === c} onclick={() => (category = category === c ? '' : c)}>{c}</button>
        {/each}
      </div>
    {/if}
  </div>

  {#if catalog === null && !error}
    <div class="grid">
      {#each [1, 2, 3, 4] as i (i)}<div class="card tpl"><span class="skeleton" style="height:12px;width:{80 + i * 20}px"></span></div>{/each}
    </div>
  {:else if catalog && shown.length === 0}
    <section class="card card-pad"><p class="meta">{query || category ? 'No template matches.' : 'No templates yet.'}</p></section>
  {:else}
    <div class="grid">
      {#each shown as t (t.source + '/' + t.id)}
        {#if t.error}
          <div class="card tpl broken">
            <div class="top">
              <span class="name">{t.name}</span>
              <span class="badge down">Can't be used</span>
            </div>
            <p class="desc error-text">{t.error}</p>
            <p class="meta src">{t.sourceLabel}</p>
          </div>
        {:else}
          <a class="card tpl" href={href(t)}>
            <div class="top">
              <span class="name">{t.name}</span>
              {#if t.category}<span class="badge">{t.category}</span>{/if}
            </div>
            <p class="desc">{t.description}</p>
            <p class="meta src">{t.sourceLabel}{t.version ? ` · version ${t.version}` : ''}</p>
          </a>
        {/if}
      {/each}
    </div>
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 1200px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
    flex-wrap: wrap;
  }
  .head-actions {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .problems-wrap {
    display: grid;
    gap: 8px;
  }
  .problem {
    display: flex;
    gap: 8px;
    font-size: 13px;
    color: var(--signal-text);
    overflow-wrap: anywhere;
  }
  .filters {
    display: grid;
    gap: 10px;
  }
  .search {
    display: flex;
    align-items: center;
    gap: 8px;
    max-width: 380px;
    width: 100%;
    padding: 0 12px;
    min-height: 36px;
    cursor: text;
  }
  .search :global(.icon) {
    flex-shrink: 0;
    color: var(--haze);
  }
  .search input {
    flex: 1;
    min-width: 0;
    border: 0;
    background: transparent;
    color: var(--foam);
    font: inherit;
    outline: none;
    padding: 0;
  }
  .search input::placeholder {
    color: var(--haze);
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    padding: 4px 10px;
    border: 1px solid var(--rope-stronger);
    border-radius: 999px;
    background: transparent;
    color: var(--haze);
    font: inherit;
    font-size: 12px;
    cursor: pointer;
  }
  .chip[aria-pressed='true'] {
    background: var(--buoy-wash);
    border-color: var(--buoy);
    color: var(--foam);
  }
  .chip:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: 1px;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 260px), 1fr));
    gap: 12px;
  }
  .tpl {
    display: grid;
    align-content: start;
    gap: 8px;
    padding: 14px 18px;
    color: inherit;
    min-width: 0;
  }
  a.tpl:hover {
    text-decoration: none;
    box-shadow: 0 0 0 1px var(--rope-stronger);
  }
  a.tpl:focus-visible {
    outline: none;
    box-shadow:
      0 0 0 2px var(--buoy),
      0 0 0 5px var(--buoy-wash);
  }
  .top {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .name {
    font-weight: 600;
    font-size: 14px;
    color: var(--foam);
    overflow-wrap: anywhere;
  }
  .desc {
    font-size: 13px;
    color: var(--mist);
    overflow-wrap: anywhere;
  }
  .broken .desc {
    color: var(--adrift-text);
  }
  .src {
    overflow-wrap: anywhere;
  }
</style>
