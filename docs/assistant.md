# AI assistant (add-on)

A chat panel (Ctrl+J or the wand icon in the top bar) that knows where things are in Moorage, reads your stacks and their Problems card, gives exact links and steps, and — at the levels you allow — proposes or makes changes.

## Setup

**Settings → Add-ons → AI assistant → Enable → Configure.**

- **Provider:** Anthropic (Claude, default model `claude-opus-5-5` through the official Go SDK, with the server-side refusal fallback), OpenAI, Google Gemini, Ollama (local) or any OpenAI-compatible endpoint (OpenRouter, Groq, Mistral, DeepSeek, LM Studio, vLLM, LocalAI…). Models without function calling can only advise.
- **API key:** stored like other Moorage secrets, write-only, never returned to the browser. Nothing is sent to a provider until someone writes a message.
- **Usage:** tokens per month are counted; an optional monthly limit stops the assistant when it is reached.

## The chat

The conversation lives only in your browser tab: closing or reloading the page ends it, and the server stores nothing. It is limited to 40 messages; then start a new chat. Links are built only from Moorage's own route map and the address you have Moorage open at.

## What it may do

Per kind of action: **Off**, **Advise** (explains and links), **Propose** (default: an action card with a diff and **Apply**), or **Auto** (does it and shows the result).

| Kind | Tool |
| --- | --- |
| Start / restart / stop / pull a stack | `stack_action` (never `down` or delete, never Moorage's own stack) |
| Compose edits & deploy | `propose_compose` – a new version with a diff; Auto needs a ticked confirmation |
| New stacks | `create_stack` – new secrets only through `@generate(...)` |

When the assistant read logs or other outside content in the same answer, an Auto action becomes a proposal (prompt-injection guard). A proposal is refused if the stack changed in the meantime. Every action is in the audit log as `<user> via assistant` and in the Logbook.

**Never possible** (the tools don't exist): deleting stacks, volumes or snapshots, `down`, terminals, users, roles, tokens, sign-in and SSO, self-update, restoring backups or snapshots, disk cleanup, reading or changing existing `.env` values. The assistant gives the steps and the link instead.

Viewers get only the reading tools. Read-only hosts have changes turned off.

## What it may read

Always: hosts, stacks, containers, the Problems card, `compose.yaml` with `.env` values masked (keys visible), host addresses. **Container logs**: *Ask in chat* by default (an Allow button appears; at most 200 lines; `.env` values and secret patterns are masked on the server before anything is sent), or *Never* / *Allowed*.

The system prompt is in [`internal/assistant/system_prompt.md`](../internal/assistant/system_prompt.md); its static part is cached, the session context (version, origin, role, page, hosts, Problems of the open stack) follows it.
