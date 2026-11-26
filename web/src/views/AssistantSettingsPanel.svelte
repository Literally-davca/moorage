<script lang="ts">
  import { api, type AssistantSettings } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';

  let s = $state<AssistantSettings | null>(null);
  let apiKey = $state('');
  let error = $state('');
  let saved = $state(false);
  let busy = $state(false);

  const providers = [
    { value: 'anthropic', label: 'Anthropic (Claude)' },
    { value: 'openai', label: 'OpenAI' },
    { value: 'gemini', label: 'Google Gemini' },
    { value: 'ollama', label: 'Ollama (local)' },
    { value: 'openai-compatible', label: 'OpenAI-compatible endpoint (OpenRouter, Groq, LM Studio…)' },
  ];
  const defaults: Record<string, string> = {
    anthropic: 'claude-opus-5-5',
    openai: 'gpt-5',
    gemini: 'gemini-2.5-pro',
    ollama: 'llama3.1',
    'openai-compatible': '',
  };
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

  async function load() {
    try {
      s = await api.assistantSettings();
    } catch (e) {
      error = (e as Error).message;
    }
  }
  load();

  async function save(clearKey = false) {
    if (!s) return;
    busy = true;
    error = '';
    saved = false;
    try {
      s = await api.saveAssistantSettings({ ...s, apiKey: apiKey || undefined, clearKey });
      apiKey = '';
      saved = true;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  function setProvider(v: string) {
    if (!s) return;
    if (!s.model || s.model === defaults[s.provider]) s.model = defaults[v] ?? '';
    s.provider = v as AssistantSettings['provider'];
  }
</script>

<div class="apanel">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
  {#if s}
    <div class="grid">
      <div class="sel-wrap"><Select label="Provider" value={s.provider} items={providers} onchange={setProvider} /></div>
      <input class="field mono" bind:value={s.model} placeholder="Model" aria-label="Model" />
      {#if s.provider !== 'anthropic'}
        <input class="field mono" bind:value={s.baseUrl} placeholder={s.provider === 'openai-compatible' ? 'https://openrouter.ai/api/v1' : 'Endpoint (default for the provider)'} aria-label="Endpoint URL" />
      {/if}
      <input
        class="field mono"
        type="password"
        bind:value={apiKey}
        autocomplete="new-password"
        placeholder={s.apiKeySet ? 'API key saved – leave empty to keep' : s.provider === 'ollama' ? 'API key (not needed for Ollama)' : 'API key'}
        aria-label="API key" />
    </div>
    <p class="meta">The key is stored like other Moorage secrets and never sent back to the browser. Nothing goes to the provider until someone sends a message. Models without function calling can only advise.</p>

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
    <p class="meta">Never possible: deleting stacks, volumes or snapshots, down, terminals, users, roles, tokens, sign-in, self-update, restores, disk cleanup, reading or changing .env values.</p>

    <h4>What the assistant may read</h4>
    <div class="row">
      <span>Container logs (masked, at most 200 lines)</span>
      <div class="sel-wrap"><Select label="Container logs" value={s.data.logs} items={dataLevels} onchange={(v) => (s!.data.logs = v as 'ask')} /></div>
    </div>
    <p class="meta">Stack state, Problems, compose.yaml (with .env values hidden) and host addresses are always available to it.</p>

    <h4>Usage</h4>
    <div class="row">
      <span>This month: {s.usageTokens.toLocaleString()} tokens</span>
      <label class="num">
        <span class="meta">Monthly token limit (0 = none)</span>
        <input class="field" type="number" min="0" bind:value={s.monthlyTokenLimit} />
      </label>
    </div>

    <div class="acts">
      <button type="button" class="btn btn-primary" onclick={() => save()} disabled={busy}>Save</button>
      {#if s.apiKeySet}<button type="button" class="btn btn-ghost" onclick={() => save(true)} disabled={busy}>Remove key</button>{/if}
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
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
    gap: 8px;
  }
  .row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .warn {
    color: var(--degraded-text, var(--warning));
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
