// Shared state of the assistant chat (M19E): the docked panel and the #/assistant page
// show the same conversation, so moving between pages or expanding the panel keeps it.
// Nothing is stored on the server; a reload ends the chat.

import { api, type AssistantAction, type AssistantEvent, type AssistantLink, type AssistantStatus, type AssistantCommand } from './api';
import { app, currentHost } from './state.svelte';
import { currentNotices, type PageNotice } from './pageNotices.svelte';

export interface ReadEntry {
  tool: string;
  target: string;
  args?: unknown;
  result?: string;
  failed?: boolean;
  foreign: boolean;
  trimmed?: boolean;
}

export type ChatItem =
  | { kind: 'user'; text: string }
  | {
      kind: 'assistant';
      text: string;
      streaming: boolean;
      incomplete?: boolean;
      stopped?: boolean;
      thinking: string;
      thinkingMs: number;
      reads: ReadEntry[];
      links: AssistantLink[];
      notices: PageNotice[];
      model?: string;
      tokens?: number;
    }
  | { kind: 'action'; action: AssistantAction; state: 'pending' | 'applied' | 'rejected' | 'done' | 'stale'; error?: string; result?: string }
  | { kind: 'permission'; what: string; granted: boolean }
  | { kind: 'choice'; question: string; options: string[]; answered?: string }
  | { kind: 'error'; text: string; code?: string; detail?: string; model?: string }
  | { kind: 'note'; text: string };

type AssistantItem = Extract<ChatItem, { kind: 'assistant' }>;

/** Tools whose results quote container output (rendered as plain text, never Markdown). */
const foreignTools = new Set(['get_logs']);

export const MAX_MESSAGES = 40;
export const MAX_MESSAGE_BYTES = 32 * 1024;

export const chat = $state({
  items: [] as ChatItem[],
  busy: false,
  status: '' as string, // "Reading stack immich…"
  statusSince: 0,
  allow: [] as string[],
  input: '',
  editing: false,
  open: false,
  expanded: false,
  tokens: 0,
  info: null as AssistantStatus | null,
  pick: { provider: '', model: '' },
  commands: [] as AssistantCommand[],
  focusTick: 0,
});

let ctrl: AbortController | null = null;

export async function loadStatus(models = false) {
  try {
    chat.info = await api.assistantStatus(models);
  } catch {
    chat.info = null;
  }
}

export async function loadCommands() {
  try {
    chat.commands = (await api.assistantCommands()).commands;
  } catch {
    chat.commands = [];
  }
}

function lastIndex(kind: ChatItem['kind']): number {
  for (let i = chat.items.length - 1; i >= 0; i--) if (chat.items[i].kind === kind) return i;
  return -1;
}

export function userMessageCount(): number {
  return chat.items.filter((i) => i.kind === 'user').length;
}

function history() {
  const out: { role: string; text: string }[] = [];
  const push = (role: string, text: string) => {
    const last = out[out.length - 1];
    if (last?.role === role) last.text += '\n' + text;
    else out.push({ role, text });
  };
  for (const it of chat.items) {
    if (it.kind === 'user') push('user', it.text);
    else if (it.kind === 'assistant' && it.text) push('assistant', it.text);
    else if (it.kind === 'action') push('assistant', `[action ${it.action.title}: ${it.state}]`);
    else if (it.kind === 'choice') push('assistant', `[asked: ${it.question} – options: ${it.options.join(' / ')}]`);
  }
  return out;
}

function pageContext() {
  const r = app.route as { page: string; name?: string };
  return { origin: location.origin, page: location.hash, hostId: app.host, stack: r.page === 'stack' ? (r.name ?? '') : '' };
}

/** expand fills {stack}, {host} and {container} from the page. */
export function expandVars(text: string): string {
  const r = app.route as { page: string; name?: string };
  const stack = r.page === 'stack' ? (r.name ?? '') : '';
  return text
    .replaceAll('{stack}', stack || 'the stack on this page')
    .replaceAll('{host}', currentHost()?.name ?? app.host)
    .replaceAll('{container}', stack ? stack + ' (its containers)' : 'the container on this page');
}

export const builtinCommands: { name: string; label: string; text: () => string }[] = [
  {
    name: 'why',
    label: 'Why does it fail?',
    text: () => {
      const r = app.route as { page: string; name?: string };
      return r.page === 'stack' ? `Why is ${r.name} not working, and how do I fix it? Check its Problems first.` : 'Which stacks need attention and why?';
    },
  },
  { name: 'logs', label: 'Summarize recent errors in the logs', text: () => expandVars('Summarize the recent errors in the logs of {stack}.') },
  { name: 'explain', label: 'Explain what is on my screen', text: () => 'Explain the notices and errors on my screen and how to fix them.' },
  { name: 'link', label: 'Where does it run?', text: () => expandVars('Where does {stack} run? Give me its link.') },
];

