<script lang="ts">
  import { tick } from 'svelte';
  import Icon from './Icon.svelte';
  import Markdown from './Markdown.svelte';
  import QrLink from './QrLink.svelte';
  import { app, isKnownRoute } from './state.svelte';
  import type { LinkCheck } from './markdown';
  import {
    chat,
    send,
    stop,
    regenerate,
    editLast,
    cancelEdit,
    grant,
    decide,
    answerChoice,
    reset,
    builtinCommands,
    lastAnswerChanged,
    userMessageCount,
    MAX_MESSAGES,
    MAX_MESSAGE_BYTES,
    type ChatItem,
  } from './assistantStore.svelte';

  // The conversation (M19E): used by the docked panel and the #/assistant page.

  let log = $state<HTMLDivElement | null>(null);
  let box = $state<HTMLTextAreaElement | null>(null);
  let atBottom = $state(true);
  let newBelow = $state(false);
  let now = $state(Date.now());
  let openReads = $state<Record<string, boolean>>({});
  let openThinking = $state<Record<number, boolean>>({});
  let copiedIdx = $state(-1);
  let slashSel = $state(0);
  let announce = $state('');

  $effect(() => {
    if (!chat.busy) return;
    const t = setInterval(() => (now = Date.now()), 1000);
    return () => clearInterval(t);
  });

  // focus the box when asked (open, after an answer, Edit)
  $effect(() => {
    void chat.focusTick;
    tick().then(() => box?.focus());
  });

  // auto-scroll only when the user is at the end of the log
  $effect(() => {
    void chat.items.length;
    const last = chat.items[chat.items.length - 1];
    void (last && last.kind === 'assistant' ? last.text.length : 0);
    tick().then(() => {
      if (!log) return;
      if (atBottom) log.scrollTop = log.scrollHeight;
      else newBelow = true;
    });
  });

  // screen readers hear finished answers only, not every token
  $effect(() => {
    if (chat.busy) return;
    const last = [...chat.items].reverse().find((i) => i.kind === 'assistant');
    if (last && last.kind === 'assistant') announce = last.text.slice(0, 400);
  });

  function onScroll() {
    if (!log) return;
    atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
    if (atBottom) newBelow = false;
  }
  function toBottom() {
    if (log) log.scrollTop = log.scrollHeight;
    newBelow = false;
  }

  // ---- composer ----
  const bytes = $derived(new TextEncoder().encode(chat.input).length);
  const slashOptions = $derived.by(() => {
    const m = /^\/([a-z0-9-]*)$/.exec(chat.input);
    if (!m) return [];
    const all = [
      ...builtinCommands.map((c) => ({ name: c.name, label: c.label })),
      ...chat.commands.map((c) => ({ name: c.name, label: c.text.slice(0, 80) })),
    ];
    return all.filter((c) => c.name.startsWith(m[1])).slice(0, 8);
  });
  const count = $derived(userMessageCount());
  const atLimit = $derived(count >= MAX_MESSAGES);

  function autosize() {
    if (!box) return;
    box.style.height = 'auto';
    const line = 20;
    box.style.height = Math.min(Math.max(box.scrollHeight, line * 2 + 16), line * 8 + 16) + 'px';
  }
  $effect(() => {
    void chat.input;
    tick().then(autosize);
  });

  function onKey(e: KeyboardEvent) {
    if (slashOptions.length) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        slashSel = (slashSel + (e.key === 'ArrowDown' ? 1 : slashOptions.length - 1)) % slashOptions.length;
        return;
      }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
        e.preventDefault();
        chat.input = '/' + slashOptions[Math.min(slashSel, slashOptions.length - 1)].name + ' ';
        slashSel = 0;
        return;
      }
    }
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      if (bytes <= MAX_MESSAGE_BYTES) void send();
      return;
    }
    if (e.key === 'ArrowUp' && !chat.input && !chat.busy) {
      e.preventDefault();
      editLast();
      return;
    }
    if (e.key === 'Escape') {
      if (chat.busy) {
        e.stopPropagation();
        stop();
      } else if (chat.editing) {
        e.stopPropagation();
        cancelEdit();
      }
    }
  }

  // ---- empty state ----
  const suggestions = $derived.by(() => {
    const r = app.route as { page: string; name?: string; tab?: string };
    if (r.page === 'stack' && r.name) return [`Why is ${r.name} failing?`, `Explain the stack ${r.name}`, `Where does ${r.name} run?`];
    if (r.page === 'settings') return ['Where do I set up alerts?', 'How do I add a host?'];
    if (r.page === 'monitoring' || r.page === 'check') return ['Which checks are down and why?', 'How do I add a check?'];
    return ['Which stacks need attention?', 'Where does my media server run?', 'How do I add a new service?'];
  });

  // ---- links ----
  function checkFor(it: ChatItem): LinkCheck {
    const allowed = new Set(it.kind === 'assistant' ? it.links.map((l) => l.url) : []);
    return (url: string) => {
      if (url.startsWith('#/')) return isKnownRoute(url) ? { href: url, external: false } : null;
      if (url.startsWith(location.origin + '/#/')) {
        const h = url.slice(location.origin.length + 1);
        return isKnownRoute(h) ? { href: h, external: false } : null;
      }
      if (allowed.has(url)) return { href: url, external: true };
      return null;
    };
  }
  function primaryLinks(it: ChatItem) {
    if (it.kind !== 'assistant') return [];
    const seen = new Set<string>();
    return it.links.filter((l) => l.primary && !seen.has(l.service + l.stack) && seen.add(l.service + l.stack)).slice(0, 4);
  }
  function altLinks(it: ChatItem, primary: { service: string; stack?: string }) {
    if (it.kind !== 'assistant') return [];
    return it.links.filter((l) => !l.primary && l.service === primary.service && l.stack === primary.stack);
  }

  // ---- errors ----
  const errorText: Record<string, string> = {
    unreachable: "Moorage can't reach the AI provider.",
    auth: 'The AI provider refused the API key.',
    forbidden: "The API key isn't allowed to use this model.",
    rate_limit: 'The AI provider is busy (rate limit). Try again in a moment.',
    quota: 'The AI provider account has no credit left.',
    model: "The AI provider doesn't know this model.",
    timeout: 'The AI provider took too long to answer.',
  };

  async function copyAnswer(text: string, i: number) {
    try {
      await navigator.clipboard.writeText(text);
      copiedIdx = i;
      setTimeout(() => (copiedIdx = -1), 1500);
    } catch {
      copiedIdx = -2;
    }
  }

  function lastAssistantIndex() {
    for (let i = chat.items.length - 1; i >= 0; i--) if (chat.items[i].kind === 'assistant') return i;
    return -1;
  }
  function lastUserIndex() {
    for (let i = chat.items.length - 1; i >= 0; i--) if (chat.items[i].kind === 'user') return i;
    return -1;
  }
  const changed = $derived(lastAnswerChanged());

  function diffLines(d: string) {
    return d.split('\n').filter((l, i, a) => l !== '' || i < a.length - 1);
  }
  let openDiff = $state<Record<number, boolean>>({});

  async function apply(it: ChatItem & { kind: 'action' }) {
    const res = await decide(it, true);
    if (res && res.selfApply) {
      it.result = res.ownHub
        ? 'Apply & restart started: Moorage restarts and this page reloads when it is back.'
        : 'Apply & restart started on the agent: it restarts and reconnects within a minute.';
    }
  }

  function args(a: unknown) {
    try {
      return JSON.stringify(a, null, 1);
    } catch {
      return '';
    }
  }
