<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Check, type HostEntry, type MaintenanceInput, type MaintenanceStack, type MaintenanceWindow } from '../lib/api';
  import { isAdmin } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import { fmtMinutes, fmtRange, stateText, weekdayShort, weekOrder } from '../lib/maintenance';

  let { id }: { id: number | null } = $props();

  let loaded = $state(false);
  let current = $state<MaintenanceWindow | null>(null);
  let hubTz = $state('');
  let checks = $state<Check[]>([]);
  let hosts = $state<HostEntry[]>([]);
  let hostStacks = $state<Record<string, string[]>>({});

  let title = $state('');
  let message = $state('');
  let kind = $state<'once' | 'weekly'>('once');
  let startLocal = $state('');
  let endLocal = $state('');
  let weekdays = $state<number[]>([0]);
  let startTime = $state('02:00');
  let durHours = $state(1);
  let durMins = $state(0);
  let timeZone = $state('');
  let everything = $state(false);
  let stacks = $state<MaintenanceStack[]>([]);
  let checkIds = $state<number[]>([]);
  let enabled = $state(true);

  let saving = $state(false);
  let error = $state('');
  let confirmDelete = $state(false);

  const readOnly = $derived(!isAdmin());
  const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const zones: string[] = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone') ?? [];

  // "2026-11-22T02:00" in the browser's zone, for <input type="datetime-local">
  function toLocalInput(d: Date): string {
    const p = (n: number) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
  }

  async function init() {
    try {
      const [list, chs, hs] = await Promise.all([api.maintenance(), api.checks(), api.hosts()]);
      hubTz = list.hubTimeZone === 'Local' ? '' : list.hubTimeZone;
      checks = chs;
      hosts = hs.hosts;
      const byHost: Record<string, string[]> = {};
      await Promise.all(
        hs.hosts.map(async (h) => {
          byHost[h.id] = [];
          if (!h.connected) return;
          try {
            byHost[h.id] = (await api.stacksOf(h.id)).stacks.map((s) => s.name);
          } catch {
            /* an offline host just offers no stacks */
          }
        }),
      );
      hostStacks = byHost;
      if (id) {
        const w = list.windows.find((x) => x.id === id);
        if (!w) throw new Error('Maintenance window not found.');
        current = w;
        fill(w);
      } else {
        const start = new Date();
        start.setMinutes(0, 0, 0);
        start.setHours(start.getHours() + 1);
        startLocal = toLocalInput(start);
        endLocal = toLocalInput(new Date(start.getTime() + 3600_000));
        timeZone = hubTz;
      }
    } catch (e) {
      error = (e as Error).message;
    }
    loaded = true;
  }

  function fill(w: MaintenanceWindow) {
    title = w.title;
    message = w.message;
    kind = w.kind;
    if (w.startsAt) startLocal = toLocalInput(new Date(w.startsAt));
    if (w.endsAt) endLocal = toLocalInput(new Date(w.endsAt));
    if (w.kind === 'weekly') {
      weekdays = [...w.weekdays];
      startTime = w.startTime ?? '02:00';
      durHours = Math.floor((w.duration ?? 60) / 60);
      durMins = (w.duration ?? 60) % 60;
    }
    timeZone = w.timeZone ?? '';
    everything = !!w.scope.all;
    stacks = [...(w.scope.stacks ?? [])];
    checkIds = [...(w.scope.checks ?? [])];
    enabled = w.enabled;
  }

  $effect(() => {
    untrack(() => void init());
  });

  const hostName = (hid: string) => hosts.find((h) => h.id === hid)?.name ?? hid;
  const stackKey = (s: MaintenanceStack) => `${s.hostId}/${s.stack}`;

  // every stack of every host, plus stacks that only checks still know about
  const stackChoices = $derived.by(() => {
    const seen = new Set(stacks.map(stackKey));
    const out: MaintenanceStack[] = [];
    const add = (s: MaintenanceStack) => {
      const k = stackKey(s);
      if (!seen.has(k)) {
        seen.add(k);
        out.push(s);
      }
    };
    for (const h of hosts) for (const s of hostStacks[h.id] ?? []) add({ hostId: h.id, stack: s });
    for (const c of checks) if (c.hostId && c.stack) add({ hostId: c.hostId, stack: c.stack });
    return out;
  });
  // The pickers list the chosen stacks too, so they can be unticked there.
  const allStacks = $derived([...stacks, ...stackChoices]);
  const stackItems = $derived(allStacks.map((s) => ({ value: stackKey(s), label: s.stack, meta: 'on ' + hostName(s.hostId) })));
  function setStacks(keys: string[]) {
    const byKey = new Map(allStacks.map((s) => [stackKey(s), s]));
    stacks = keys.map((k) => byKey.get(k)).filter((s): s is MaintenanceStack => !!s);
  }
  const checkItems = $derived(checks.map((c) => ({ value: String(c.id), label: c.name })));
  function toggleDay(d: number) {
    weekdays = weekdays.includes(d) ? weekdays.filter((x) => x !== d) : [...weekdays, d];
  }

  const duration = $derived(Math.max(0, Math.round(durHours || 0) * 60 + Math.round(durMins || 0)));

  function input(): MaintenanceInput {
    const base: MaintenanceInput = {
      title: title.trim(),
      message: message.trim(),
      kind,
      weekdays: [],
      enabled,
      scope: everything ? { all: true } : { stacks, checks: checkIds },
    };
    if (kind === 'once') {
      base.startsAt = startLocal ? new Date(startLocal).toISOString() : undefined;
      base.endsAt = endLocal ? new Date(endLocal).toISOString() : undefined;
    } else {
      base.weekdays = [...weekdays].sort();
      base.startTime = startTime;
      base.duration = duration;
      base.timeZone = timeZone.trim();
    }
    return base;
  }

  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (readOnly) return;
    saving = true;
    error = '';
    try {
      await api.saveMaintenance(id, input());
      location.hash = '#/maintenance';
    } catch (err) {
      error = (err as Error).message;
    } finally {
      saving = false;
    }
  }

  async function del() {
    if (!id) return;
    try {
      await api.deleteMaintenance(id);
      location.hash = '#/maintenance';
    } catch (err) {
      error = (err as Error).message;
    }
  }
