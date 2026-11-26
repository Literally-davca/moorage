<script lang="ts">
  import { api, type AssistantAction, type AssistantEvent } from './api';
  import { app } from './state.svelte';
  import Icon from './Icon.svelte';

  // The assistant panel (M18G): a one-off chat that lives only in this page. Nothing is
  // stored on the server; closing or reloading the page ends the conversation.
  let { open = $bindable(false) }: { open: boolean } = $props();

  type Item =
    | { kind: 'user'; text: string }
    | { kind: 'assistant'; text: string; incomplete?: boolean }
    | { kind: 'action'; action: AssistantAction; state: 'pending' | 'applied' | 'rejected' | 'done'; error?: string }
    | { kind: 'permission'; what: string; granted: boolean }
    | { kind: 'error'; text: string }
    | { kind: 'tool'; name: string };

  let items = $state<Item[]>([]);
  let input = $state('');
  let busy = $state(false);
  let allow = $state<string[]>([]);
  let tokens = $state(0);
  let ctrl: AbortController | null = null;

  const toolLabel: Record<string, string> = {
    list_stacks: 'Looking at stacks…',
    get_stack: 'Reading the stack…',
    get_logs: 'Reading logs…',
    stack_action: 'Preparing a stack action…',
    propose_compose: 'Preparing a compose change…',
    create_stack: 'Preparing a new stack…',
  };

  function history() {
    const out: { role: string; text: string }[] = [];
    for (const it of items) {
      if (it.kind === 'user') out.push({ role: 'user', text: it.text });
      else if (it.kind === 'assistant' && it.text) {
        const last = out[out.length - 1];
        if (last?.role === 'assistant') last.text += '\n' + it.text;
        else out.push({ role: 'assistant', text: it.text });
      } else if (it.kind === 'action') {
        const note = `[action ${it.action.title}: ${it.state}]`;
        const last = out[out.length - 1];
        if (last?.role === 'assistant') last.text += '\n' + note;
        else out.push({ role: 'assistant', text: note });
      }
    }
    return out;
  }

  function pageContext() {
    const r = app.route as { page: string; name?: string };
    return {
      origin: location.origin,
      page: location.hash,
      hostId: app.host,
      stack: r.page === 'stack' ? (r.name ?? '') : '',
    };
  }

  async function send(text?: string) {
    const msg = (text ?? input).trim();
    if (!msg || busy) return;
    if (text === undefined) input = '';
    items.push({ kind: 'user', text: msg });
    await run();
  }

  async function run() {
    busy = true;
    ctrl = new AbortController();
    let current: { kind: 'assistant'; text: string; incomplete?: boolean } | null = null;
    try {
      const res = await fetch('/api/assistant/chat', {
        method: 'POST',
        headers: { 'X-Moorage': '1', 'Content-Type': 'application/json' },
        body: JSON.stringify({ messages: history(), context: pageContext(), allow }),
        signal: ctrl.signal,
        credentials: 'same-origin',
      });
      if (!res.ok || !res.body) {
        const data = await res.json().catch(() => ({}));
        items.push({ kind: 'error', text: data.error ?? `Request failed (${res.status}).` });
        return;
      }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      let finished = false;
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const chunk = buf.slice(0, i);
          buf = buf.slice(i + 2);
          if (!chunk.startsWith('data: ')) continue;
          const ev = JSON.parse(chunk.slice(6)) as AssistantEvent;
          switch (ev.type) {
            case 'text':
              current = { kind: 'assistant', text: ev.text ?? '', incomplete: ev.incomplete };
              items.push(current);
              break;
            case 'tool':
              items.push({ kind: 'tool', name: ev.tool ?? '' });
              break;
            case 'action':
              if (ev.action) items.push({ kind: 'action', action: ev.action, state: ev.action.done ? 'done' : 'pending' });
              break;
            case 'permission':
              if (ev.permission && !allow.includes(ev.permission)) items.push({ kind: 'permission', what: ev.permission, granted: false });
              break;
            case 'error':
              items.push({ kind: 'error', text: ev.text ?? 'Error' });
              break;
            case 'done':
              finished = true;
              tokens += (ev.inTokens ?? 0) + (ev.outTokens ?? 0);
              break;
          }
        }
      }
      if (!finished) items.push({ kind: 'error', text: 'The answer is incomplete (the connection ended).' });
    } catch (e) {
      if ((e as Error).name !== 'AbortError') items.push({ kind: 'error', text: (e as Error).message });
    } finally {
      busy = false;
      ctrl = null;
      // drop the transient "working" lines
      items = items.filter((it) => it.kind !== 'tool');
    }
  }

  async function grant(it: Item & { kind: 'permission' }) {
    it.granted = true;
    if (!allow.includes(it.what)) allow = [...allow, it.what];
    await run();
  }

  async function decide(it: Item & { kind: 'action' }, apply: boolean) {
    if (!it.action.draftId) return;
    it.error = undefined;
    try {
      if (apply) {
        await api.assistantApply(it.action.draftId);
        it.state = 'applied';
      } else {
        await api.assistantReject(it.action.draftId);
        it.state = 'rejected';
      }
    } catch (e) {
      it.error = (e as Error).message;
    }
  }

  function reset() {
    ctrl?.abort();
    items = [];
    allow = [];
    tokens = 0;
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  }

  // links in answers: only Moorage's own origin becomes clickable
  function linkify(text: string): { t: string; href?: string }[] {
    const out: { t: string; href?: string }[] = [];
    const re = /(https?:\/\/[^\s)`'"]+)/g;
    let last = 0;
    let m;
    while ((m = re.exec(text))) {
      if (m.index > last) out.push({ t: text.slice(last, m.index) });
      const url = m[1];
      out.push(url.startsWith(location.origin) ? { t: url, href: url } : { t: url });
      last = m.index + url.length;
    }
    if (last < text.length) out.push({ t: text.slice(last) });
    return out;
  }
</script>

{#if open}
  <aside class="assistant card" aria-label="Assistant">
    <header class="ahead">
      <strong><Icon name="wand" /> Assistant</strong>
      <span class="meta">{tokens ? `${tokens.toLocaleString()} tokens` : ''}</span>
      <button type="button" class="btn btn-ghost btn-sm" onclick={reset} title="New chat" disabled={items.length === 0}>New chat</button>
      <button type="button" class="btn btn-ghost btn-sm" onclick={() => (open = false)} aria-label="Close assistant"><Icon name="x" /></button>
    </header>
    <div class="log" aria-live="polite">
      {#if items.length === 0}
        <p class="meta">Ask about your stacks, why something fails, or where a setting is. The chat isn't saved.</p>
      {/if}
      {#each items as it, i (i)}
        {#if it.kind === 'user'}
          <div class="bubble user">{it.text}</div>
        {:else if it.kind === 'assistant'}
          <div class="bubble bot">
            {#each linkify(it.text) as part, j (j)}{#if part.href}<a href={part.href}>{part.t}</a>{:else}{part.t}{/if}{/each}
            {#if it.incomplete}<span class="meta"> (incomplete)</span>{/if}
          </div>
        {:else if it.kind === 'tool'}
          <div class="meta working">{toolLabel[it.name] ?? 'Working…'}</div>
        {:else if it.kind === 'permission'}
          <div class="perm">
            <span>The assistant wants to read <strong>{it.what}</strong> (secrets are masked).</span>
            {#if !it.granted}<button type="button" class="btn btn-sm btn-primary" onclick={() => grant(it)} disabled={busy}>Allow for this chat</button>{/if}
          </div>
        {:else if it.kind === 'action'}
          <div class="action">
            <div><strong>{it.action.title}</strong>{#if it.action.demoted}<span class="meta"> – proposed instead of done, because outside content was read</span>{/if}</div>
            {#if it.action.diff}<pre class="diff mono">{it.action.diff}</pre>{/if}
            {#if it.state === 'pending' && it.action.draftId}
              <div class="acts">
                <button type="button" class="btn btn-sm btn-primary" onclick={() => decide(it, true)}>Apply</button>
                <button type="button" class="btn btn-sm btn-ghost" onclick={() => decide(it, false)}>Reject</button>
              </div>
            {:else}
              <span class="meta">{it.state === 'done' ? 'Done by the assistant.' : it.state === 'applied' ? 'Applied.' : 'Rejected.'} {it.action.undo ?? ''}</span>
            {/if}
            {#if it.error}<p class="error-text">{it.error}</p>{/if}
          </div>
        {:else if it.kind === 'error'}
          <div class="bubble err" role="alert">
            {it.text}
            {#if app.user?.role === 'admin'}<a href="#/settings/addons"> Assistant settings</a>{/if}
          </div>
        {/if}
      {/each}
      {#if busy}<div class="meta working">Thinking…</div>{/if}
    </div>
    <form class="compose" onsubmit={(e) => { e.preventDefault(); send(); }}>
      <textarea class="field" rows="2" bind:value={input} onkeydown={onKey} placeholder="Ask… (Enter to send)" aria-label="Message"></textarea>
      {#if busy}
        <button type="button" class="btn" onclick={() => ctrl?.abort()}>Stop</button>
      {:else}
        <button type="submit" class="btn btn-primary" disabled={!input.trim()}>Send</button>
      {/if}
    </form>
  </aside>
{/if}

<style>
  .assistant {
    position: fixed;
    right: 12px;
    top: 72px;
    bottom: 12px;
    width: min(420px, calc(100vw - 24px));
    z-index: 60;
    display: grid;
    grid-template-rows: auto 1fr auto;
    gap: 8px;
    padding: 10px;
  }
  .ahead {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .ahead strong {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin-right: auto;
  }
  .log {
    overflow: auto;
    display: grid;
    align-content: start;
    gap: 8px;
  }
  .bubble {
    padding: 8px 10px;
    border-radius: var(--r-md);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .user {
    background: var(--info-wash);
    justify-self: end;
    max-width: 90%;
  }
  .bot {
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .err {
    color: var(--danger);
    background: var(--well);
  }
  .action,
  .perm {
    display: grid;
    gap: 6px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--info);
  }
  .diff {
    margin: 0;
    font-size: 12px;
    max-height: 200px;
    overflow: auto;
    white-space: pre;
  }
  .acts {
    display: flex;
    gap: 6px;
  }
  .compose {
    display: flex;
    gap: 6px;
    align-items: end;
  }
  .compose textarea {
    flex: 1;
    resize: vertical;
  }
</style>
