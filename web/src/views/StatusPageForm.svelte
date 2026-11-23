<script lang="ts">
  import { untrack } from 'svelte';
  import {
    api,
    type Check,
    type HostEntry,
    type PublicStatusPage,
    type StackSummary,
    type StatusPageInput,
  } from '../lib/api';
  import { isAdmin } from '../lib/state.svelte';
  import { kindName } from '../lib/checks';
  import Icon from '../lib/Icon.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';
  import Confirm from '../lib/Confirm.svelte';

  let { id }: { id: number | null } = $props();

  let currentId = $state<number | null>(null);
  let form = $state<StatusPageInput>({
    slug: '',
    title: '',
    description: '',
    domain: '',
    published: false,
    showIncidents: true,
    items: [],
  });
  let slugTouched = $state(false);
  let loaded = $state(false);
  let checks = $state<Check[]>([]);
  let hosts = $state<HostEntry[]>([]);
  let hostStacks = $state<Record<string, StackSummary[]>>({});


  let saving = $state(false);
  let saveOk = $state(false);
  let error = $state('');
  let previewing = $state(false);
  let preview = $state<PublicStatusPage | null>(null);
  let confirmDelete = $state(false);
  // Slug of the saved, published page – the link must not follow unsaved edits.
  let liveSlug = $state('');

  function slugify(s: string): string {
    return s
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
      .slice(0, 63);
  }

  $effect(() => {
    untrack(() => {
      void init();
    });
  });

  async function init() {
    currentId = id;
    try {
      const [chs, hs] = await Promise.all([api.checks(), api.hosts()]);
      checks = chs;
      hosts = hs.hosts;
      const byHost: Record<string, StackSummary[]> = {};
      await Promise.all(
        hs.hosts.map(async (h) => {
          if (!h.connected) {
            byHost[h.id] = [];
            return;
          }
          try {
            const r = await api.stacksOf(h.id);
            byHost[h.id] = r.stacks;
          } catch {
            byHost[h.id] = [];
          }
        }),
      );
      hostStacks = byHost;
    } catch {
      /* handled below */
    }
    if (id) {
      try {
        const p = await api.statusPage(id);
        form = {
          slug: p.slug,
          title: p.title,
          description: p.description,
          domain: p.domain,
          published: p.published,
          showIncidents: p.showIncidents,
          items: p.items.map((it) => ({ ...it })),
        };
        slugTouched = true;
        liveSlug = p.published ? p.slug : '';
      } catch (e) {
        error = (e as Error).message;
      }
    }
    loaded = true;
  }

  interface StackChoice {
    key: string;
    hostId: string;
    hostName: string;
    stack: string;
    checkCount: number;
  }

  const stackChoices = $derived.by((): StackChoice[] => {
    const counts: Record<string, number> = {};
    for (const c of checks) {
      if (c.hostId && c.stack) {
        const k = `${c.hostId}/${c.stack}`;
        counts[k] = (counts[k] ?? 0) + 1;
      }
    }
    const seen = new Set<string>();
    const out: StackChoice[] = [];
    const hostMap = Object.fromEntries(hosts.map((h) => [h.id, h.name]));

    for (const h of hosts) {
      for (const s of hostStacks[h.id] ?? []) {
        const k = `${h.id}/${s.name}`;
        seen.add(k);
        out.push({ key: k, hostId: h.id, hostName: h.name, stack: s.name, checkCount: counts[k] ?? 0 });
      }
    }
    for (const c of checks) {
      if (c.hostId && c.stack) {
        const k = `${c.hostId}/${c.stack}`;
        if (!seen.has(k)) {
          seen.add(k);
          out.push({
            key: k,
            hostId: c.hostId,
            hostName: hostMap[c.hostId] ?? c.hostId,
            stack: c.stack,
            checkCount: counts[k] ?? 0,
          });
        }
      }
    }
    return out.sort((a, b) => a.hostName.localeCompare(b.hostName) || a.stack.localeCompare(b.stack));
  });

  function onTitleInput() {
    if (!slugTouched && !currentId) {
      form.slug = slugify(form.title);
    }
  }

  // The pickers add and remove items; the list below keeps their order, names and sections.
  const pickedChecks = $derived(form.items.filter((it) => it.checkId).map((it) => String(it.checkId)));
  const pickedStacks = $derived(form.items.filter((it) => it.stack).map((it) => `${it.hostId}/${it.stack}`));
  const checkItems = $derived(checks.map((c) => ({ value: String(c.id), label: c.name, meta: kindName[c.kind] })));
  const stackItems = $derived(
    stackChoices.map((sc) => ({
      value: sc.key,
      label: sc.stack,
      meta: `on ${sc.hostName}${sc.checkCount ? ` · ${sc.checkCount} ${sc.checkCount === 1 ? 'check' : 'checks'}` : ''}`,
      disabled: sc.checkCount === 0 ? 'Add a check to this stack first' : undefined,
    })),
  );
  function setChecks(next: string[]) {
    const want = new Set(next.map(Number));
    const have = new Set(form.items.filter((it) => it.checkId).map((it) => it.checkId));
    const kept = form.items.filter((it) => !it.checkId || want.has(it.checkId));
    const added = checks
      .filter((c) => want.has(c.id) && !have.has(c.id))
      .map((c) => ({ section: '', displayName: c.name, checkId: c.id }));
    form.items = [...kept, ...added];
  }
  function setStacks(next: string[]) {
    const want = new Set(next);
    const key = (it: StatusPageInput['items'][number]) => `${it.hostId}/${it.stack}`;
    const have = new Set(form.items.filter((it) => it.stack).map(key));
    const kept = form.items.filter((it) => !it.stack || want.has(key(it)));
    const added = stackChoices
      .filter((sc) => want.has(sc.key) && !have.has(sc.key) && sc.checkCount > 0)
      .map((sc) => ({ section: '', displayName: sc.stack, hostId: sc.hostId, stack: sc.stack }));
    form.items = [...kept, ...added];
  }

  function moveItem(idx: number, dir: -1 | 1) {
    const target = idx + dir;
    if (target < 0 || target >= form.items.length) return;
    const copy = [...form.items];
    [copy[idx], copy[target]] = [copy[target], copy[idx]];
    form.items = copy;
  }

  function removeItem(idx: number) {
    form.items = form.items.filter((_, i) => i !== idx);
  }

  function itemSourceLabel(it: StatusPageInput['items'][number]): string {
    if (it.checkId) {
      const c = checks.find((x) => x.id === it.checkId);
      return c ? `Check: ${c.name} (${kindName[c.kind]})` : `Check #${it.checkId}`;
    }
    const h = hosts.find((x) => x.id === it.hostId);
    return `Stack: ${it.stack} on ${h?.name ?? it.hostId}`;
  }

  async function save(e?: SubmitEvent) {
    e?.preventDefault();
    saving = true;
    saveOk = false;
    error = '';
    try {
      const snap = $state.snapshot(form) as StatusPageInput;
      const r = await api.saveStatusPage(currentId, snap);
      currentId = r.id;
      liveSlug = snap.published ? snap.slug : '';
      saveOk = true;
      setTimeout(() => (saveOk = false), 2500);
      if (!id) {
        location.hash = `#/status-pages/${r.id}`;
      }
      if (preview) {
        preview = await api.previewStatusPage(r.id);
      }
    } catch (err) {
      error = (err as Error).message;
    } finally {
      saving = false;
    }
  }

  // Preview never saves: saving a published page would make half-finished edits public.
  async function runPreview() {
    previewing = true;
    error = '';
    try {
      const snap = $state.snapshot(form) as StatusPageInput;
      preview = isAdmin()
        ? await api.previewStatusDraft(currentId, snap)
        : currentId
          ? await api.previewStatusPage(currentId)
          : null;
    } catch (err) {
      error = (err as Error).message;
    } finally {
      previewing = false;
    }
  }

  async function del() {
    if (!currentId) return;
    try {
      await api.deleteStatusPage(currentId);
      location.hash = '#/status-pages';
    } catch (err) {
      error = (err as Error).message;
    }
  }
