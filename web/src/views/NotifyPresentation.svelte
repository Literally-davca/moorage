<script lang="ts">
  // How much a channel's messages contain (M17B): preset Minimal / Standard / Detailed /
  // Custom, the parts of Custom, rich or plain format, and a live preview of the message.
  import Icon from '../lib/Icon.svelte';
  import type { ChannelConfig, ChannelKind } from '../lib/api';
  import { notifications, parts, presetParts, presets, richLabel, supportsRich, type NotifyPart, type NotifyPreset, type NotifyView } from '../lib/notifications';

  let { kind, config = $bindable() }: { kind: ChannelKind; config: ChannelConfig } = $props();

  const preset = $derived<NotifyPreset>(config.preset ?? 'standard');
  const rich = $derived(supportsRich(kind) && config.format !== 'plain');

  function setPreset(p: NotifyPreset) {
    const next: ChannelConfig = { ...config, preset: p };
    if (p === 'custom') {
      // start Custom from what the previous preset showed
      next.include = config.include && config.preset === 'custom' ? config.include : presetParts(config.preset);
    } else {
      delete next.include;
    }
    config = next;
  }

  function togglePart(k: NotifyPart) {
    const inc = { ...(config.include ?? {}) };
    if (inc[k]) delete inc[k];
    else inc[k] = true;
    config = { ...config, include: inc };
  }

  function setFormat(f: 'rich' | 'plain') {
    const next: ChannelConfig = { ...config, format: f };
    if (f === 'rich') delete next.format; // rich is the default
    config = next;
  }

  // ---- preview ----
  let view = $state<NotifyView | null>(null);
  let payload = $state('');
  let showPayload = $state(false);
  let previewErr = $state('');

  $effect(() => {
    const k = kind;
    const c = $state.snapshot(config) as ChannelConfig;
    const t = setTimeout(async () => {
      try {
        const r = await notifications.preview(k, c);
        view = r.view;
        payload = r.payload ?? '';
        previewErr = '';
      } catch (e) {
        previewErr = (e as Error).message;
      }
    }, 300);
    return () => clearTimeout(t);
  });

  const statusClass = (s: string) => (s === 'down' ? 'down' : s === 'up' ? 'up' : 'test');
</script>