</script>

<div class="chat">
  <div class="log" role="log" aria-label="Conversation" bind:this={log} onscroll={onScroll}>
    {#if chat.items.length === 0}
      <div class="empty">
        <p class="meta">Ask about your stacks, why something fails, or where a setting is.</p>
        <div class="sugg">
          {#each suggestions as s (s)}
            <button type="button" class="btn btn-sm" onclick={() => { chat.input = s; chat.focusTick++; }}>{s}</button>
          {/each}
        </div>
        <p class="meta small">
          {chat.info?.model ?? ''}{chat.info?.model ? ' · ' : ''}The chat isn't saved. Type <code>/</code> for commands.
        </p>
      </div>
    {/if}
    {#each chat.items as it, i (i)}
      {#if it.kind === 'user'}
        <div class="row-user" class:faded={chat.editing && i >= lastUserIndex()}>
          <span class="sr">You:</span>
          {#if it.text.length > 4000}
            <details class="bubble user">
              <summary>Pasted text – {it.text.split('\n').length} lines</summary>
              <pre class="mono paste">{it.text}</pre>
            </details>
          {:else}
            <div class="bubble user">{it.text}</div>
          {/if}
          {#if i === lastUserIndex() && !chat.busy && !chat.editing}
            <button type="button" class="btn btn-ghost btn-sm" onclick={editLast} disabled={changed}
              title={changed ? 'This answer already changed something – ask a new question.' : 'Edit your message (↑)'}>Edit</button>
          {/if}
        </div>
      {:else if it.kind === 'assistant'}
        <div class="answer" class:faded={chat.editing && i > lastUserIndex()}>
          <span class="sr">Assistant:</span>
          {#if it.thinking && chat.info?.showThinking !== false}
            <details class="thinking" bind:open={openThinking[i]}>
              <summary class="meta">
                {it.streaming && !it.text ? 'Thinking…' : `Thought for ${Math.max(1, Math.round(it.thinkingMs / 1000))} s`}
                {#if it.streaming && !it.text && !openThinking[i]}<span class="think-live">{it.thinking.split('\n').filter(Boolean).at(-1)}</span>{/if}
              </summary>
              <pre class="think-text">{it.thinking}</pre>
            </details>
          {/if}
          {#if it.reads.length}
            <div class="reads">
              <span class="meta">Read:</span>
              {#each it.reads as r, j (j)}
                {@const key = i + ':' + j}
                <button type="button" class="read-chip" class:failed={r.failed} aria-expanded={!!openReads[key]} onclick={() => (openReads[key] = !openReads[key])}>{r.target}</button>
                {#if openReads[key]}
                  <div class="read-box">
                    {#if r.args}<pre class="mono read-args">{args(r.args)}</pre>{/if}
                    {#if r.result}<pre class="mono read-result">{r.result}</pre>{/if}
                  </div>
                {/if}
              {/each}
            </div>
          {/if}
          {#if it.text}
            <Markdown text={it.text} check={checkFor(it)} />
          {/if}
          {#if it.streaming && it.text}<span class="cursor" aria-hidden="true"></span>{/if}
          {#if it.stopped}<p class="meta">Stopped.</p>{/if}
          {#if it.incomplete && !it.stopped}<p class="meta">The answer is incomplete.</p>{/if}
          {#each primaryLinks(it) as l (l.url)}
            <div class="open-row">
              <a class="btn btn-sm btn-primary" href={l.url} target="_blank" rel="noopener noreferrer"><Icon name="world" /> Open {l.service}</a>
              <QrLink url={l.url} />
              {#if altLinks(it, l).length}
                <details class="alts">
                  <summary class="meta">Other addresses</summary>
                  {#each altLinks(it, l) as a (a.url)}
                    <div class="meta">{a.label} <a href={a.url} target="_blank" rel="noopener noreferrer">{a.url}</a></div>
                  {/each}
                </details>
              {/if}
            </div>
          {/each}
          {#if !it.streaming}
            <div class="answer-acts">
              {#if it.text}
                <button type="button" class="btn btn-ghost btn-sm" onclick={() => copyAnswer(it.text, i)} title={it.tokens ? `${it.tokens.toLocaleString()} tokens · ${it.model ?? ''}` : it.model}>
                  <Icon name={copiedIdx === i ? 'check' : 'copy'} /> {copiedIdx === i ? 'Copied' : 'Copy'}
                </button>
              {/if}
              {#if i === lastAssistantIndex() && !chat.busy}
                <button type="button" class="btn btn-ghost btn-sm" onclick={regenerate} disabled={changed}
                  title={changed ? 'This answer already changed something – ask a new question.' : 'Answer again'}><Icon name="refresh" /> Regenerate</button>
              {/if}
            </div>
          {/if}
        </div>
      {:else if it.kind === 'action'}
        <div class="card-act" class:self={it.action.selfApply}>
          <div class="act-head">
            <strong>{it.action.title}</strong>
            {#if it.action.stack}<span class="meta">{it.action.stack}{it.action.host ? ' · host ' + it.action.host : ''}</span>{/if}
          </div>
          <p class="meta">
            {#if it.state === 'pending'}Proposal – nothing changes until you apply it.{#if it.action.demoted} (Proposed instead of done: outside content was read{it.action.selfApply ? ', or it is Moorage itself' : ''}.){/if}
            {:else if it.state === 'done'}Done by the assistant.
            {:else if it.state === 'applied'}Applied.
            {:else if it.state === 'rejected'}Rejected.
            {:else}Out of date – the stack changed since this was proposed.{/if}
          </p>
          {#if it.action.widens?.length}
            <div class="widen" role="alert">
              <Icon name="alert" />
              <div>
                <strong>This gives Moorage more access to the host:</strong>
                <ul>{#each it.action.widens as w (w)}<li>{w}</li>{/each}</ul>
              </div>
            </div>
          {/if}
          {#if it.action.impact}<p class="meta">{it.action.impact}</p>{/if}
          {#if it.action.diff}
            <div class="diff mono" class:open={openDiff[i]}>
              {#each diffLines(it.action.diff) as l, k (k)}<div class:add={l.startsWith('+')} class:del={l.startsWith('-')}>{l}</div>{/each}
            </div>
            {#if it.action.diff.split('\n').length > 10}
              <button type="button" class="btn btn-ghost btn-sm" onclick={() => (openDiff[i] = !openDiff[i])}>{openDiff[i] ? 'Collapse' : 'Expand'}</button>
            {/if}
          {/if}
          {#if it.state === 'pending' && it.action.draftId}
            <div class="acts">
              <button type="button" class="btn btn-sm btn-primary" onclick={() => apply(it)}>{it.action.selfApply ? 'Apply & restart' : 'Apply'}</button>
              <button type="button" class="btn btn-sm btn-ghost" onclick={() => decide(it, false)}>Reject</button>
            </div>
          {:else if it.state === 'applied' || it.state === 'done'}
            <p class="meta">
              {it.result ?? ''}
              {#if it.action.stack && it.action.hostId}
                <a href={`#/h/${encodeURIComponent(it.action.hostId)}/stacks/${encodeURIComponent(it.action.stack)}/history`}>Open History</a>
              {/if}
              {it.action.undo ?? ''}
            </p>
          {:else if it.state === 'stale'}
            <button type="button" class="btn btn-sm" onclick={() => send('The stack changed. Please look again and redo the proposal.')} disabled={chat.busy}>Ask to redo it</button>
          {/if}
          {#if it.error && it.state !== 'stale'}<p class="error-text">{it.error}</p>{/if}
        </div>
      {:else if it.kind === 'permission'}
        <div class="card-act">
          <span>The assistant wants to read <strong>{it.what}</strong> (secrets are masked).</span>
          {#if it.granted}
            <span class="meta">Allowed for this chat.</span>
          {:else}
            <button type="button" class="btn btn-sm btn-primary" onclick={() => grant(it.what)} disabled={chat.busy}>Allow for this chat</button>
          {/if}
        </div>
      {:else if it.kind === 'choice'}
        <div class="card-act">
          <strong>{it.question}</strong>
          {#if it.answered}
            <span class="meta">{it.answered === '(answered in the chat)' ? 'Answered in the chat.' : 'You chose: ' + it.answered}</span>
          {:else}
            <div class="acts wrap">
              {#each it.options as o, k (o)}
                <button type="button" class="btn btn-sm" class:btn-primary={k === 0} onclick={() => answerChoice(it, o)} disabled={chat.busy}>{o}</button>
              {/each}
            </div>
          {/if}
        </div>
      {:else if it.kind === 'error'}
        <div class="bubble err" role="alert">
          <p>{(it.code && errorText[it.code]) || it.text}</p>
          {#if it.code === 'unreachable' && /localhost|127\.0\.0\.1/.test(it.text + (it.detail ?? ''))}
            <p class="meta">In a container, <code>localhost</code> is the container itself: use the host's address or <code>host.docker.internal</code>.</p>
          {/if}
          {#if it.detail || (it.code && errorText[it.code])}
            <details><summary class="meta">Details</summary><pre class="mono">{it.text}{it.detail ? '\n' + it.detail : ''}</pre></details>
          {/if}
          <div class="acts wrap">
            <button type="button" class="btn btn-sm" onclick={regenerate} disabled={chat.busy}>Retry</button>
            {#if chat.info?.admin}
              <a class="btn btn-sm btn-ghost" href="#/settings/addons">Assistant settings</a>
              {#if chat.info?.models && chat.info.models.length > 1}<span class="meta">or pick another model above.</span>{/if}
            {:else}
              <span class="meta">If it keeps failing, ask an admin.</span>
            {/if}
          </div>
        </div>
      {:else if it.kind === 'note'}
        <p class="meta note">{it.text}</p>
      {/if}
    {/each}
    {#if chat.busy}
      <div class="meta status" role="status">
        {chat.status}… {Math.max(0, Math.round((now - chat.statusSince) / 1000))} s
      </div>
    {/if}
  </div>
  {#if newBelow}
    <button type="button" class="btn btn-sm new-below" onclick={toBottom}>New message ↓</button>
  {/if}
  <div class="sr" aria-live="polite">{announce}</div>

  {#if count >= 35 && !atLimit}
    <p class="meta limit">{MAX_MESSAGES - count} messages left – start a new chat for something else.</p>
  {/if}
  {#if atLimit}
    <div class="compose">
      <p class="meta">This chat is full.</p>
      <button type="button" class="btn btn-primary" onclick={reset}>New chat</button>
    </div>
  {:else}
    {#if chat.editing}
      <div class="editing"><span>Editing your message</span><button type="button" class="btn btn-ghost btn-sm" onclick={cancelEdit}>Cancel</button></div>
    {/if}
    {#if slashOptions.length}
      <div class="slash" role="listbox" aria-label="Commands">
        {#each slashOptions as c, k (c.name)}
          <button type="button" role="option" aria-selected={k === slashSel} class:sel={k === slashSel}
            onclick={() => { chat.input = '/' + c.name + ' '; chat.focusTick++; }}>
            <span class="mono">/{c.name}</span> <span class="meta">{c.label}</span>
          </button>
        {/each}
      </div>
    {/if}
    <form class="compose" onsubmit={(e) => { e.preventDefault(); void send(); }}>
      <textarea
        bind:this={box}
        class="input"
        rows="2"
        bind:value={chat.input}
        onkeydown={onKey}
        placeholder="Ask… (Enter to send, / for commands)"
        aria-label="Message"
      ></textarea>
      {#if chat.busy}
        <button type="button" class="btn" onclick={stop} title="Stop (Esc)">Stop</button>
      {:else}
        <button type="submit" class="btn btn-primary" disabled={!chat.input.trim() || bytes > MAX_MESSAGE_BYTES}>Send</button>
      {/if}
    </form>
    {#if bytes > 4000}
      <p class="meta counter" class:over={bytes > MAX_MESSAGE_BYTES}>{(bytes / 1024).toFixed(1)} / 32 KiB</p>
    {/if}
  {/if}
</div>

<style>
  .chat {
    display: grid;
    grid-template-rows: 1fr auto;
    min-height: 0;
    height: 100%;
    position: relative;
    gap: 6px;
  }
  .log {
    overflow: auto;
    display: grid;
    align-content: start;
    gap: 12px;
    min-height: 0;
    padding-right: 4px;
  }
  .empty {
    display: grid;
    gap: 10px;
    padding: 8px 0;
  }
  .sugg {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .small {
    font-size: 12px;
  }
  .row-user {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 2px;
  }
  .bubble {
    padding: 8px 10px;
    border-radius: var(--r-md);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .user {
    background: var(--info-wash);
    max-width: 90%;
  }
  .paste {
    max-height: 240px;
    overflow: auto;
    font-size: 12px;
    white-space: pre-wrap;
  }
  .answer {
    display: grid;
    gap: 6px;
    min-width: 0;
  }
  .faded {
    opacity: 0.45;
  }
  .thinking summary {
    cursor: pointer;
    display: flex;
    gap: 8px;
    min-width: 0;
  }
  .think-live {
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
    opacity: 0.8;
  }
  .think-text {
    margin: 4px 0 0;
    white-space: pre-wrap;
    font-size: 12.5px;
    color: var(--mist, inherit);
    max-height: 240px;
    overflow: auto;
  }
  .reads {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    align-items: center;
  }
  .read-chip {
    border: 0;
    border-radius: 999px;
    padding: 1px 8px;
    font: inherit;
    font-size: 12px;
    background: var(--well);
    color: inherit;
    cursor: pointer;
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .read-chip.failed {
    text-decoration: line-through;
  }
  .read-box {
    flex-basis: 100%;
    display: grid;
    gap: 4px;
  }
  .read-args,
  .read-result {
    margin: 0;
    max-height: 320px;
    overflow: auto;
    font-size: 11.5px;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    padding: 6px 8px;
    border-radius: var(--r-sm);
    background: var(--well);
  }
  .cursor {
    display: inline-block;
    width: 7px;
    height: 14px;
    background: currentColor;
    opacity: 0.6;
    animation: blink 1s steps(2) infinite;
  }
  @media (prefers-reduced-motion: reduce) {
    .cursor {
      animation: none;
    }
  }
  @keyframes blink {
    50% {
      opacity: 0;
    }
  }
  .open-row {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .alts summary {
    cursor: pointer;
  }
  .answer-acts {
    display: flex;
    gap: 4px;
  }
  .card-act {
    display: grid;
    gap: 6px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--info);
  }
  .card-act.self {
    box-shadow: inset 0 0 0 1px var(--signal);
  }
  .act-head {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: baseline;
  }
  .widen {
    display: flex;
    gap: 8px;
    padding: 8px;
    border-radius: var(--r-sm);
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .widen ul {
    margin: 4px 0 0;
    padding-left: 18px;
  }
  .diff {
    font-size: 12px;
    max-height: 200px;
    overflow: auto;
    white-space: pre;
    background: var(--well);
    border-radius: var(--r-sm);
    padding: 4px 0;
  }
  .diff.open {
    max-height: none;
  }
  .diff div {
    padding: 0 8px;
  }
  .diff .add {
    background: var(--afloat-wash);
    color: var(--afloat-text);
  }
  .diff .del {
    background: var(--adrift-wash);
    color: var(--adrift-text);
  }
  .acts {
    display: flex;
    gap: 6px;
  }
  .wrap {
    flex-wrap: wrap;
  }
  .err {
    color: var(--adrift-text);
    background: var(--adrift-wash);
    display: grid;
    gap: 6px;
    white-space: normal;
  }
  .err pre {
    white-space: pre-wrap;
    font-size: 11.5px;
    margin: 4px 0 0;
  }
  .note {
    font-style: italic;
  }
  .status {
    font-size: 12.5px;
  }
  .new-below {
    position: absolute;
    bottom: 70px;
    left: 50%;
    transform: translateX(-50%);
  }
  .limit {
    font-size: 12px;
  }
  .editing {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 2px 8px;
    border-radius: var(--r-sm);
    background: var(--info-wash);
    font-size: 12.5px;
  }
  .slash {
    display: grid;
    border-radius: var(--r-md);
    box-shadow: var(--lift-raised);
    background: var(--well);
    overflow: hidden;
  }
  .slash button {
    text-align: left;
    border: 0;
    background: transparent;
    color: inherit;
    padding: 6px 10px;
    font: inherit;
    cursor: pointer;
  }
  .slash button.sel,
  .slash button:hover {
    background: var(--rope-strong);
  }
  .compose {
    display: flex;
    gap: 6px;
    align-items: end;
  }
  .input {
    flex: 1;
    resize: none;
    font: 400 14px/20px var(--font);
    padding: 8px 10px;
    border: 0;
    border-radius: var(--r-md);
    background: var(--well);
    color: inherit;
    box-shadow: inset 0 0 0 1px var(--rope-strong);
    min-height: 56px;
  }
  .input:focus-visible {
    outline: 2px solid var(--buoy);
    outline-offset: 1px;
  }
  .counter {
    font-size: 11.5px;
    text-align: right;
  }
  .counter.over {
    color: var(--adrift-text);
  }
  .sr {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
  }
</style>