</script>

<form class="page" onsubmit={save}>
  <section class="card card-pad head">
    <div class="head-row">
      <div>
        <a class="back meta" href="#/status-pages"><Icon name="chevronLeft" /> Status pages</a>
        <h1 class="title">{currentId ? form.title || 'Edit status page' : 'Add status page'}</h1>
      </div>
      <div class="head-actions">
        {#if liveSlug}
          <a class="btn" href={`/status/${encodeURIComponent(liveSlug)}`} target="_blank" rel="noopener">
            <Icon name="world" /> Open public page
          </a>
        {/if}
        {#if currentId && isAdmin()}
          <button type="button" class="btn btn-ghost danger" onclick={() => (confirmDelete = true)} aria-label="Delete status page">
            <Icon name="trash" />
          </button>
        {/if}
      </div>
    </div>
  </section>

  {#if !loaded}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:40%"></div></section>
  {:else}
    <section class="card card-pad grid">
      <div class="two">
        <div>
          <label class="label" for="sp-title">Title</label>
          <input
            id="sp-title"
            class="field"
            bind:value={form.title}
            oninput={onTitleInput}
            maxlength="100"
            placeholder="Moorage Status"
            required
            disabled={!isAdmin()}
          />
        </div>
        <div>
          <label class="label" for="sp-slug">URL slug</label>
          <input
            id="sp-slug"
            class="field mono"
            bind:value={form.slug}
            oninput={() => (slugTouched = true)}
            maxlength="63"
            placeholder="main"
            required
            disabled={!isAdmin()}
          />
          <p class="hint">Served at <code>/status/{form.slug || 'slug'}</code> and <code>/status/{form.slug || 'slug'}/feed.xml</code>.</p>
        </div>
      </div>

      <div>
        <label class="label" for="sp-desc">Description <span class="opt">optional</span></label>
        <input
          id="sp-desc"
          class="field"
          bind:value={form.description}
          maxlength="500"
          placeholder="Live status and 30-day uptime of our services."
          disabled={!isAdmin()}
        />
      </div>

      <div>
        <label class="label" for="sp-dom">Custom domain <span class="opt">optional</span></label>
        <input
          id="sp-dom"
          class="field mono"
          bind:value={form.domain}
          placeholder="status.example.com"
          spellcheck="false"
          autocomplete="off"
          disabled={!isAdmin()}
        />
        <p class="hint">
          Point the domain at your reverse proxy and forward it to Moorage. On this domain Moorage serves only this page.
        </p>
      </div>

      <div class="switches">
        <label class="checkbox">
          <input type="checkbox" bind:checked={form.published} disabled={!isAdmin()} />
          <span><strong>Published</strong> · visible to anyone without signing in</span>
        </label>
        <label class="checkbox">
          <input type="checkbox" bind:checked={form.showIncidents} disabled={!isAdmin()} />
          <span><strong>Show incident history</strong> · lists start/end times and durations on the page and in the RSS feed (never causes or log lines)</span>
        </label>
      </div>
    </section>

    <section class="card card-pad grid">
      <div>
        <h2 class="section-title">Services on this page</h2>
        <p class="hint">
          The public sees only the display name, status, uptime % and 30-day daily bars. Internal check names, URLs, hostnames, probe messages and logs are never shown.
        </p>
      </div>

      {#if isAdmin()}
        <div class="adder">
          <div>
            <span class="label">Checks</span>
            <MultiSelect label="Checks on this page" placeholder={checks.length ? 'Pick checks' : 'No checks configured yet'}
              items={checkItems} selected={pickedChecks} onchange={setChecks} />
          </div>
          <div>
            <span class="label">Stacks <span class="opt">all checks of the stack</span></span>
            <MultiSelect label="Stacks on this page" placeholder="Pick stacks" items={stackItems} selected={pickedStacks} onchange={setStacks} />
          </div>
        </div>
      {/if}

      {#if form.items.length === 0}
        <p class="meta">No services added yet. Add a check or a stack above.</p>
      {:else}
        <div class="item-list">
          {#each form.items as it, idx (idx)}
            <div class="item-row">
              <div class="item-fields">
                <div>
                  <label class="label" for={`dn-${idx}`}>Public display name</label>
                  <input
                    id={`dn-${idx}`}
                    class="field"
                    bind:value={it.displayName}
                    maxlength="100"
                    required
                    disabled={!isAdmin()}
                  />
                </div>
                <div>
                  <label class="label" for={`sec-${idx}`}>Section heading <span class="opt">optional</span></label>
                  <input
                    id={`sec-${idx}`}
                    class="field"
                    bind:value={it.section}
                    maxlength="100"
                    placeholder="Core services"
                    disabled={!isAdmin()}
                  />
                </div>
              </div>
              <div class="item-foot">
                <span class="meta">{itemSourceLabel(it)} · public sees only “{it.displayName}”</span>
                {#if isAdmin()}
                  <div class="item-btns">
                    <button type="button" class="btn btn-ghost btn-icon" onclick={() => moveItem(idx, -1)} disabled={idx === 0} title="Move up" aria-label="Move up">
                      <Icon name="arrowUp" />
                    </button>
                    <button type="button" class="btn btn-ghost btn-icon" onclick={() => moveItem(idx, 1)} disabled={idx === form.items.length - 1} title="Move down" aria-label="Move down">
                      <Icon name="arrowDown" />
                    </button>
                    <button type="button" class="btn btn-ghost btn-icon danger" onclick={() => removeItem(idx)} title="Remove item" aria-label="Remove item">
                      <Icon name="trash" />
                    </button>
                  </div>
                {/if}
              </div>
            </div>
          {/each}
        </div>
      {/if}

      {#if error}<p class="error-text" role="alert">{error}</p>{/if}

      <div class="buttons">
        <button
          type="button"
          class="btn"
          onclick={runPreview}
          disabled={previewing || !form.title.trim() || !form.slug.trim()}
          aria-busy={previewing}
        >
          Preview
        </button>
        {#if liveSlug}
          <a class="btn" href={`/status/${encodeURIComponent(liveSlug)}`} target="_blank" rel="noopener">Open public page</a>
        {/if}
        <span class="grow"></span>
        {#if saveOk}<span class="meta ok"><Icon name="check" /> Saved</span>{/if}
        <a class="btn btn-ghost" href="#/status-pages">Cancel</a>
        {#if isAdmin()}
          <button class="btn btn-primary" disabled={saving || !form.title.trim() || !form.slug.trim()} aria-busy={saving}>
            {currentId ? 'Save' : 'Create status page'}
          </button>
        {/if}
      </div>
    </section>

    {#if preview}
      <section class="card card-pad preview">
        <div class="prev-head">
          <span class="section-title">Public preview ({form.published ? 'Published' : 'Draft — not public yet'})</span>
          <span class="badge {preview.bannerStatus === 'operational' ? 'up' : preview.bannerStatus === 'down' ? 'down' : preview.bannerStatus === 'maintenance' ? 'degraded' : ''}">
            {preview.bannerText}
          </span>
        </div>
        {#each preview.sections as sec, si (si)}
          <div class="prev-sec">
            {#if sec.name}<h3 class="prev-sec-title">{sec.name}</h3>{/if}
            {#each sec.items as it, ii (ii)}
              <div class="prev-item">
                <div class="prev-top">
                  <span class="prev-name">{it.displayName}</span>
                  <span class="meta num">24 h {it.uptime24hText} · 30 d {it.uptime30dText} · <strong>{it.statusLabel}</strong></span>
                </div>
                <div class="prev-strip">
                  {#each it.days as d, di (di)}
                    <span class="ptick {d.status}" title={d.tooltip}></span>
                  {/each}
                </div>
              </div>
            {/each}
          </div>
        {/each}
      </section>
    {/if}
  {/if}
</form>

<Confirm
  bind:open={confirmDelete}
  title="Delete {form.title || 'this status page'}?"
  danger
  confirmLabel="Delete status page"
  body="Removes the status page and its public URL. The underlying checks and stacks are untouched."
  onConfirm={del}
/>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 860px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 16px;
    flex-wrap: wrap;
  }
  .head-actions {
    display: flex;
    gap: 8px;
  }
  .back {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    margin-bottom: 6px;
    font-size: 12px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .grid {
    display: grid;
    gap: 16px;
  }
  .two {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }
  .opt {
    font-weight: 400;
    color: var(--haze);
    margin-left: 4px;
  }
  .switches {
    display: grid;
    gap: 10px;
  }
  .adder {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 12px;
  }
  .item-list {
    display: grid;
    gap: 10px;
  }
  .item-row {
    padding: 12px 14px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    display: grid;
    gap: 8px;
  }
  .item-fields {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 10px;
  }
  .item-foot {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .item-btns {
    display: flex;
    gap: 4px;
  }
  .danger {
    color: var(--adrift-text);
  }
  .buttons {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .ok {
    color: var(--afloat-text);
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .preview {
    display: grid;
    gap: 14px;
  }
  .prev-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .prev-sec {
    display: grid;
    gap: 10px;
  }
  .prev-sec-title {
    font-size: 13px;
    font-weight: 600;
    color: var(--mist);
  }
  .prev-item {
    display: grid;
    gap: 6px;
  }
  .prev-top {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .prev-name {
    font-weight: 500;
    color: var(--foam);
  }
  .prev-strip {
    display: grid;
    grid-template-columns: repeat(30, minmax(0, 1fr));
    gap: 3px;
    height: 18px;
    align-items: end;
  }
  .ptick {
    display: block;
    height: 16px;
    border-radius: 3px;
    background: var(--rope-strong);
  }
  .ptick.operational {
    background: var(--afloat);
  }
  .ptick.down {
    background: var(--adrift);
    height: 18px;
  }
  .ptick.maintenance {
    background: var(--signal);
    height: 12px;
  }
  .ptick.paused {
    background: var(--becalmed);
    height: 12px;
  }

  @media (max-width: 767px) {
    .two,
    .item-fields {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
