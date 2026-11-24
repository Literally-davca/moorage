<!--
  Accepted HTTP status codes as chips. Quick picks for common codes and ranges, plus
  typed codes (100–599) or ranges (200-204): Enter or a comma makes a chip, Backspace in
  the empty input or × removes one. The value is the comma list the server stores ("200,204").
-->
<script lang="ts">
  import Icon from './Icon.svelte';
  import { commonStatusCodes, splitStatus, validStatusToken } from './checks';

  let { value = $bindable(''), id = 'codes' }: { value?: string; id?: string } = $props();

  let draft = $state('');
  let bad = $state('');
  const chips = $derived(splitStatus(value));

  function set(next: string[]) {
    value = [...new Set(next)].join(',');
  }
  function add(raw: string) {
    const t = raw.trim().replace(/\s+/g, '').replace('–', '-');
    if (!t) return true;
    if (!validStatusToken(t)) {
      bad = `${t} isn't a code (100–599) or a range like 200-204.`;
      return false;
    }
    bad = '';
    set([...chips, t]);
    return true;
  }
  function remove(t: string) {
    set(chips.filter((c) => c !== t));
  }
  function toggle(t: string) {
    if (chips.includes(t)) remove(t);
    else set([...chips, t]);
  }
  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      if (add(draft)) draft = '';
    } else if (e.key === 'Backspace' && draft === '' && chips.length) {
      remove(chips[chips.length - 1]);
    }
  }
  function onblur() {
    if (draft && add(draft)) draft = '';
  }
</script>

<div class="codes">
  <div class="box field" class:invalid={!!bad}>
    {#each chips as c (c)}
      <span class="chip mono">{c}<button type="button" class="x" aria-label={`Remove ${c}`} onclick={() => remove(c)}><Icon name="x" /></button></span>
    {:else}
      <span class="chip mono placeholder" title="Without codes, any 200–399 answer passes">200-399 (default)</span>
    {/each}
    <input {id} class="mono" bind:value={draft} {onkeydown} {onblur} placeholder={chips.length ? '' : 'Type a code or range'}
      inputmode="numeric" autocomplete="off" spellcheck="false" aria-describedby={`${id}-hint`} />
  </div>
  {#if bad}<p class="hint bad" role="alert">{bad}</p>{/if}
  <div class="quick" id={`${id}-hint`}>
    {#each commonStatusCodes as c (c)}
      <button type="button" class="q mono" class:on={chips.includes(c)} aria-pressed={chips.includes(c)} onclick={() => toggle(c)}>{c}</button>
    {/each}
  </div>
</div>

<style>
  .box {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px;
    min-height: 36px;
    height: auto;
    padding: 4px 6px;
  }
  .box.invalid {
    box-shadow: inset 0 0 0 1px var(--adrift);
  }
  .box input {
    flex: 1;
    min-width: 80px;
    border: 0;
    outline: 0;
    background: transparent;
    color: var(--foam);
    font-size: 13px;
    height: 26px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    height: 24px;
    padding: 0 4px 0 8px;
    border-radius: 999px;
    background: var(--rope-strong);
    color: var(--foam);
    font-size: 12px;
  }
  .chip.placeholder {
    padding: 0 8px;
    background: transparent;
    box-shadow: inset 0 0 0 1px var(--rope-strong);
    color: var(--haze);
  }
  .x {
    display: inline-grid;
    place-items: center;
    width: 18px;
    height: 18px;
    border: 0;
    border-radius: 50%;
    background: transparent;
    color: var(--haze);
    cursor: pointer;
    padding: 0;
  }
  .x:hover {
    background: var(--rope-stronger);
    color: var(--foam);
  }
  .x :global(svg) {
    width: 12px;
    height: 12px;
  }
  .quick {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    margin-top: 6px;
  }
  .q {
    height: 24px;
    padding: 0 8px;
    border: 0;
    border-radius: 999px;
    background: transparent;
    box-shadow: inset 0 0 0 1px var(--rope-strong);
    color: var(--mist);
    font-size: 12px;
    cursor: pointer;
  }
  .q:hover {
    color: var(--foam);
  }
  .q.on {
    background: var(--buoy-wash);
    box-shadow: inset 0 0 0 1px var(--buoy);
    color: var(--foam);
  }
  .hint.bad {
    color: var(--adrift-text);
    font-size: 12px;
    margin: 4px 0 0;
  }
</style>
