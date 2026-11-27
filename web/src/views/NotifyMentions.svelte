<script lang="ts">
  import type { ChannelConfig, MentionRule } from '../lib/api';
  import Icon from '../lib/Icon.svelte';

  // Mentions on Discord and Slack (M19L): who to ping, on which events, for which stacks
  // or checks, and optionally only on the first alert of an outage. Nobody by default.
  let { kind, config = $bindable() }: { kind: 'discord' | 'slack'; config: ChannelConfig } = $props();

  const events = [
    ['down', 'DOWN'],
    ['up', 'Back up'],
    ['degraded', 'Degraded'],
    ['agent_offline', 'Host offline'],
    ['update_failed', 'Update / deploy failed'],
    ['rolled_back', 'Rolled back'],
    ['self_rolled_back', 'Apply & restart rolled back'],
  ] as const;

  const rules = $derived(config.mentions ?? []);
  function setRules(r: MentionRule[]) {
    config.mentions = r.length ? r : undefined;
  }
  function add(preset = false) {
    setRules([...rules, { who: preset ? [kind === 'discord' ? 'role:' : 'group:'] : [], events: ['down'], firstOnly: preset }]);
  }
  function remove(i: number) {
    setRules(rules.filter((_, j) => j !== i));
  }
  function whoText(r: MentionRule) {
    return r.who.join(', ');
  }
  function setWho(r: MentionRule, v: string) {
    r.who = v
      .split(/[\s,]+/)
      .map((x) => x.trim())
      .filter(Boolean)
      .map((x) => (x === '@here' ? 'here' : x === '@channel' ? 'channel' : x));
    setRules([...rules]);
  }
  function toggleEvent(r: MentionRule, e: string) {
    r.events = r.events.includes(e) ? r.events.filter((x) => x !== e) : [...r.events, e];
    setRules([...rules]);
  }
  function setList(r: MentionRule, key: 'stacks' | 'checks', v: string) {
    const list = v.split(',').map((x) => x.trim()).filter(Boolean);
    r[key] = list.length ? list : undefined;
    setRules([...rules]);
  }
  const preview = $derived(
    [...new Set(rules.filter((r) => r.events.includes('down') && !r.stacks?.length && !r.checks?.length).flatMap((r) => r.who))].join(', '),
  );
</script>

<div class="mentions">
  <span class="label">Mentions</span>
  {#if rules.length === 0}
    <p class="hint">Nobody is mentioned. <button type="button" class="btn btn-ghost btn-sm" onclick={() => add(true)}>Mention a {kind === 'discord' ? 'role' : 'user group'} on DOWN</button></p>
  {/if}
  {#each rules as r, i (i)}
    <div class="rule">
      <label>
        <span class="hint">Who ({kind === 'discord' ? 'role:<id>, user:<id>, @here' : 'group:<id>, user:<id>, @here, @channel'})</span>
        <input class="field mono" value={whoText(r)} onchange={(e) => setWho(r, (e.target as HTMLInputElement).value)} placeholder={kind === 'discord' ? 'role:123456789012345678' : 'group:S0123ABC'} />
      </label>
      <div class="evs">
        {#each events as [id, label] (id)}
          <label class="check"><input type="checkbox" checked={r.events.includes(id)} onchange={() => toggleEvent(r, id)} /> {label}</label>
        {/each}
      </div>
      <div class="scope">
        <label><span class="hint">Only stacks (comma separated, empty = all)</span><input class="field" value={(r.stacks ?? []).join(', ')} onchange={(e) => setList(r, 'stacks', (e.target as HTMLInputElement).value)} /></label>
        <label><span class="hint">Only checks (names, empty = all)</span><input class="field" value={(r.checks ?? []).join(', ')} onchange={(e) => setList(r, 'checks', (e.target as HTMLInputElement).value)} /></label>
      </div>
      <div class="row">
        <label class="check"><input type="checkbox" bind:checked={r.firstOnly} onchange={() => setRules([...rules])} /> Only the first alert of an outage</label>
        <button type="button" class="btn btn-ghost btn-sm" onclick={() => remove(i)} aria-label="Remove rule"><Icon name="trash" /></button>
      </div>
    </div>
  {/each}
  {#if rules.length}
    <button type="button" class="btn btn-sm" onclick={() => add()} disabled={rules.length >= 10}><Icon name="plus" /> Add rule</button>
    <p class="hint">
      A DOWN alert would mention: {preview || 'nobody (all rules are narrowed)'}. Only these IDs can ping – text from logs never mentions anyone.
      {kind === 'discord' ? 'Copy a role ID in Discord with Developer Mode on (right-click the role → Copy ID).' : 'User group IDs start with S (Slack → People → User groups).'}
    </p>
  {/if}
</div>

<style>
  .mentions {
    display: grid;
    gap: 8px;
  }
  .rule {
    display: grid;
    gap: 6px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .rule label {
    display: grid;
    gap: 2px;
  }
  .evs {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 12px;
  }
  .evs .check,
  .row .check {
    display: inline-flex;
    gap: 6px;
    align-items: center;
  }
  .scope {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
    gap: 6px;
  }
  .row {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
</style>
