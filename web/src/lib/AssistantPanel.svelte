<script lang="ts">
  import Icon from './Icon.svelte';
  import Select from './Select.svelte';
  import AssistantChat from './AssistantChat.svelte';
  import { app, go } from './state.svelte';
  import { chat, reset, revoke, loadStatus, loadCommands } from './assistantStore.svelte';

  // The assistant panel (M18G, M19E): docked on the right from 1280 px (the page gets
  // narrower), an overlay below, full screen under 768 px. Expand opens the same chat on
  // #/assistant. The chat lives in assistantChat.svelte.ts, so it survives page changes.

  const MIN_W = 360;
  const MAX_W = 640;
  let width = $state(readWidth());
  let vw = $state(typeof window !== 'undefined' ? window.innerWidth : 1400);
  const onPage = $derived((app.route as { page: string }).page === 'assistant');
  const shown = $derived(chat.open && !onPage);
  const docked = $derived(shown && vw >= 1280);
  const mobile = $derived(vw < 768);

  function readWidth(): number {
    try {
      const n = Number(localStorage.getItem('moorage-assistant-width'));
      if (n >= MIN_W && n <= MAX_W) return n;
    } catch {
      /* storage blocked */
    }
    return 420;
  }

  // the page makes room for the docked panel
  $effect(() => {
    const root = document.documentElement;
    if (docked) root.style.setProperty('--assistant-dock', width + 'px');
    else root.style.removeProperty('--assistant-dock');
    return () => root.style.removeProperty('--assistant-dock');
  });

  $effect(() => {
    if (shown) {
      void loadStatus(true);
      void loadCommands();
    }
  });

  let dragging = false;
  function startDrag(e: PointerEvent) {
    dragging = true;
    (e.target as HTMLElement).setPointerCapture(e.pointerId);
  }
  function onDrag(e: PointerEvent) {
    if (!dragging) return;
    width = Math.min(MAX_W, Math.max(MIN_W, window.innerWidth - e.clientX));
  }
  function endDrag() {
    dragging = false;
    try {
      localStorage.setItem('moorage-assistant-width', String(width));
    } catch {
      /* storage blocked */
    }
  }
  function onResizeKey(e: KeyboardEvent) {
    if (e.key === 'ArrowLeft') width = Math.min(MAX_W, width + 20);
    else if (e.key === 'ArrowRight') width = Math.max(MIN_W, width - 20);
    else return;
    e.preventDefault();
    endDrag();
  }

  function close() {
    chat.open = false;
    document.getElementById('assistant-toggle')?.focus();
  }
  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && !chat.busy && !chat.editing) close();
  }
  function onGlobalKey(e: KeyboardEvent) {
    if ((e.ctrlKey || e.metaKey) && e.shiftKey && e.key.toLowerCase() === 'o' && (shown || onPage)) {
      e.preventDefault();
      reset();
    }
  }

  // ---- model picker (admins, among models allowed in the settings) ----
  const modelItems = $derived(
    (chat.info?.models ?? []).flatMap((g) => g.models.map((m) => ({ value: g.id + '|' + m, label: m, meta: g.name }))),
  );
  const pickValue = $derived(
    chat.pick.provider ? chat.pick.provider + '|' + chat.pick.model : chat.info?.default ? chat.info.default.provider + '|' + chat.info.default.model : '',
  );
  function pickModel(v: string) {
    const [p, ...rest] = v.split('|');
    chat.pick = { provider: p, model: rest.join('|') };
  }
  const tokenWarn = $derived(
    chat.info?.tokenLimit && chat.info.tokensUsed !== undefined && chat.info.tokensUsed >= chat.info.tokenLimit * 0.8
      ? `${Math.round((chat.info.tokensUsed / chat.info.tokenLimit) * 100)} % of this month's tokens used`
      : '',
  );
</script>

<svelte:window onresize={() => (vw = window.innerWidth)} onkeydown={onGlobalKey} />

{#if shown}
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <aside class="assistant card" class:docked class:mobile style:width={mobile ? undefined : width + 'px'} aria-label="Assistant" onkeydown={onKey}>
    {#if !mobile}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div
        class="grip"
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize the assistant"
        aria-valuemin={MIN_W}
        aria-valuemax={MAX_W}
        aria-valuenow={width}
        tabindex="0"
        onpointerdown={startDrag}
        onpointermove={onDrag}
        onpointerup={endDrag}
        onkeydown={onResizeKey}
      ></div>
    {/if}
    <header class="ahead">
      <strong><Icon name="wand" /> Assistant</strong>
      <button type="button" class="btn btn-ghost btn-sm" onclick={reset} title="New chat (Ctrl+Shift+O)" disabled={chat.items.length === 0}>New chat</button>
      <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={() => go('assistant')} aria-label="Expand to a page"><Icon name="maximize" /></button>
      <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={close} aria-label="Close assistant (Esc)"><Icon name="x" /></button>
    </header>
    <div class="sub">
      {#if chat.info?.admin && modelItems.length > 1}
        <div class="picker"><Select items={modelItems} value={pickValue} label="Model" compact onchange={pickModel} /></div>
      {:else if chat.info?.model}
        <span class="meta">{chat.info.model}</span>
      {/if}
      {#each chat.allow as a (a)}
        <span class="chip">{a} allowed <button type="button" aria-label="Revoke {a}" onclick={() => revoke(a)}><Icon name="x" /></button></span>
      {/each}
      {#if tokenWarn}<span class="meta warn">{tokenWarn}</span>{/if}
    </div>
    <AssistantChat />
  </aside>
{/if}

<style>
  .assistant {
    position: fixed;
    right: 12px;
    top: 72px;
    bottom: 12px;
    max-width: calc(100vw - 24px);
    z-index: 60;
    display: grid;
    grid-template-rows: auto auto 1fr;
    gap: 6px;
    padding: 10px 12px;
  }
  .assistant.docked {
    right: 0;
    top: 0;
    bottom: 0;
    border-radius: 0;
    box-shadow: -1px 0 0 var(--rope-strong);
  }
  .assistant.mobile {
    inset: 0;
    max-width: none;
    border-radius: 0;
    padding: max(10px, env(safe-area-inset-top)) max(12px, env(safe-area-inset-right)) max(10px, env(safe-area-inset-bottom))
      max(12px, env(safe-area-inset-left));
  }
  .grip {
    position: absolute;
    left: -4px;
    top: 0;
    bottom: 0;
    width: 8px;
    cursor: col-resize;
  }
  .grip:focus-visible {
    outline: 2px solid var(--buoy);
  }
  .ahead {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .ahead strong {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin-right: auto;
  }
  .sub {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .picker {
    min-width: 180px;
    max-width: 100%;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    padding: 1px 4px 1px 8px;
    border-radius: 999px;
    font-size: 12px;
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .chip button {
    border: 0;
    background: none;
    color: inherit;
    cursor: pointer;
    display: inline-flex;
  }
  .warn {
    color: var(--signal-text);
  }
</style>
