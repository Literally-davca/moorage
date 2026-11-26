<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Channel, type CheckInput, type CheckKind, type CheckTestResult, type HostEntry, type Quorum, type StackSummary } from '../lib/api';
  import { app, stackLabel } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import MultiSelect from '../lib/MultiSelect.svelte';
  import Select from '../lib/Select.svelte';
  import StatusCodes from '../lib/StatusCodes.svelte';
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
  // HTTP: the URLs of the check (one or more); stack: "hostId/stack" values (one or more).
  let urls = $state<string[]>(['']);
  let stackSel = $state<string[]>([]);
  let allStacks = $state<{ value: string; label: string; meta: string }[]>([]);
  const MAX_TARGETS = 20;

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
      urls = form.settings.urls?.length ? [...form.settings.urls] : [form.kind === 'http' ? form.target : ''];
      stackSel = form.settings.stacks?.length
        ? form.settings.stacks.map((s) => `${s.hostId}/${s.stack}`)
        : form.kind === 'stack' && form.target
          ? [`${form.hostId}/${form.target}`]
          : [];
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

  // Stack checks can watch stacks of every host: list them all (offline hosts are skipped).
  $effect(() => {
    if (form.kind !== 'stack') return;
    const hosts = app.hosts.filter((h) => h.connected !== false);
    untrack(() => {
      Promise.all(
        hosts.map((h) =>
          api
            .stacksOf(h.id)
            .then((r) =>
              r.stacks.map((s) => {
                const disp = stackLabel(h.id, s.name, s.displayName);
                return {
                  value: `${h.id}/${s.name}`,
                  label: disp,
                  meta: disp !== s.name ? `${h.name} · ${s.name}` : h.name,
                };
              }),
            )
            .catch(() => []),
        ),
      ).then((lists) => (allStacks = lists.flat()));
    });
  });

  function stacksChanged(next: string[]) {
    const first = next[0];
    if (!first) {
      form.target = '';
      return;
    }
    const i = first.indexOf('/');
    form.hostId = first.slice(0, i);
    form.target = form.stack = first.slice(i + 1);
  }

  function setUrl(i: number, v: string) {
    urls[i] = v;
    form.target = urls.find((u) => u.trim()) ?? '';
  }
  function addUrl() {
    if (urls.length < MAX_TARGETS) urls = [...urls, ''];
  }
  function removeUrl(i: number) {
    urls = urls.filter((_, j) => j !== i);
    if (!urls.length) urls = [''];
    form.target = urls.find((u) => u.trim()) ?? '';
  }

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

  function locationsChanged(next: string[]) {
    if (next.length >= 2 && !form.quorum) form.quorum = 'majority';
  }

  const canSubmit = $derived.by(() => {
    if (form.kind === 'log') {
      return !!form.hostId && !!form.stack && !!form.settings.pattern?.trim() && !regexError;
    }
    if (isRemote && form.locations.length === 0) return false;
    if (form.kind === 'stack') return stackSel.length > 0 && stackSel.length <= MAX_TARGETS;
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
    // StatusCodes binds this field; it must never be undefined.
    form.settings.status ??= '';
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
    delete c.settings.urls;
    delete c.settings.stacks;
    if (c.kind === 'http') {
      const list = [...new Set(urls.map((u) => u.trim()).filter(Boolean))];
      c.target = list[0] ?? '';
      if (list.length > 1) c.settings.urls = list;
    }
    if (c.kind === 'stack') {
      const list = stackSel.map((v) => {
        const i = v.indexOf('/');
        return { hostId: v.slice(0, i), stack: v.slice(i + 1) };
      });
      if (list.length) {
        c.hostId = list[0].hostId;
        c.target = c.stack = list[0].stack;
      }
      if (list.length > 1) c.settings.stacks = list;
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
        <div>
          <span class="label">Stacks</span>
          <MultiSelect label="Stacks" placeholder="Pick one or more stacks" items={allStacks} bind:selected={stackSel} onchange={stacksChanged} />
          {#if stackSel.length > MAX_TARGETS}<p class="hint bad">One check can watch at most {MAX_TARGETS} stacks.</p>{/if}
        </div>
        <p class="hint">
          Down when a container exits, restarts in a loop or turns unhealthy. A stack you stopped on purpose doesn't alert.
          {#if stackSel.length > 1}With several stacks (on any hosts) the check is <b>Partially down</b> when some of them fail and Down when all do;
            a stack that is deploying or in maintenance sits the run out.{/if}
        </p>
      {:else if form.kind === 'log'}
        <div class="three">
          <div>
            <span class="label">Host</span>
            <Select
              label="Host"
              value={form.hostId}
              items={app.hosts.map((h) => ({ value: h.id, label: h.name }))}
              onchange={(v) => (form.hostId = v)}
            />
          </div>
          <div>
            <span class="label">Stack</span>
            <Select
              label="Stack"
              placeholder="Pick a stack"
              value={form.stack}
              items={stacks.map((s) => {
                const disp = stackLabel(form.hostId, s.name, s.displayName);
                return { value: s.name, label: disp, meta: disp !== s.name ? s.name : undefined };
              })}
              onchange={(v) => (form.stack = v)}
            />
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
      {:else if form.kind === 'http'}
        <div>
          <label class="label" for="target">{urls.length > 1 ? 'URLs' : 'URL'}</label>
          <div class="urls">
            {#each urls as u, i (i)}
              <div class="url-row">
                <input id={i === 0 ? 'target' : `url-${i}`} class="field mono" value={u} oninput={(e) => setUrl(i, e.currentTarget.value)}
                  placeholder={targetPlaceholder.http} spellcheck="false" autocomplete="off" required={i === 0} aria-label={`URL ${i + 1}`} />
                {#if urls.length > 1}
                  <button type="button" class="btn btn-ghost icon-only" aria-label={`Remove URL ${i + 1}`} onclick={() => removeUrl(i)}><Icon name="x" /></button>
                {/if}
              </div>
            {/each}
          </div>
          {#if urls.length < MAX_TARGETS}
            <button type="button" class="btn btn-ghost add-url" onclick={addUrl}><Icon name="plus" /> Add URL</button>
          {/if}
          {#if urls.length > 1}
            <p class="hint">All URLs share the method, status codes, text, timeout and locations, and are checked in parallel. Some failing makes the check <b>Partially down</b>, all failing makes it Down.</p>
          {/if}
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
            <StatusCodes id="codes" bind:value={form.settings.status} />
          </div>
        </div>
      {:else if form.kind === 'dns'}
        <div class="three">
          <div>
            <span class="label">Record</span>
            <Select
              label="DNS record type"
              value={form.settings.recordType ?? 'A'}
              items={['A', 'AAAA', 'CNAME', 'MX', 'TXT', 'NS'].map((t) => ({ value: t, label: t }))}
              onchange={(v) => (form.settings.recordType = v)}
            />
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
          <span class="label">Every</span>
          <Select
            label="Check interval"
            value={String(form.interval)}
            items={[
              { value: '20', label: '20 s' },
              { value: '30', label: '30 s' },
              { value: '60', label: '1 min' },
              { value: '120', label: '2 min' },
              { value: '300', label: '5 min' },
              { value: '900', label: '15 min' },
              { value: '3600', label: '1 h' },
            ]}
            onchange={(v) => (form.interval = Number(v))}
          />
        </div>
        <div>
          <span class="label">Timeout</span>
          <Select
            label="Timeout"
            value={String(form.timeout)}
            items={[5, 10, 15, 30].map((v) => ({
              value: String(v),
              label: `${v} s`,
              disabled: v >= form.interval ? 'Must be < interval' : undefined,
            }))}
            onchange={(v) => (form.timeout = Number(v))}
          />
        </div>
        <div>
          <span class="label">Alert after</span>
          <Select
            label="Failures before alert"
            value={String(form.retries)}
            items={[1, 2, 3, 5].map((v) => ({
              value: String(v),
              label: v === 1 ? 'the first failure' : `${v} failures in a row`,
            }))}
            onchange={(v) => (form.retries = Number(v))}
          />
        </div>
      </div>

      {#if isRemote}
        <fieldset class="locs">
          <legend class="label">Check from</legend>
          <MultiSelect label="Check from" placeholder="Pick locations" bind:selected={form.locations} onchange={locationsChanged}
            items={[{ value: 'hub', label: 'Hub', meta: 'this Moorage' }, ...agents.map((h) => ({ value: h.id, label: h.name, disabled: agentReason(h) || undefined }))]} />
          {#if form.locations.length === 0}<p class="hint bad">Pick at least one location.</p>{/if}
          {#if form.locations.length >= 2}
            <div class="quorum">
              <span class="label">Counts as down when</span>
              <Select
                label="Counts as down when"
                value={form.quorum}
                items={quorums.map((q) => ({ value: q, label: quorumText[q] }))}
                onchange={(v) => (form.quorum = v as Quorum)}
              />
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
            <Select
              label="Host"
              value={form.hostId}
              items={app.hosts.map((h) => ({ value: h.id, label: h.name }))}
              onchange={(v) => (form.hostId = v)}
            />
            <Select
              label="Stack"
              value={form.stack}
              items={[
                { value: '', label: 'No stack' },
                ...stacks.map((s) => {
                  const disp = stackLabel(form.hostId, s.name, s.displayName);
                  return { value: s.name, label: disp, meta: disp !== s.name ? s.name : undefined };
                }),
              ]}
              onchange={(v) => (form.stack = v)}
            />
          </div>
          <p class="hint">Linked checks stay quiet while the stack deploys, show deploys on their strip, and attach the stack's last log lines to alerts.</p>
        </div>
      {/if}

      <div>
        <span class="label">Send alerts to</span>
        {#if channels.length === 0}
          <p class="hint">No notification channels yet. <a href="#/notifications">Add one</a>, the check will still record incidents.</p>
        {:else}
          <MultiSelect label="Send alerts to" placeholder="No channel (incidents only)"
            items={channels.map((ch) => ({ value: String(ch.id), label: ch.name, meta: ch.kind + (ch.enabled ? '' : ' · off') }))}
            selected={form.channels.map(String)} onchange={(v) => (form.channels = v.map(Number))} />
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
                  <span class="label">Method</span>
                  <Select
                    label="HTTP method"
                    value={form.settings.method ?? ''}
                    items={[
                      { value: '', label: 'GET' },
                      { value: 'HEAD', label: 'HEAD' },
                    ]}
                    onchange={(v) => (form.settings.method = v as '' | 'HEAD')}
                  />
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
  .urls {
    display: grid;
    gap: 6px;
  }
  .url-row {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .url-row .field {
    flex: 1;
    min-width: 0;
  }
  .add-url {
    margin-top: 6px;
  }
  .link .two {
    margin-bottom: 0;
  }
  .locs {
    border: 0;
    padding: 0;
    margin: 0;
    display: grid;
    gap: 10px;
    min-width: 0;
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