<div class="pres">
  <div>
    <span class="label">Message content</span>
    <div class="seg" role="radiogroup" aria-label="Message content">
      {#each presets as p (p.k)}
        <button type="button" role="radio" aria-checked={preset === p.k} class:on={preset === p.k} onclick={() => setPreset(p.k)}>
          <span class="pname">{p.label}</span>
          <span class="phint">{p.hint}</span>
        </button>
      {/each}
    </div>
    {#if preset === 'custom'}
      <div class="parts">
        <span class="meta">Status and title are always included.</span>
        <div class="part-list">
          {#each parts as p (p.k)}
            <label class="part">
              <input type="checkbox" checked={!!config.include?.[p.k]} onchange={() => togglePart(p.k)} />
              <span>{p.label}</span>
            </label>
          {/each}
        </div>
      </div>
    {/if}
  </div>

  {#if supportsRich(kind)}
    <div>
      <span class="label">Format</span>
      <div class="seg two" role="radiogroup" aria-label="Format">
        <button type="button" role="radio" aria-checked={rich} class:on={rich} onclick={() => setFormat('rich')}>
          <span class="pname">Rich</span>
          <span class="phint">{richLabel[kind]}</span>
        </button>
        <button type="button" role="radio" aria-checked={!rich} class:on={!rich} onclick={() => setFormat('plain')}>
          <span class="pname">Plain text</span>
          <span class="phint">For old clients and bridges</span>
        </button>
      </div>
    </div>
  {/if}

  <div>
    <div class="prev-head">
      <span class="label">Preview</span>
      <span class="meta">Example alert, sent as by “Send test”</span>
    </div>
    {#if view}
      <div class="preview {statusClass(view.status)}" aria-live="polite">
        <div class="sender">
          <img src={view.iconUrl} alt="" width="20" height="20" loading="lazy" referrerpolicy="no-referrer" />
          <span>{view.sender}</span>
        </div>
        <div class="ptitle">{view.title}</div>
        {#if view.description}<p class="desc">{view.description}</p>{/if}
        {#if view.fields?.length}
          <dl class="fields">
            {#each view.fields as f, i (i)}
              <div class:full={!f.inline}>
                <dt>{f.name}</dt>
                <dd>{f.value}</dd>
              </div>
            {/each}
          </dl>
        {/if}
        {#if view.logs?.length}<pre class="logs">{view.logs.join('\n')}</pre>{/if}
        <div class="foot">
          <span>{view.footer}</span>
          {#if view.url}<span class="mono link"><Icon name="external" /> Open in Moorage</span>{/if}
        </div>
      </div>
      {#if payload}
        <button type="button" class="btn btn-ghost raw-toggle" aria-expanded={showPayload} onclick={() => (showPayload = !showPayload)}>
          <Icon name="code" /> {showPayload ? 'Hide' : 'Show'} what is sent
        </button>
        {#if showPayload}<pre class="payload">{payload}</pre>{/if}
      {/if}
    {:else if previewErr}
      <p class="meta">{previewErr}</p>
    {:else}
      <div class="skeleton" style="height:64px"></div>
    {/if}
  </div>
</div>

<style>
  .pres {
    display: grid;
    gap: 14px;
  }
  .seg {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 6px;
    margin-top: 4px;
  }
  .seg.two {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  @media (max-width: 720px) {
    .seg {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  .seg button {
    display: grid;
    gap: 2px;
    text-align: left;
    padding: 9px 12px;
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
  .pname {
    font-size: 13px;
    font-weight: 600;
  }
  .phint {
    font-size: 11px;
    color: var(--haze);
  }
  .parts {
    display: grid;
    gap: 6px;
    margin-top: 8px;
  }
  .part-list {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 16px;
  }
  .part {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 13px;
    cursor: pointer;
  }
  .prev-head {
    display: flex;
    align-items: baseline;
    gap: 8px;
    flex-wrap: wrap;
  }
  .preview {
    margin-top: 6px;
    padding: 12px 14px;
    border-radius: var(--r-md);
    background: var(--well);
    box-shadow: inset 3px 0 0 var(--buoy), inset 0 0 0 1px var(--rope);
    display: grid;
    gap: 6px;
    min-width: 0;
  }
  .preview.down {
    box-shadow: inset 3px 0 0 var(--adrift), inset 0 0 0 1px var(--rope);
  }
  .preview.up {
    box-shadow: inset 3px 0 0 var(--afloat), inset 0 0 0 1px var(--rope);
  }
  .sender {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    font-weight: 600;
    color: var(--mist);
  }
  .sender img {
    border-radius: 5px;
    flex: none;
  }
  .ptitle {
    font-size: 14px;
    font-weight: 600;
    color: var(--foam);
    text-wrap: balance;
  }
  .desc {
    margin: 0;
    font-size: 13px;
    color: var(--mist);
    white-space: pre-line;
    text-wrap: pretty;
  }
  .fields {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(140px, 1fr));
    gap: 6px 12px;
    margin: 0;
  }
  .fields .full {
    grid-column: 1 / -1;
  }
  .fields dt {
    font-size: 11px;
    font-weight: 600;
    color: var(--haze);
  }
  .fields dd {
    margin: 0;
    font-size: 12px;
    color: var(--foam);
    white-space: pre-line;
    overflow-wrap: anywhere;
  }
  .logs,
  .payload {
    margin: 0;
    padding: 8px 10px;
    border-radius: var(--r-sm);
    background: var(--deep-water);
    font: 12px/1.6 var(--mono);
    color: var(--mist);
    overflow-x: auto;
    white-space: pre;
  }
  .payload {
    max-height: 320px;
    overflow: auto;
    margin-top: 6px;
  }
  .foot {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    font-size: 11px;
    color: var(--haze);
  }
  .link {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    color: var(--buoy-text);
  }
  .raw-toggle {
    margin-top: 6px;
  }
</style>