/** resolveSlash turns "/name rest" into the command's text; null when not a command. */
export function resolveSlash(input: string): string | null {
  const m = /^\/([a-z0-9-]+)\s*(.*)$/s.exec(input.trim());
  if (!m) return null;
  const extra = m[2] ? '\n' + m[2] : '';
  const b = builtinCommands.find((c) => c.name === m[1]);
  if (b) return b.text() + extra;
  const c = chat.commands.find((c) => c.name === m[1]);
  return c ? expandVars(c.text) + extra : null;
}

export function open(focus = true) {
  chat.open = true;
  if (focus) chat.focusTick++;
}

/** ask opens the panel with a prefilled question; the user sends it (M19I). */
export function ask(question: string) {
  chat.input = question;
  chat.editing = false;
  open();
}

export async function send(text?: string) {
  let msg = (text ?? chat.input).trim();
  if (!msg || chat.busy) return;
  const slash = msg.startsWith('/') ? resolveSlash(msg) : null;
  if (slash) msg = slash;
  if (chat.editing) {
    // Edit: the last question and everything after it are replaced
    const i = lastIndex('user');
    if (i >= 0) {
      await dropFrom(i);
    }
    chat.editing = false;
  }
  if (text === undefined) chat.input = '';
  // a pending choice card answered by typing
  for (const it of chat.items) if (it.kind === 'choice' && !it.answered) it.answered = '(answered in the chat)';
  chat.items.push({ kind: 'user', text: msg });
  await run();
}

/** dropFrom removes items from index i on; pending proposals among them are rejected. */
async function dropFrom(i: number) {
  const removed = chat.items.slice(i);
  chat.items = chat.items.slice(0, i);
  for (const it of removed) {
    if (it.kind === 'action' && it.state === 'pending' && it.action.draftId) {
      try {
        await api.assistantReject(it.action.draftId);
      } catch {
        /* already decided */
      }
    }
  }
}

/** changedSince: an answer after the last question already changed something. */
export function lastAnswerChanged(): boolean {
  const i = lastIndex('user');
  return chat.items.slice(i + 1).some((it) => it.kind === 'action' && (it.state === 'done' || it.state === 'applied'));
}

export async function regenerate() {
  if (chat.busy || lastAnswerChanged()) return;
  const i = lastIndex('user');
  if (i < 0) return;
  await dropFrom(i + 1);
  await run();
}

export function editLast() {
  if (chat.busy || lastAnswerChanged()) return;
  const i = lastIndex('user');
  if (i < 0) return;
  chat.input = (chat.items[i] as { text: string }).text;
  chat.editing = true;
  chat.focusTick++;
}

export function cancelEdit() {
  chat.editing = false;
  chat.input = '';
}

export function stop() {
  ctrl?.abort();
}

export function reset() {
  ctrl?.abort();
  for (const it of chat.items) {
    if (it.kind === 'action' && it.state === 'pending' && it.action.draftId) void api.assistantReject(it.action.draftId).catch(() => {});
  }
  chat.items = [];
  chat.allow = [];
  chat.tokens = 0;
  chat.input = '';
  chat.editing = false;
  chat.focusTick++;
}

export async function grant(what: string) {
  if (!chat.allow.includes(what)) chat.allow = [...chat.allow, what];
  for (const it of chat.items) if (it.kind === 'permission' && it.what === what) it.granted = true;
  // ask the last question again: the "I need permission" answer is replaced
  const i = lastIndex('user');
  if (i >= 0 && !chat.busy) {
    await dropFrom(i + 1);
    await run();
  }
}

export function revoke(what: string) {
  chat.allow = chat.allow.filter((a) => a !== what);
}

export async function answerChoice(it: ChatItem & { kind: 'choice' }, option: string) {
  if (it.answered || chat.busy) return;
  it.answered = option;
  chat.items.push({ kind: 'user', text: option });
  await run();
}

export async function decide(it: ChatItem & { kind: 'action' }, apply: boolean): Promise<{ selfApply?: boolean; ownHub?: boolean } | void> {
  if (!it.action.draftId) return;
  it.error = undefined;
  try {
    if (apply) {
      const res = await api.assistantApply(it.action.draftId);
      it.state = 'applied';
      return res;
    }
    await api.assistantReject(it.action.draftId);
    it.state = 'rejected';
  } catch (e) {
    const msg = (e as Error).message;
    if (/changed since this was proposed/i.test(msg)) it.state = 'stale';
    it.error = msg;
  }
}

/** M20E: results of earlier answers keep their first 4 KiB; the model got them whole. */
function compactOlder() {
  for (const it of chat.items) {
    if (it.kind !== 'assistant') continue;
    for (const r of it.reads) {
      if (r.result && r.result.length > 4096 && !r.trimmed) {
        r.result = r.result.slice(0, 4096) + `\n…(${Math.ceil((r.result.length - 4096) / 1024)} KiB more, shown to the model)`;
        r.trimmed = true;
      }
    }
  }
}

