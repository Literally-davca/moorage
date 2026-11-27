<script lang="ts">
  import { api } from '../lib/api';
  import Icon from '../lib/Icon.svelte';

  // Files tab (M19M): text files of the stack folder (configs next to compose.yaml).
  // compose.yaml and .env stay in their own editors. Paths never leave the folder or
  // follow symlinks; the previous contents of a saved file are kept (last 10).
  let { name, editable }: { name: string; editable: boolean } = $props();

  type F = { path: string; size: number; dir?: boolean; symlink?: boolean; managed?: boolean; modTime: string };
  let files = $state<F[]>([]);
  let error = $state('');
  let open = $state<string | null>(null);
  let content = $state('');
  let saved = $state('');
  let busy = $state(false);
  let msg = $state('');
  let versions = $state<string[]>([]);
  let newPath = $state('');

  async function load() {
    try {
      files = (await api.stackFiles(name)) as F[];
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }
  $effect(() => {
    void name;
    void load();
  });

  async function openFile(p: string) {
    msg = '';
    try {
      content = saved = (await api.stackFile(name, p)).content;
      open = p;
      versions = await api.stackFileVersions(name, p);
    } catch (e) {
      msg = (e as Error).message;
    }
  }
  async function save() {
    if (!open) return;
    busy = true;
    msg = '';
    try {
      await api.saveStackFile(name, open, content);
      saved = content;
      msg = 'Saved. The previous version is kept below. Restart the stack if the service reads it only at start.';
      versions = await api.stackFileVersions(name, open);
      void load();
    } catch (e) {
      msg = (e as Error).message;
    } finally {
      busy = false;
    }
  }
  async function loadVersion(id: string) {
    if (!open) return;
    try {
      content = (await api.stackFileVersion(name, open, id)).content;
      msg = 'Loaded the version of ' + id.slice(0, 8) + ' – Save to use it.';
    } catch (e) {
      msg = (e as Error).message;
    }
  }
  function createFile() {
    const p = newPath.trim().replace(/^\/+/, '');
    if (!p) return;
    open = p;
    content = saved = '';
    versions = [];
    newPath = '';
  }
  const dirty = $derived(open !== null && content !== saved);
  function size(n: number) {
    return n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KB` : `${(n / 1048576).toFixed(1)} MB`;
  }
</script>

<section class="card card-pad files">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
  <div class="grid">
    <ul class="list" aria-label="Files">
      {#each files as f (f.path)}
        <li style:padding-left={(f.path.split('/').length - 1) * 12 + 'px'}>
          {#if f.dir}
            <span class="meta">{f.path.split('/').at(-1)}/</span>
          {:else if f.symlink}
            <span class="meta" title="Symbolic link – edit it on the host">{f.path.split('/').at(-1)} →</span>
          {:else if f.managed}
            <span class="meta" title="Edited in the Compose / .env editor">{f.path}</span>
          {:else}
            <button type="button" class="file" class:cur={open === f.path} onclick={() => openFile(f.path)}>
              {f.path.split('/').at(-1)} <span class="meta">{size(f.size)}</span>
            </button>
          {/if}
        </li>
      {/each}
      {#if editable}
        <li class="new">
          <input class="field mono" bind:value={newPath} placeholder="conf/new-file.conf" aria-label="New file path" />
          <button type="button" class="btn btn-sm" onclick={createFile} disabled={!newPath.trim()}><Icon name="plus" /> New</button>
        </li>
      {/if}
    </ul>
    <div class="editor">
      {#if open}
        <div class="bar">
          <strong class="mono">{open}</strong>
          {#if editable}
            <button type="button" class="btn btn-sm btn-primary" onclick={save} disabled={busy || !dirty}>Save</button>
          {:else}
            <span class="meta">read-only</span>
          {/if}
        </div>
        <textarea class="field mono text" bind:value={content} readonly={!editable} spellcheck="false" aria-label="Contents of {open}"></textarea>
        {#if versions.length}
          <details>
            <summary class="meta">Previous versions ({versions.length})</summary>
            {#each versions as v (v)}
              <button type="button" class="btn btn-ghost btn-sm" onclick={() => loadVersion(v)}>{v.slice(0, 4)}-{v.slice(4, 6)}-{v.slice(6, 8)} {v.slice(9, 11)}:{v.slice(11, 13)}</button>
            {/each}
          </details>
        {/if}
      {:else}
        <p class="meta">Pick a file. Text files up to 512 KB open here; compose.yaml and .env are in the Compose tab.</p>
      {/if}
      {#if msg}<p class="meta" role="status">{msg}</p>{/if}
    </div>
  </div>
</section>

<style>
  .grid {
    display: grid;
    grid-template-columns: minmax(180px, 260px) 1fr;
    gap: 12px;
  }
  @media (max-width: 760px) {
    .grid {
      grid-template-columns: 1fr;
    }
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 2px;
    align-content: start;
    max-height: 60vh;
    overflow: auto;
  }
  .file {
    all: unset;
    cursor: pointer;
    display: flex;
    gap: 6px;
    justify-content: space-between;
    width: 100%;
    padding: 2px 4px;
    border-radius: var(--r-sm);
  }
  .file.cur,
  .file:hover {
    background: var(--rope-strong);
  }
  .new {
    display: flex;
    gap: 4px;
    margin-top: 8px;
  }
  .editor {
    display: grid;
    gap: 8px;
    align-content: start;
    min-width: 0;
  }
  .bar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
  }
  .text {
    min-height: 360px;
    resize: vertical;
    font-size: 12.5px;
    white-space: pre;
  }
</style>