</script>

<form class="page" onsubmit={save}>
  <section class="card card-pad head">
    <a class="back meta" href="#/maintenance"><Icon name="chevronLeft" /> Maintenance</a>
    <h1 class="title">{id ? (readOnly ? title || 'Maintenance window' : 'Edit maintenance') : 'Plan maintenance'}</h1>
    {#if current}
      <p class="meta state">
        <span class="badge {current.state === 'active' ? 'maintenance' : ''}">{stateText(current)}</span>
        {#if current.state === 'active' && current.next && current.kind === 'weekly'}<span>Then {fmtRange(current.next)}</span>{/if}
        <span>Planned by {current.createdBy || 'unknown'}</span>
      </p>
    {/if}
  </section>

  {#if !loaded}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:40%"></div></section>
  {:else}
    <section class="card card-pad grid">
      <div>
        <label class="label" for="mw-title">Title</label>
        <input id="mw-title" class="field" bind:value={title} maxlength="80" placeholder="Database upgrade" required disabled={readOnly} />
      </div>
      <div>
        <label class="label" for="mw-msg">Message <span class="opt">optional</span></label>
        <textarea id="mw-msg" class="field msg" rows="2" bind:value={message} maxlength="500"
          placeholder="We upgrade the database. The site may be slow or unavailable." disabled={readOnly}></textarea>
        <p class="hint">The title and the message are public: status pages that list an affected service show them.</p>
      </div>
    </section>

    <section class="card card-pad grid">
      <fieldset class="plain">
        <legend class="label">When</legend>
        <div class="seg" role="radiogroup" aria-label="Schedule">
          <button type="button" role="radio" aria-checked={kind === 'once'} class:on={kind === 'once'} onclick={() => (kind = 'once')} disabled={readOnly}>Once</button>
          <button type="button" role="radio" aria-checked={kind === 'weekly'} class:on={kind === 'weekly'} onclick={() => (kind = 'weekly')} disabled={readOnly}>Every week</button>
        </div>
      </fieldset>

      {#if kind === 'once'}
        <div class="two">
          <div>
            <label class="label" for="mw-start">Starts</label>
            <input id="mw-start" class="field" type="datetime-local" bind:value={startLocal} required disabled={readOnly} />
          </div>
          <div>
            <label class="label" for="mw-end">Ends</label>
            <input id="mw-end" class="field" type="datetime-local" bind:value={endLocal} required disabled={readOnly} />
          </div>
        </div>
        <p class="hint">In your browser's time zone ({browserTz}). From 5 minutes to 7 days.</p>
      {:else}
        <fieldset class="plain">
          <legend class="label">Days</legend>
          <div class="days">
            {#each weekOrder as d (d)}
              <label class="day" class:on={weekdays.includes(d)}>
                <input type="checkbox" checked={weekdays.includes(d)} onchange={() => toggleDay(d)} disabled={readOnly} />
                <span>{weekdayShort[d]}</span>
              </label>
            {/each}
          </div>
        </fieldset>
        <div class="three">
          <div>
            <label class="label" for="mw-time">Starts at</label>
            <input id="mw-time" class="field mono" type="time" bind:value={startTime} required disabled={readOnly} />
          </div>
          <div>
            <span class="label" id="mw-dur">Lasts</span>
            <div class="dur" role="group" aria-labelledby="mw-dur">
              <input class="field mono" type="number" min="0" max="168" bind:value={durHours} aria-label="Hours" disabled={readOnly} />
              <span class="meta">h</span>
              <input class="field mono" type="number" min="0" max="59" step="5" bind:value={durMins} aria-label="Minutes" disabled={readOnly} />
              <span class="meta">min</span>
            </div>
          </div>
          <div>
            <label class="label" for="mw-tz">Time zone</label>
            <input id="mw-tz" class="field" list="mw-zones" bind:value={timeZone} placeholder={hubTz || "Hub's time zone"}
              spellcheck="false" autocomplete="off" disabled={readOnly} />
            <datalist id="mw-zones">{#each zones as z (z)}<option value={z}></option>{/each}</datalist>
          </div>
        </div>
        <p class="hint">
          {fmtMinutes(duration)} each time. When a clock change skips the start time, the window starts at the next valid time.
        </p>
      {/if}
    </section>

    <section class="card card-pad grid">
      <fieldset class="plain">
        <legend class="label">What it covers</legend>
        <div class="choices">
          <label class="checkbox">
            <input type="radio" name="scope" checked={!everything} onchange={() => (everything = false)} disabled={readOnly} />
            <span>Selected stacks and checks</span>
          </label>
          <label class="checkbox">
            <input type="radio" name="scope" checked={everything} onchange={() => (everything = true)} disabled={readOnly} />
            <span>Everything. Also pauses host-offline alerts and the agents' hub watchdog.</span>
          </label>
        </div>
      </fieldset>

      {#if !everything}
        <div class="pickers">
          <div>
            <span class="label">Stacks <span class="opt">all checks of the stack</span></span>
            {#if !readOnly}
              <MultiSelect label="Stacks in scope" placeholder="Pick stacks" items={stackItems}
                selected={stacks.map(stackKey)} onchange={setStacks} />
            {/if}
            <ul class="chips" aria-label="Stacks in scope">
              {#each stacks as s (stackKey(s))}
                <li class="chip">
                  <span>{s.stack} <span class="meta">on {hostName(s.hostId)}</span></span>
                  {#if !readOnly}
                    <button type="button" class="btn btn-ghost btn-icon" aria-label={`Remove ${s.stack}`}
                      onclick={() => (stacks = stacks.filter((x) => stackKey(x) !== stackKey(s)))}><Icon name="x" /></button>
                  {/if}
                </li>
              {:else}
                <li class="meta">No stacks.</li>
              {/each}
            </ul>
          </div>
          <div>
            <span class="label">Checks</span>
            {#if !readOnly}
              <MultiSelect label="Checks in scope" placeholder="Pick checks" items={checkItems}
                selected={checkIds.map(String)} onchange={(v) => (checkIds = v.map(Number))} />
            {/if}
            <ul class="chips" aria-label="Checks in scope">
              {#each checkIds as cid (cid)}
                <li class="chip">
                  <span>{checks.find((c) => c.id === cid)?.name ?? `Check ${cid}`}</span>
                  {#if !readOnly}
                    <button type="button" class="btn btn-ghost btn-icon" aria-label="Remove check"
                      onclick={() => (checkIds = checkIds.filter((x) => x !== cid))}><Icon name="x" /></button>
                  {/if}
                </li>
              {:else}
                <li class="meta">No single checks.</li>
              {/each}
            </ul>
          </div>
        </div>
      {/if}

      <label class="checkbox">
        <input type="checkbox" bind:checked={enabled} disabled={readOnly} />
        <span>Enabled</span>
      </label>
    </section>

    {#if error}<p class="error-text" role="alert">{error}</p>{/if}

    {#if !readOnly}
      <section class="card card-pad buttons">
        {#if id}
          <button type="button" class="btn btn-ghost btn-danger" onclick={() => (confirmDelete = true)}><Icon name="trash" /> Delete</button>
        {/if}
        <span class="grow"></span>
        <a class="btn btn-ghost" href="#/maintenance">Cancel</a>
        <button class="btn btn-primary" disabled={saving || !title.trim()} aria-busy={saving}>{id ? 'Save' : 'Plan maintenance'}</button>
      </section>
    {/if}
  {/if}
</form>

<Confirm
  bind:open={confirmDelete}
  title="Delete {title || 'this window'}?"
  danger
  confirmLabel="Delete window"
  body="If the window is running, it ends now: failing checks in it alert as usual from their next result."
  onConfirm={del}
/>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 860px;
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
    overflow-wrap: anywhere;
  }
  .state {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
    margin-top: 8px;
  }
  .grid {
    display: grid;
    gap: 16px;
  }
  .two,
  .three {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }
  .three {
    grid-template-columns: minmax(0, 1fr) minmax(0, 1.2fr) minmax(0, 1.4fr);
  }
  .opt {
    font-weight: 400;
    color: var(--haze);
    margin-left: 4px;
  }
  .msg {
    font: 400 14px/1.5 var(--font);
  }
  .plain {
    border: 0;
    padding: 0;
    margin: 0;
    min-width: 0;
  }
  .seg {
    display: inline-flex;
    gap: 4px;
    padding: 3px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .seg button {
    height: 30px;
    padding: 0 14px;
    border: 0;
    border-radius: var(--r-sm);
    background: transparent;
    color: var(--haze);
    font: 500 13px var(--font);
    cursor: pointer;
  }
  .seg button.on {
    background: var(--rope-strong);
    color: var(--foam);
  }
  .days {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .day {
    position: relative;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 44px;
    height: 32px;
    padding: 0 8px;
    border-radius: var(--r-md);
    box-shadow: 0 0 0 1px var(--rope-stronger);
    color: var(--mist);
    font-size: 13px;
    cursor: pointer;
  }
  .day input {
    position: absolute;
    opacity: 0;
    inset: 0;
    margin: 0;
    cursor: pointer;
  }
  .day.on {
    background: var(--buoy-wash);
    color: var(--foam);
    box-shadow: 0 0 0 1px var(--buoy);
  }
  .day:focus-within {
    box-shadow:
      0 0 0 2px var(--buoy),
      0 0 0 5px var(--buoy-wash);
  }
  .dur {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .dur .field {
    width: 72px;
  }
  .choices {
    display: grid;
    gap: 10px;
  }
  .pickers {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 16px;
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .pickers :global(.ms) {
    margin-bottom: 8px;
  }
  .chips {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 6px;
  }
  .chip {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    min-height: 36px;
    padding: 2px 4px 2px 12px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    font-size: 13px;
    overflow-wrap: anywhere;
  }
  .buttons {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }

  @media (max-width: 767px) {
    .two,
    .three,
    .pickers {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