async function run() {
  compactOlder();
  chat.busy = true;
  chat.status = 'Thinking';
  chat.statusSince = Date.now();
  ctrl = new AbortController();
  const notices = currentNotices();
  const answer: AssistantItem = { kind: 'assistant', text: '', streaming: true, thinking: '', thinkingMs: 0, reads: [], links: [], notices };
  chat.items.push(answer);
  // the reactive proxy of the pushed item
  const cur = chat.items[chat.items.length - 1] as AssistantItem;
  if (notices.length) cur.reads.push({ tool: 'screen', target: `screen notices (${notices.length})`, result: notices.map((n) => `[${n.severity}] ${n.title}: ${n.text}`).join('\n'), foreign: notices.some((n) => n.foreign) });
  let thinkStart = 0;
  let finished = false;
  // M20E: deltas are merged into at most ~15 writes a second; every write re-renders
  let thinking = '';
  let pendingText: string | null = null;
  let flushTimer = 0;
  const flush = () => {
    flushTimer = 0;
    if (pendingText !== null) cur.text = pendingText;
    pendingText = null;
    if (thinking !== cur.thinking) {
      cur.thinking = thinking;
      cur.thinkingMs = Date.now() - thinkStart;
    }
  };
  const later = () => {
    if (!flushTimer) flushTimer = window.setTimeout(flush, 66);
  };
  const body: Record<string, unknown> = { messages: history(), context: pageContext(), allow: chat.allow, notices };
  if (chat.info?.admin && chat.pick.provider) {
    body.provider = chat.pick.provider;
    body.model = chat.pick.model;
  }
  try {
    const res = await fetch('/api/assistant/chat', {
      method: 'POST',
      headers: { 'X-Moorage': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: ctrl.signal,
      credentials: 'same-origin',
    });
    if (!res.ok || !res.body) {
      const data = await res.json().catch(() => ({}));
      chat.items.push({ kind: 'error', text: data.error ?? `Request failed (${res.status}).` });
      return;
    }
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    let segment = ''; // text of the current model step
    let base = ''; // text of the finished steps
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
          case 'model':
            cur.model = ev.model;
            if (ev.text) chat.items.splice(chat.items.length - 1, 0, { kind: 'note', text: ev.text });
            break;
          case 'thinking':
            if (!thinkStart) thinkStart = Date.now();
            thinking += ev.text ?? '';
            later();
            if (chat.status !== 'Thinking') chat.status = 'Thinking';
            break;
          case 'delta':
            segment += ev.text ?? '';
            pendingText = joinText(base, segment);
            later();
            if (chat.status !== 'Writing') chat.status = 'Writing';
            break;
          case 'text':
            // final text of a step: replaces the streamed part (or arrives whole)
            base = joinText(base, ev.text ?? '');
            pendingText = base;
            flush();
            segment = '';
            if (ev.incomplete) cur.incomplete = true;
            break;
          case 'tool':
            chat.status = ev.label || 'Working';
            if (ev.target) chat.status += ': ' + ev.target;
            chat.statusSince = Date.now();
            break;
          case 'tool_done':
            cur.reads.push({ tool: ev.tool ?? '', target: ev.target || ev.tool || '', args: ev.args, result: ev.result, failed: ev.failed, foreign: foreignTools.has(ev.tool ?? '') });
            if (ev.links?.length) cur.links.push(...ev.links);
            chat.status = 'Thinking';
            break;
          case 'action':
            if (ev.action) chat.items.push({ kind: 'action', action: ev.action, state: ev.action.done ? 'done' : 'pending' });
            break;
          case 'permission':
            if (ev.permission && !chat.allow.includes(ev.permission) && !chat.items.some((x) => x.kind === 'permission' && x.what === ev.permission && !x.granted))
              chat.items.push({ kind: 'permission', what: ev.permission, granted: false });
            break;
          case 'choice':
            if (ev.choice) chat.items.push({ kind: 'choice', question: ev.choice.question, options: ev.choice.options });
            break;
          case 'error':
            chat.items.push({ kind: 'error', text: ev.text ?? 'Error', code: ev.code, detail: ev.detail, model: ev.model });
            break;
          case 'done':
            finished = true;
            cur.tokens = (ev.inTokens ?? 0) + (ev.outTokens ?? 0);
            chat.tokens += cur.tokens;
            break;
        }
      }
    }
    if (!finished) cur.incomplete = true;
  } catch (e) {
    if ((e as Error).name === 'AbortError') cur.stopped = true;
    else chat.items.push({ kind: 'error', text: (e as Error).message, code: 'unreachable' });
  } finally {
    clearTimeout(flushTimer);
    flush();
    // M20E: only the end of long thinking stays in memory
    if (cur.thinking.length > 8192) cur.thinking = '…' + cur.thinking.slice(-8192);
    cur.streaming = false;
    if (!cur.text && !cur.reads.length && !cur.thinking && !cur.stopped) {
      const idx = chat.items.indexOf(cur);
      if (idx >= 0) chat.items.splice(idx, 1);
    }
    chat.busy = false;
    chat.status = '';
    ctrl = null;
    chat.focusTick++;
  }
}

function joinText(base: string, add: string): string {
  if (!add) return base;
  if (!base) return add;
  return base + '\n\n' + add;
}
