<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type SharedVar } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';

  let vars = $state<SharedVar[] | null>(null);
  let error = $state('');
  let saving = $state(false);
  let savedMsg = $state('');

  let key = $state('');
  let value = $state('');
  let hostId = $state('');
  let secret = $state(false);

  const presets = ['TZ', 'ACME_EMAIL', 'PUID', 'PGID', 'DOMAIN'];

  async function load() {
    try {
      const r = await api.sharedVars();
      vars = r.vars;
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => void load());
  });

  function pickPreset(k: string) {
    key = k;
    secret = false;
    if (k === 'TZ' && !value) value = Intl.DateTimeFormat().resolvedOptions().timeZone || 'Europe/Prague';
    if (k === 'PUID' && !value) value = '1000';
    if (k === 'PGID' && !value) value = '1000';
  }

  async function addVar(e: SubmitEvent) {
    e.preventDefault();
    saving = true;
    error = '';
    savedMsg = '';
    try {
      await api.saveSharedVar({ key: key.trim(), value, hostId, secret });
      key = '';
      value = '';
      secret = false;
      savedMsg = 'Saved';
      setTimeout(() => (savedMsg = ''), 2000);
      await load();
    } catch (err) {
      error = (err as Error).message;
    } finally {
      saving = false;
    }
  }

  async function removeVar(v: SharedVar) {
    error = '';
    try {
      await api.deleteSharedVar(v.id);
      await load();
    } catch (err) {
      error = (err as Error).message;
    }
  }

  function hostNameOf(id: string): string {
    if (!id) return 'Global (all hosts)';
    return app.hosts.find((h) => h.id === id)?.name ?? id;
  }
</script>

<div class="page">
  <section class="card card-pad">
    <h1 class="title">Shared variables</h1>
    <p class="meta">
      Reusable defaults (<code>TZ</code>, <code>ACME_EMAIL</code>, <code>PUID</code>, <code>PGID</code>, custom keys) automatically prefill matching
      template fields and can be inserted into any stack's <code>.env</code> in one click. Secret values are write-only and encrypted in backups.
    </p>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  <form class="card card-pad add-form" onsubmit={addVar}>
    <div class="head-row">
      <h2 class="section-title">Add or update variable</h2>
      <div class="presets">
        <span class="meta">Quick fill:</span>
        {#each presets as p (p)}
          <button type="button" class="chip mono" onclick={() => pickPreset(p)}>{p}</button>
        {/each}
      </div>
    </div>

    <div class="grid-4">
      <label class="field-wrap">
        <span>Name</span>
        <input class="field mono" bind:value={key} placeholder="TZ" required spellcheck="false" autocomplete="off" />
      </label>

      <label class="field-wrap">
        <span>Value</span>
        <input class="field mono" type={secret ? 'password' : 'text'} bind:value={value} placeholder={secret ? '********' : 'Europe/Prague'} spellcheck="false" autocomplete="off" />
      </label>

      <div class="field-wrap">
        <span>Scope</span>
        <Select
          label="Shared variable scope"
          value={hostId}
          items={[{ value: '', label: 'Global (all hosts)' }, ...app.hosts.map((h) => ({ value: h.id, label: h.name }))]}
          onchange={(v) => (hostId = v)}
        />
      </div>

      <label class="check-row" style="align-self:end;height:34px">
        <input type="checkbox" bind:checked={secret} />
        <span>Secret (write-only)</span>
      </label>
    </div>

    <div class="actions">
      <button type="submit" class="btn btn-primary" disabled={saving || !key.trim()}><Icon name="plus" /> {saving ? 'Saving…' : 'Save variable'}</button>
      {#if savedMsg}<span class="meta ok"><Icon name="check" /> {savedMsg}</span>{/if}
    </div>
  </form>

  <section class="card card-pad">
    <h2 class="section-title" style="margin-bottom:10px">Configured variables</h2>
    {#if vars === null}
      <div class="skeleton" style="height:14px;width:45%"></div>
    {:else if vars.length === 0}
      <p class="meta">No shared variables configured yet.</p>
    {:else}
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Key</th>
              <th>Value</th>
              <th>Scope</th>
              <th class="r"></th>
            </tr>
          </thead>
          <tbody>
            {#each vars as v (`${v.hostId}:${v.key}`)}
              <tr>
                <td class="mono"><b>{v.key}</b></td>
                <td class="mono">
                  {#if v.secret}
                    <span class="meta">********</span> <span class="pill">Secret</span>
                  {:else}
                    {v.value || '–'}
                  {/if}
                  {#if v.fromEnv}<span class="pill env">Set via environment</span>{/if}
                </td>
                <td>{hostNameOf(v.hostId)}</td>
                <td class="r">
                  {#if !v.fromEnv}
                    <button type="button" class="btn btn-sm btn-ghost danger" onclick={() => removeVar(v)}><Icon name="trash" /> Remove</button>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .add-form {
    display: grid;
    gap: 12px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .presets {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .chip {
    height: 24px;
    padding: 0 8px;
    border: 0;
    border-radius: 6px;
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope-strong);
    color: var(--foam);
    font-size: 12px;
    cursor: pointer;
  }
  .grid-4 {
    display: grid;
    grid-template-columns: minmax(150px, 1fr) minmax(200px, 1.5fr) minmax(170px, 1fr) auto;
    gap: 12px;
  }
  .field-wrap {
    display: grid;
    gap: 5px;
    font-size: 13px;
    color: var(--mist);
  }
  .check-row {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    color: var(--foam);
    cursor: pointer;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .ok {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    color: var(--afloat-text);
  }
  .danger {
    color: var(--adrift-text);
  }
  .pill {
    display: inline-flex;
    align-items: center;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
    margin-left: 6px;
  }
  .pill.env {
    background: var(--buoy-wash);
    color: var(--buoy-text);
  }
  @media (max-width: 820px) {
    .grid-4 {
      grid-template-columns: 1fr 1fr;
    }
  }
  @media (max-width: 520px) {
    .grid-4 {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
