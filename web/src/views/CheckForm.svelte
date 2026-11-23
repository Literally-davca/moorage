<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Channel, type CheckInput, type CheckKind, type CheckTestResult, type HostEntry, type Quorum, type StackSummary } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import { emptyCheck, kindName, quorumText, remoteKinds, toInput, voteText } from '../lib/checks';

  let { id, host, stack }: { id: number | null; host?: string; stack?: string } = $props();

  let form = $state<CheckInput>(emptyCheck());
  let loaded = $state(false);
  let channels = $state<Channel[]>([]);
  let stacks = $state<StackSummary[]>([]);
  let stacksError = $state('');
  let saving = $state(false);
  let error = $state('');
  let testing = $state(false);
  let test = $state<CheckTestResult | null>(null);
  let advanced = $state(false);

  const kinds: { k: CheckKind; hint: string }[] = [
    { k: 'http', hint: 'A web page or API answers' },
    { k: 'tcp', hint: 'A port accepts connections' },
    { k: 'ping', hint: 'A host answers ping' },
    { k: 'dns', hint: 'A name resolves' },
    { k: 'stack', hint: 'Containers of a stack keep running' },
    { k: 'log', hint: 'A pattern appears in container logs' },
  ];

  $effect(() => {
    untrack(async () => {
      try {
        channels = await api.channels();
      } catch {
        channels = [];
      }
      if (id) {
        try {
          form = toInput(await api.check(id));
        } catch (e) {
          error = (e as Error).message;
        }
      } else {
        form = emptyCheck();
        form.channels = channels.filter((c) => c.enabled).map((c) => c.id);
        if (host && stack) {
          form.hostId = host;
          form.stack = stack;
          form.kind = 'stack';
          form.target = stack;
          form.name = stack;
        }
      }
      if (!form.hostId && app.hosts.length) form.hostId = app.host;
      // In the form the hub is an explicit location; clean() stores "hub only" as an empty list.
      if (!form.locations.length) form.locations = ['hub'];
      loaded = true;
    });
  });

  // stacks of the selected host, for the stack picker
  const hostKey = $derived(form.hostId);
  const watchKind = $derived(form.kind === 'stack' || form.kind === 'log');
  $effect(() => {
    const h = hostKey;
    const allStacks = watchKind;
    stacks = [];
    stacksError = '';
    if (!h) return;
    api
      .stacksOf(h)
      .then((r) => (stacks = r.stacks.filter((s) => s.managed || allStacks)))
      .catch((e) => (stacksError = (e as Error).message));
  });

  const linkStack = $derived(form.kind !== 'stack' && form.kind !== 'log');
  const targetLabel: Record<CheckKind, string> = { http: 'URL', tcp: 'Host and port', ping: 'Host', dns: 'Name', stack: 'Stack', log: 'Container' };
  const targetPlaceholder: Record<CheckKind, string> = {
    http: 'https://cloud.example.com/status',
    tcp: 'db.lan:5432',
    ping: '192.168.1.1',
    dns: 'example.com',
    stack: '',
    log: '',
  };

  const regexError = $derived.by(() => {
    if (form.kind !== 'log' || !form.settings.regex || !form.settings.pattern) return '';
    // The server uses Go's RE2 and has the final word. JavaScript only catches
    // plain syntax errors here, after removing RE2 inline flags like (?i) it doesn't know.
    try {
      new RegExp(form.settings.pattern.replace(/\(\?[imsU]+\)/g, ''));
      return '';
    } catch (e) {
      return (e as Error).message;
    }
  });

  const windowMin = $derived(Math.max(1, Math.round((form.settings.window ?? 300) / 60)));

  // ---- locations (multi-location checks) ----
  const isRemote = $derived(remoteKinds.includes(form.kind));
  const agents = $derived(app.hosts.filter((h) => h.kind === 'agent'));
  const quorums = Object.keys(quorumText) as Quorum[];

  /** Why an agent can't check from its network right now; empty when it can. */
  function agentReason(h: HostEntry): string {
    if (!h.connected) return 'offline';
    if (!h.probes) return 'checks turned off (MOORAGE_PROBES) or agent too old';
    return '';
  }

  function toggleLocation(loc: string, on: boolean) {
    form.locations = on ? [...form.locations.filter((l) => l !== loc), loc] : form.locations.filter((l) => l !== loc);
    if (form.locations.length >= 2 && !form.quorum) form.quorum = 'majority';
  }

  const canSubmit = $derived.by(() => {
    if (form.kind === 'log') {
      return !!form.hostId && !!form.stack && !!form.settings.pattern?.trim() && !regexError;
    }
    if (isRemote && form.locations.length === 0) return false;
    return !!form.target;
  });

  function setKind(k: CheckKind) {
    if (k === form.kind) return;
    form.kind = k;
    test = null;
    if (k === 'dns') {
      form.settings = { recordType: 'A' };
    } else if (k === 'log') {
      form.settings = { pattern: '', regex: false, caseSensitive: false, threshold: 1, window: 300 };
      form.retries = 1;
      // a name taken over from the stack check would hide which rule fired
      if (form.name === form.target || form.name === form.stack) form.name = '';
      form.target = '';
      if (!form.hostId && app.hosts.length) form.hostId = app.host;
    } else {
      form.settings = {};
    }
    if (k === 'stack') form.target = form.stack || '';
    else if (k !== 'log' && form.target === form.stack) form.target = '';
  }

  function clean(): CheckInput {
    const c = $state.snapshot(form) as CheckInput;
    if (c.kind === 'log') {
      c.locations = [];
      c.quorum = '';
      c.target = c.target.trim();
      c.settings.pattern = (c.settings.pattern ?? '').trim();
      c.settings.threshold = Math.max(1, Number(c.settings.threshold) || 1);
      c.settings.window = Math.max(10, Number(c.settings.window) || 300);
      c.name = c.name.trim() || `${c.stack}: ${c.settings.pattern}`.slice(0, 60);
      return c;
    }
    c.name = c.name.trim() || (c.kind === 'stack' ? c.target : c.target.replace(/^https?:\/\//, '').slice(0, 60));
    if (c.kind !== 'stack' && !c.stack) c.hostId = '';
    if (!remoteKinds.includes(c.kind) || (c.locations.length === 1 && c.locations[0] === 'hub')) c.locations = [];
    if (c.locations.length < 2) c.quorum = '';
    return c;
  }

  async function runTest() {
    testing = true;
    test = null;
    error = '';
    try {
      test = await api.testCheck(clean());
    } catch (e) {
      error = (e as Error).message;
    } finally {
      testing = false;
    }
  }

  async function save(e: SubmitEvent) {
    e.preventDefault();
    saving = true;
    error = '';
    try {
      const r = await api.saveCheck(id, clean());
      location.hash = '#/checks/' + r.id;
    } catch (err) {
      error = (err as Error).message;
    } finally {
      saving = false;
    }
  }

  function toggleChannel(chId: number, on: boolean) {
    form.channels = on ? [...form.channels, chId] : form.channels.filter((x) => x !== chId);
  }
</script>

<form class="page" onsubmit={save}>
  <section class="card card-pad head">
    <a class="back meta" href={id ? `#/checks/${id}` : '#/checks'}><Icon name="chevronLeft" /> {id ? 'Check' : 'Checks'}</a>
    <h1 class="title">{id ? 'Edit check' : 'Add check'}</h1>
  </section>

  {#if !loaded}
    <section class="card card-pad" aria-busy="true"><div class="skeleton" style="height:14px;width:40%"></div></section>
  {:else}
    <section class="card card-pad grid">
      <fieldset class="kinds">
        <legend class="label">What to check</legend>
        <div class="seg" role="radiogroup">
          {#each kinds as k (k.k)}
            <button type="button" role="radio" aria-checked={form.kind === k.k} class:on={form.kind === k.k} onclick={() => setKind(k.k)}>
              <span class="kname">{kindName[k.k]}</span>
              <span class="khint">{k.hint}</span>
            </button>
          {/each}
        </div>
      </fieldset>

      {#if form.kind === 'stack'}
        <div class="two">
          <div>
            <label class="label" for="host">Host</label>
            <select id="host" class="field" bind:value={form.hostId}>
              {#each app.hosts as h (h.id)}<option value={h.id}>{h.name}</option>{/each}
            </select>
          </div>
          <div>
            <label class="label" for="stack">Stack</label>
            <select id="stack" class="field" bind:value={form.target} onchange={() => (form.stack = form.target)}>
              <option value="" disabled>Pick a stack</option>
              {#each stacks as s (s.name)}<option value={s.name}>{s.name}</option>{/each}
            </select>
            {#if stacksError}<p class="hint bad">{stacksError}</p>{/if}
          </div>
        </div>
        <p class="hint">Down when a container exits, restarts in a loop or turns unhealthy. A stack you stopped on purpose doesn't alert.</p>
      {:else if form.kind === 'log'}
        <div class="three">
          <div>
            <label class="label" for="host">Host</label>
            <select id="host" class="field" bind:value={form.hostId}>
              {#each app.hosts as h (h.id)}<option value={h.id}>{h.name}</option>{/each}
            </select>
          </div>
          <div>
            <label class="label" for="stack">Stack</label>
            <select id="stack" class="field" bind:value={form.stack}>
              <option value="" disabled>Pick a stack</option>
              {#each stacks as s (s.name)}<option value={s.name}>{s.name}</option>{/each}
            </select>
            {#if stacksError}<p class="hint bad">{stacksError}</p>{/if}
          </div>
          <div>
            <label class="label" for="ctr">Container <span class="opt">optional</span></label>
            <input id="ctr" class="field mono" bind:value={form.target} placeholder="All containers" spellcheck="false" autocomplete="off" />
          </div>
        </div>
        <div>
          <label class="label" for="pat">Pattern</label>
          <input id="pat" class="field mono" bind:value={form.settings.pattern} maxlength="256" placeholder={form.settings.regex ? 'FATAL|panic|out of memory' : 'FATAL'} spellcheck="false" autocomplete="off" required />
          <div class="toggles">
            <label class="checkbox small"><input type="checkbox" bind:checked={form.settings.regex} /><span>Regular expression</span></label>
            <label class="checkbox small"><input type="checkbox" bind:checked={form.settings.caseSensitive} /><span>Case sensitive</span></label>
          </div>
          {#if regexError}<p class="hint bad" role="alert">Invalid regular expression: {regexError}</p>{/if}
        </div>
        <div>
          <span class="label">Condition</span>
          <div class="rule-row">
            <span>Alert when</span>
            <input type="number" class="field mono num-in" min="1" max="1000" bind:value={form.settings.threshold} aria-label="Matches" />
            <span>{(form.settings.threshold ?? 1) === 1 ? 'match' : 'matches'} within</span>
            <input
              type="number"
              class="field mono num-in"
              min="1"
              max="1440"
              value={windowMin}
              oninput={(e) => (form.settings.window = Math.max(1, Math.min(1440, Number(e.currentTarget.value) || 1)) * 60)}
              aria-label="Window in minutes"
            />
            <span>minutes</span>
          </div>
          <p class="hint">Recovers automatically once the window has no matches. A stack you stopped on purpose doesn't alert.</p>
        </div>
      {:else}
        <div>
          <label class="label" for="target">{targetLabel[form.kind]}</label>
          <input id="target" class="field mono" bind:value={form.target} placeholder={targetPlaceholder[form.kind]} spellcheck="false" autocomplete="off" required />
        </div>
      {/if}

      <div>
        <label class="label" for="name">Name</label>
        <input id="name" class="field" bind:value={form.name} maxlength="60" placeholder={form.kind === 'stack' ? form.target : form.kind === 'log' ? `${form.stack || 'app'}: ${form.settings.pattern || 'FATAL'}` : 'Nextcloud'} autocomplete="off" />
      </div>

      {#if form.kind === 'http'}
        <div class="two">
          <div>
            <label class="label" for="kw">Text that must appear <span class="opt">optional</span></label>
            <input id="kw" class="field" bind:value={form.settings.keyword} placeholder="Welcome" />
            {#if form.settings.keyword}
              <label class="checkbox small"><input type="checkbox" bind:checked={form.settings.keywordAbsent} /><span>Must not appear instead</span></label>
            {/if}
          </div>
          <div>
            <label class="label" for="codes">Accepted status codes</label>
            <input id="codes" class="field mono" bind:value={form.settings.status} placeholder="200-399" />
          </div>
        </div>
      {:else if form.kind === 'dns'}
        <div class="three">
          <div>
            <label class="label" for="rt">Record</label>
            <select id="rt" class="field" bind:value={form.settings.recordType}>
              {#each ['A', 'AAAA', 'CNAME', 'MX', 'TXT', 'NS'] as t (t)}<option value={t}>{t}</option>{/each}
            </select>
          </div>
          <div>
            <label class="label" for="exp">Answer contains <span class="opt">optional</span></label>
            <input id="exp" class="field mono" bind:value={form.settings.expect} placeholder="203.0.113.7" />
          </div>
          <div>
            <label class="label" for="res">DNS server <span class="opt">optional</span></label>
            <input id="res" class="field mono" bind:value={form.settings.resolver} placeholder="1.1.1.1" />
          </div>
        </div>
      {/if}

      <div class="three">
        <div>
          <label class="label" for="iv">Every</label>
          <select id="iv" class="field" bind:value={form.interval}>
            {#each [[20, '20 s'], [30, '30 s'], [60, '1 min'], [120, '2 min'], [300, '5 min'], [900, '15 min'], [3600, '1 h']] as [v, l] (v)}
              <option value={v}>{l}</option>
            {/each}
          </select>
        </div>
        <div>
          <label class="label" for="to">Timeout</label>
          <select id="to" class="field" bind:value={form.timeout}>
            {#each [5, 10, 15, 30] as v (v)}<option value={v} disabled={v >= form.interval}>{v} s</option>{/each}
          </select>
        </div>
        <div>
          <label class="label" for="rt2">Alert after</label>
          <select id="rt2" class="field" bind:value={form.retries}>
            {#each [1, 2, 3, 5] as v (v)}<option value={v}>{v === 1 ? 'the first failure' : `${v} failures in a row`}</option>{/each}
          </select>
        </div>
      </div>

      {#if isRemote}
        <fieldset class="locs">
          <legend class="label">Check from</legend>
          <div class="chans">
            <label class="checkbox">
              <input type="checkbox" checked={form.locations.includes('hub')} onchange={(e) => toggleLocation('hub', e.currentTarget.checked)} />
              <span>Hub <span class="meta">this Moorage</span></span>
            </label>
            {#each agents as h (h.id)}
              {@const reason = agentReason(h)}
              {@const on = form.locations.includes(h.id)}
              <label class="checkbox" class:off={!!reason}>
                <input type="checkbox" checked={on} disabled={!!reason && !on} onchange={(e) => toggleLocation(h.id, e.currentTarget.checked)} />
                <span>{h.name}{#if reason}<span class="meta"> · {reason}</span>{/if}</span>
              </label>
            {/each}
          </div>
          {#if form.locations.length === 0}<p class="hint bad">Pick at least one location.</p>{/if}
          {#if form.locations.length >= 2}
            <div class="quorum">
              <label class="label" for="quorum">Counts as down when</label>
              <select id="quorum" class="field" bind:value={form.quorum}>
                {#each quorums as q (q)}<option value={q}>{quorumText[q]}</option>{/each}
              </select>
            </div>
          {/if}
          <p class="hint">
            Agents run the check from their own network, so a blip in one network isn't a false alarm. Offline agents and locations that don't
            answer don't vote; when none answers, the check shows unknown and doesn't alert. Agents cap the timeout at 30 s.
          </p>
        </fieldset>
      {/if}

      {#if linkStack}
        <div class="link">
          <span class="label">Belongs to a stack <span class="opt">optional</span></span>
          <div class="two">
            <select class="field" bind:value={form.hostId} aria-label="Host">
              {#each app.hosts as h (h.id)}<option value={h.id}>{h.name}</option>{/each}
            </select>
            <select class="field" bind:value={form.stack} aria-label="Stack">
              <option value="">No stack</option>
              {#each stacks as s (s.name)}<option value={s.name}>{s.name}</option>{/each}
            </select>
          </div>
          <p class="hint">Linked checks stay quiet while the stack deploys, show deploys on their strip, and attach the stack's last log lines to alerts.</p>
        </div>
      {/if}

      <div>
        <span class="label">Send alerts to</span>
        {#if channels.length === 0}
          <p class="hint">No notification channels yet. <a href="#/notifications">Add one</a>, the check will still record incidents.</p>
        {:else}
          <div class="chans">
            {#each channels as ch (ch.id)}
              <label class="checkbox">
                <input type="checkbox" checked={form.channels.includes(ch.id)} onchange={(e) => toggleChannel(ch.id, e.currentTarget.checked)} />
                <span>{ch.name} <span class="meta">{ch.kind}{ch.enabled ? '' : ' · off'}</span></span>
              </label>
            {/each}
          </div>
        {/if}
      </div>

      {#if form.kind !== 'stack' && form.kind !== 'log'}
        <button type="button" class="adv-toggle meta" onclick={() => (advanced = !advanced)} aria-expanded={advanced}>
          {advanced ? 'Hide' : 'Show'} advanced options
        </button>
        {#if advanced}
          <div class="adv">
            {#if form.kind === 'http'}
              <div class="two">
                <div>
                  <label class="label" for="jp">JSON path <span class="opt">optional</span></label>
                  <input id="jp" class="field mono" bind:value={form.settings.jsonPath} placeholder="status or data.items[0].ok" />
                </div>
                <div>
                  <label class="label" for="jv">Expected value</label>
                  <input id="jv" class="field mono" bind:value={form.settings.jsonValue} placeholder="ok" />
                </div>
              </div>
              <div class="two">
                <div>
                  <label class="label" for="mth">Method</label>
                  <select id="mth" class="field" bind:value={form.settings.method}>
                    <option value="">GET</option>
                    <option value="HEAD">HEAD</option>
                  </select>
                </div>
                <label class="checkbox top"><input type="checkbox" bind:checked={form.settings.ignoreTls} /><span>Accept invalid TLS certificates (self-signed)</span></label>
              </div>
            {/if}
            <label class="checkbox">
              <input type="checkbox" bind:checked={form.settings.allowLinkLocal} />
              <span>Allow link-local addresses (169.254.x.x, fe80::). Off by default because cloud metadata services live there.</span>
            </label>
          </div>
        {/if}
      {/if}

      {#if test}
        <div class="test-box">
          <div class="test {test.ok ? 'ok' : test.paused || test.unknown ? 'paused' : 'bad'}" role="status">
            <Icon name={test.ok ? 'check' : 'alert'} />
            <span>{test.ok ? 'Works' : test.paused ? 'Paused' : test.unknown ? 'Unknown' : 'Fails'}: {test.msg} · {test.latencyMs} ms</span>
          </div>
          {#if test.locations?.length}
            <ul class="test-locs">
              {#each test.locations as l (l.location)}
                <li><span class="loc-name">{l.name}</span> <span class:bad={l.vote === 'down'}>{voteText(l)}</span> <span class="meta">{l.msg}{l.vote !== 'none' ? ` · ${l.latencyMs} ms` : ''}</span></li>
              {/each}
            </ul>
          {/if}
          {#if test.matches?.length}
            <pre class="test-logs">{#each test.matches as l, i (i)}<span class:err={l.s === 'stderr'}>{l.c} | {l.m}</span>{'\n'}{/each}</pre>
          {/if}
        </div>
      {/if}
      {#if error}<p class="error-text" role="alert">{error}</p>{/if}

      <div class="buttons">
        <button type="button" class="btn" onclick={runTest} disabled={testing || !canSubmit} aria-busy={testing}>Test now</button>
        <span class="grow"></span>
        <a class="btn btn-ghost" href={id ? `#/checks/${id}` : '#/checks'}>Cancel</a>
        <button class="btn btn-primary" disabled={saving || !canSubmit} aria-busy={saving}>{id ? 'Save' : 'Add check'}</button>
      </div>
    </section>
  {/if}
</form>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 820px;
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
    gap: 18px;
  }
  .kinds {
    border: 0;
    padding: 0;
    margin: 0;
  }
  .seg {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 6px;
  }
  .seg button {
    display: grid;
    gap: 2px;
    text-align: left;
    padding: 10px 12px;
    border: 0;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    color: var(--mist);
    font: inherit;
    cursor: pointer;
  }
  .seg button:hover {
    box-shadow: inset 0 0 0 1px var(--rope-stronger);
  }
  .seg button.on {
    background: var(--buoy-wash);
    box-shadow: inset 0 0 0 1px var(--buoy);
    color: var(--foam);
  }
  .seg button:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: 2px;
  }
  .kname {
    font-weight: 600;
    font-size: 13px;
  }
  .khint {
    font-size: 11px;
    color: var(--haze);
    line-height: 1.3;
  }
  .two {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }
  .three {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 12px;
  }
  .opt {
    font-weight: 400;
    color: var(--haze);
    margin-left: 4px;
  }
  .small {
    margin-top: 8px;
    font-size: 12px;
  }
  .toggles {
    display: flex;
    gap: 16px;
    flex-wrap: wrap;
  }
  .rule-row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    font-size: 13px;
    color: var(--mist);
  }
  .num-in {
    width: 80px;
  }
  .test-box {
    display: grid;
    gap: 8px;
  }
  .test-logs {
    margin: 0;
    padding: 10px 12px;
    background: var(--well);
    border-radius: var(--r-md);
    font: 12px/1.7 var(--mono);
    color: var(--mist);
    overflow-x: auto;
    white-space: pre;
  }
  .test-logs .err {
    color: var(--adrift-text);
  }
  .top {
    align-self: end;
    padding-bottom: 8px;
  }
  .link .two {
    margin-bottom: 0;
  }
  .chans {
    display: flex;
    flex-wrap: wrap;
    gap: 10px 20px;
  }
  .locs {
    border: 0;
    padding: 0;
    margin: 0;
    display: grid;
    gap: 10px;
    min-width: 0;
  }
  .locs .off {
    color: var(--haze);
  }
  .quorum {
    max-width: 280px;
  }
  .test-locs {
    margin: 0;
    padding: 0;
    list-style: none;
    display: grid;
    gap: 4px;
    font-size: 12px;
    overflow-wrap: anywhere;
  }
  .loc-name {
    font-weight: 600;
  }
  .adv-toggle {
    justify-self: start;
    border: 0;
    background: none;
    padding: 0;
    font-size: 12px;
    cursor: pointer;
    text-decoration: underline;
    text-underline-offset: 3px;
  }
  .adv {
    display: grid;
    gap: 14px;
    padding: 14px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .test {
    display: flex;
    gap: 10px;
    align-items: center;
    padding: 10px 14px;
    border-radius: var(--r-md);
    font-size: 13px;
  }
  .test.ok {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .test.bad {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .test.paused {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .bad {
    color: var(--adrift-text);
  }
  .buttons {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .grow {
    flex: 1;
  }

  @media (max-width: 767px) {
    .seg {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .two,
    .three {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
