<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type MaintenanceWindow } from '../lib/api';
  import { isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import { scheduleText, stateClass, stateText } from '../lib/maintenance';

  let windows = $state<MaintenanceWindow[] | null>(null);
  let error = $state('');

  async function load() {
    try {
      windows = (await api.maintenance()).windows;
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => void load());
    const t = setInterval(() => !document.hidden && void load(), 30000);
    return () => clearInterval(t);
  });

  const active = $derived((windows ?? []).filter((w) => w.state === 'active').length);
  const upcoming = $derived((windows ?? []).filter((w) => w.state === 'upcoming').length);
</script>

<div class="page">
  <section class="card card-pad head">
    <div>
      <h1 class="title">Maintenance</h1>
      <p class="meta">
        {#if windows}
          {active ? `${active} active · ` : ''}{upcoming} upcoming · planned work sends no alerts and shows as Maintenance on status pages
        {:else}Loading…{/if}
      </p>
    </div>
    {#if isAdmin()}
      <a class="btn btn-primary" href="#/maintenance/new"><Icon name="plus" /> Plan maintenance</a>
    {/if}
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if windows && windows.length === 0}
    <section class="card card-pad empty">
      <p class="section-title">No maintenance planned</p>
      <p class="meta">
        Plan work ahead, once or every week (say Sunday 02:00–03:00 for backups). While a window runs, failing checks in
        it are marked Maintenance instead of Down and send no alert, and status pages show your message. A check still
        down when the window ends alerts right away.
      </p>
      {#if isAdmin()}<a class="btn btn-primary" href="#/maintenance/new"><Icon name="plus" /> Plan the first window</a>{/if}
    </section>
  {:else if windows}
    <section class="card list" aria-label="Maintenance windows">
      {#each windows as w (w.id)}
        <a class="row" href={`#/maintenance/${w.id}`} class:dim={w.state === 'past' || w.state === 'disabled'}>
          <span class="who">
            <span class="name-row">
              <span class="name">{w.title}</span>
              <span class="badge {stateClass[w.state]}">
                {#if w.state === 'active'}<span class="dot maintenance"></span>{/if}{stateText(w)}
              </span>
            </span>
            <span class="meta sub">{scheduleText(w)}</span>
            <span class="meta sub scope">{w.scopeText.join(' · ') || 'Nothing left in scope'}</span>
          </span>
          <span class="go meta">{isAdmin() ? 'Edit' : 'View'}</span>
        </a>
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
    color: inherit;
    text-decoration: none;
  }
  .row + .row {
    border-top: 1px solid var(--rope);
  }
  .row:hover {
    background: var(--rope);
    text-decoration: none;
  }
  .row.dim .name {
    color: var(--mist);
  }
  .who {
    flex: 1;
    min-width: 0;
    display: grid;
    gap: 3px;
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
    overflow-wrap: anywhere;
  }
  .sub {
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  .badge {
    white-space: normal;
    height: auto;
    min-height: 22px;
  }
  .go {
    flex: none;
  }

  @media (max-width: 767px) {
    .row {
      padding: 12px 14px;
    }
    .go {
      display: none;
    }
  }
</style>
