<script lang="ts">
  import { api, type AssistantModelInfo, type AssistantProvider, type AssistantProviderType, type AssistantSettings } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';
  import ModelList from '../lib/ModelList.svelte';

  // Assistant settings (M18G, M19E): several providers with their own keys, models found
  // automatically (Ollama and OpenAI-compatible endpoints) or ticked (paid APIs), the
  // default pair, a connection test, the action and data levels and thinking (M19I).

  let s = $state<AssistantSettings | null>(null);
  let keys = $state<Record<string, string>>({});
  let clearKeys = $state<string[]>([]);
  let error = $state('');
  let saved = $state(false);
  let busy = $state(false);
  let found = $state<Record<string, { models: AssistantModelInfo[]; error?: string; at?: string; loading?: boolean }>>({});
  let tests = $state<Record<string, { ok: boolean; text: string } | 'running'>>({});

  const types = [
    { value: 'anthropic', label: 'Anthropic (Claude)' },
    { value: 'openai', label: 'OpenAI' },
    { value: 'gemini', label: 'Google Gemini' },
    { value: 'ollama', label: 'Ollama' },
    { value: 'openai-compatible', label: 'OpenAI-compatible (OpenRouter, Groq, LM Studio…)' },
  ];
  const paid = (t: string) => t === 'anthropic' || t === 'openai' || t === 'gemini';
  /** Paid APIs offer only ticked models; OpenAI-compatible when switched to Only ticked. */
  const ticksOnly = (p: AssistantProvider) => paid(p.type) || (p.type === 'openai-compatible' && !!p.onlyTicked);
  const offerItems = [
    { value: 'all', label: 'Offer all models' },
    { value: 'ticked', label: 'Only ticked' },
  ];
  const catLabel: Record<string, string> = {
    stack: 'Start / restart / stop / pull stacks',
    compose: 'Compose edits & deploy',
    generate: 'Create new stacks from a generated compose.yaml',
  };
  const levels = [
    { value: 'off', label: 'Off' },
    { value: 'advise', label: 'Advise (explain + link)' },
    { value: 'propose', label: 'Propose (Apply in chat)' },
    { value: 'auto', label: 'Auto (does it)' },
  ];
  const dataLevels = [
    { value: 'never', label: 'Never' },
    { value: 'ask', label: 'Ask in chat' },
    { value: 'allowed', label: 'Allowed' },
  ];

  // type and endpoint as saved: Load models and Test send the form values only when they
  // differ, so the server cache (5 min) serves the common case (M20H)
  let savedProv: Record<string, string> = {};
  const provSig = (p: AssistantProvider) => p.type + '|' + (p.baseUrl ?? '').trim();
  function remember() {
    savedProv = Object.fromEntries((s?.providers ?? []).map((p) => [p.id, provSig(p)]));
  }
  function unsaved(p: AssistantProvider) {
    const draft = savedProv[p.id] === provSig(p) ? undefined : { type: p.type, baseUrl: p.baseUrl };
    return { draft, key: keys[p.id]?.trim() || undefined };
  }

  async function load() {
    try {
      s = await api.assistantSettings();
      remember();
      for (const p of s.providers) void detect(p, false);
    } catch (e) {
      error = (e as Error).message;
    }
  }
  load();

  async function detect(p: AssistantProvider, fresh: boolean) {
    found[p.id] = { ...(found[p.id] ?? { models: [] }), loading: true };
    try {
      const u = unsaved(p);
      const r = await api.assistantModels(p.id, fresh, u.draft, u.key);
      found[p.id] = { models: r.models, error: r.error, at: r.at };
    } catch (e) {
      found[p.id] = { models: found[p.id]?.models ?? [], error: (e as Error).message };
    }
  }

  async function save() {
    if (!s) return;
    busy = true;
    error = '';
    saved = false;
    try {
      const k: Record<string, string> = {};
      for (const [id, v] of Object.entries(keys)) if (v && v.trim()) k[id] = v.trim();
      s = await api.saveAssistantSettings({ ...s, keys: k, clearKeys });
      keys = {};
      clearKeys = [];
      remember();
      saved = true;
      window.dispatchEvent(new Event('moorage-assistant-changed'));
      for (const p of s.providers) void detect(p, true);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  function addProvider() {
    if (!s) return;
    let n = s.providers.length + 1;
    while (s.providers.some((p) => p.id === 'p' + n)) n++;
    s.providers.push({ id: 'p' + n, name: 'Ollama ' + n, type: 'ollama', baseUrl: '', models: [] });
  }
  function removeProvider(id: string) {
    if (!s || s.default.provider === id) return;
    s.providers = s.providers.filter((p) => p.id !== id);
    clearKeys = [...clearKeys, id];
  }
  function toggleModel(p: AssistantProvider, m: string) {
    const list = p.models ?? [];
    p.models = list.includes(m) ? list.filter((x) => x !== m) : [...list, m];
  }
  async function test(p: AssistantProvider) {
    if (!s) return;
    tests[p.id] = 'running';
    const model = s.default.provider === p.id ? s.default.model : (p.models?.[0] ?? found[p.id]?.models[0]?.id ?? '');
    try {
      const u = unsaved(p);
      const r = await api.assistantTest(p.id, model, u.draft, u.key);
      tests[p.id] = r.ok ? { ok: true, text: `Works (${model}).` } : { ok: false, text: r.error ?? 'Failed.' };
    } catch (e) {
      tests[p.id] = { ok: false, text: (e as Error).message };
    }
  }

  // default model: the found / ticked models of the default provider, or a custom name
  const defProvider = $derived(s?.providers.find((p) => p.id === s?.default.provider));
  const defModels = $derived.by(() => {
    if (!defProvider) return [];
    const list = ticksOnly(defProvider) ? (defProvider.models ?? []) : (found[defProvider.id]?.models ?? []).map((m) => m.id);
    return list.map((m) => ({ value: m, label: m }));
  });
  const newPaid = (p: AssistantProvider) => (paid(p.type) ? (found[p.id]?.models ?? []).filter((m) => !(p.models ?? []).includes(m.id)).length : 0);
  function testResult(id: string) {
    const t = tests[id];
    return t && t !== 'running' ? t : null;
  }
</script>

<div class="apanel">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
  {#if s}
    <h4>Providers</h4>
    {#each s.providers as p (p.id)}
      <div class="prov">
        <div class="grid">
          <input class="field" bind:value={p.name} placeholder="Name" aria-label="Provider name" />
          <div class="sel-wrap"><Select label="Type" value={p.type} items={types} onchange={(v) => (p.type = v as AssistantProviderType)} /></div>
          {#if p.type !== 'anthropic'}
            <input class="field mono" bind:value={p.baseUrl}
              placeholder={p.type === 'openai-compatible' ? 'https://openrouter.ai/api/v1' : p.type === 'ollama' ? 'http://192.168.1.20:11434/v1' : 'Endpoint (default)'}
              aria-label="Endpoint URL" />
          {/if}
          <input class="field mono" type="password" bind:value={keys[p.id]} autocomplete="new-password"
            placeholder={s.keysSet?.[p.id] ? 'API key saved – leave empty to keep' : p.type === 'ollama' ? 'API key (not needed for Ollama)' : 'API key'}
            aria-label="API key of {p.name}" />
        </div>
        {#if p.type === 'ollama' && /localhost|127\.0\.0\.1/.test(p.baseUrl ?? '')}
          <p class="meta warn">In a container, localhost is Moorage itself: use the host's address or host.docker.internal.</p>
        {/if}
        <div class="models">
          {#if found[p.id]?.loading}
            <span class="meta">Loading models…</span>
          {:else if found[p.id]?.error}
            <span class="meta warn">Couldn't load models: {found[p.id].error}</span>
          {/if}
          {#if p.type === 'openai-compatible'}
            <div class="sel-wrap">
              <Select label="Models offered" value={p.onlyTicked ? 'ticked' : 'all'} items={offerItems} onchange={(v) => (p.onlyTicked = v === 'ticked')} />
            </div>
          {/if}
          {#if paid(p.type)}
            <span class="meta">Tick the models people can use (they differ in price){newPaid(p) ? ` · ${newPaid(p)} new models` : ''}.</span>
          {:else if !ticksOnly(p)}
            <span class="meta">Every model found is offered.</span>
          {/if}
          {#if found[p.id]?.models.length}
            <ModelList models={found[p.id].models} fallbackGroup={types.find((t) => t.value === p.type)?.label ?? p.type}
              ticked={p.models ?? []} tickable={ticksOnly(p)} ontoggle={(id) => toggleModel(p, id)} />
          {/if}
        </div>
        <div class="acts">
          <button type="button" class="btn btn-sm" onclick={() => detect(p, true)}><Icon name="refresh" /> Load models</button>
          <button type="button" class="btn btn-sm" onclick={() => test(p)} disabled={tests[p.id] === 'running'}>{tests[p.id] === 'running' ? 'Testing…' : 'Test connection'}</button>
          {#if s.keysSet?.[p.id]}<button type="button" class="btn btn-sm btn-ghost" onclick={() => (clearKeys = [...clearKeys, p.id])}>Remove key</button>{/if}
          {#if s.providers.length > 1}
            <button type="button" class="btn btn-sm btn-ghost" onclick={() => removeProvider(p.id)} disabled={s.default.provider === p.id}
              aria-label="Remove {p.name}" title={s.default.provider === p.id ? 'Choose another default first' : 'Remove'}><Icon name="trash" /></button>
          {/if}
          {#if testResult(p.id)}
            <span class={testResult(p.id)?.ok ? 'ok' : 'error-text'}>{testResult(p.id)?.text}</span>
          {/if}
        </div>
      </div>
    {/each}
    <div class="acts">
      <button type="button" class="btn btn-sm" onclick={addProvider} disabled={s.providers.length >= 10}><Icon name="plus" /> Add provider</button>
      <span class="meta">Keys are stored like other Moorage secrets and never sent back to the browser. New models and keys apply after Save.</span>
    </div>

    <h4>Default model</h4>
    <div class="grid">
      <div class="sel-wrap">
        <Select label="Default provider" value={s.default.provider} items={s.providers.map((p) => ({ value: p.id, label: p.name }))} onchange={(v) => (s!.default.provider = v)} />
      </div>
      <div class="sel-wrap">
        <Select label="Default model" value={s.default.model} items={defModels} allowCustom customPlaceholder="Model name" onchange={(v) => (s!.default.model = v)} />
      </div>
    </div>
    <p class="meta">Admins can switch between the allowed models in the assistant panel; others use the default. The chat never switches provider by itself.</p>

    <label class="check"><input type="checkbox" checked={s.showThinking !== false} onchange={(e) => (s!.showThinking = (e.target as HTMLInputElement).checked)} /> Show model thinking (collapsed and never stored; off saves tokens where the provider must be asked for it)</label>

    <h4>What the assistant may do</h4>
    {#each s.categories ?? Object.keys(catLabel) as c (c)}
      <div class="row">
        <span>{catLabel[c] ?? c}</span>
        <div class="sel-wrap"><Select label={catLabel[c] ?? c} value={s.levels[c] ?? 'propose'} items={levels} onchange={(v) => (s!.levels[c] = v as 'off')} /></div>
      </div>
      {#if s.levels[c] === 'auto'}
        <p class="warn">Auto runs changes without asking. When the assistant read logs or other outside content in the same answer, it only proposes. Every action is in the audit log.</p>
        {#if c === 'compose'}
          <label class="check"><input type="checkbox" bind:checked={s.autoConfirmed[c]} /> I understand the assistant can change and deploy compose files on its own.</label>
        {/if}
      {/if}
    {/each}
    <p class="meta">Never possible: deleting stacks, volumes or snapshots, down, terminals, users, roles, tokens, sign-in, updating Moorage, restores, disk cleanup, reading or changing .env values. A change of Moorage's own compose file is only proposed (never Auto) and never changes its image.</p>

    <h4>What the assistant may read</h4>
    <div class="row">
      <span>Container logs (masked, at most 200 lines; also log lines in Problems)</span>
      <div class="sel-wrap"><Select label="Container logs" value={s.data.logs} items={dataLevels} onchange={(v) => (s!.data.logs = v as 'ask')} /></div>
    </div>
    <p class="meta">Stack state, Problems, compose.yaml (with .env values hidden), host addresses, service links, the port map and the notices on the user's screen are always available to it.</p>

    <h4>Usage</h4>
    <div class="row">
      <span>This month: {s.usageTokens.toLocaleString()} tokens</span>
      <label class="num">
        <span class="meta">Monthly token limit (0 = none)</span>
        <input class="field" type="number" min="0" bind:value={s.monthlyTokenLimit} />
      </label>
    </div>

    <div class="acts">
      <button type="button" class="btn btn-primary" onclick={save} disabled={busy}>Save</button>
      {#if saved}<span class="ok"><Icon name="check" /> Saved. Open the assistant with Ctrl+J.</span>{/if}
    </div>
  {/if}
</div>

<style>
  .apanel {
    display: grid;
    gap: 10px;
    padding: 10px 0;
  }
  .apanel h4 {
    margin: 8px 0 0;
  }
  .prov {
    display: grid;
    gap: 8px;
    padding: 10px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
    gap: 8px;
  }
  .models {
    display: grid;
    gap: 4px;
  }
  .row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .warn {
    color: var(--signal-text);
    margin: 0;
  }
  .check {
    display: inline-flex;
    gap: 6px;
    align-items: center;
  }
  .num {
    display: grid;
    gap: 4px;
  }
  .acts {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .ok {
    display: inline-flex;
    gap: 6px;
    align-items: center;
    color: var(--afloat-text);
  }
</style>
