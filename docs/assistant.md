# AI assistant (add-on)

A chat panel that knows where things are in Moorage, reads your stacks and their Problems
card, finds where services run, gives exact links and steps, and – at the levels you
allow – proposes or makes changes. The conversation lives only in your browser: reloading
the page ends it, and the server stores nothing of it. A chat holds 40 messages.

## Setup

**Settings → Add-ons → AI assistant → Enable → Configure.**

- **Providers:** Anthropic (Claude, default model `claude-opus-5-5`, official Go SDK with
  the server-side refusal fallback), OpenAI, Google Gemini, Ollama or any OpenAI-compatible
  endpoint (OpenRouter, Groq, Mistral, DeepSeek, LM Studio, vLLM, LocalAI…). Add several
  – for example Ollama on a GPU PC and on a NAS plus a paid API – each with its own key.
- **Models:** Ollama and OpenAI-compatible endpoints offer every model they report
  (**Load models**; Ollama models without tool support are marked "advise only"). For
  Anthropic, OpenAI and Gemini tick the models people may use – they differ in price.
  **Test connection** sends a short question.
- **Default model:** used by everyone; admins can switch between the allowed models in the
  panel. The chat never switches provider by itself, so logs never move from a local model
  to the cloud unless you choose it.
- **API keys** are stored like other Moorage secrets, write-only, never returned to the
  browser. Nothing goes to a provider until someone sends a message.
- **Usage:** tokens per month are counted; an optional monthly limit stops the assistant
  when it is reached. The panel warns from 80 %.

Ollama in Docker: `localhost` inside the Moorage container is the container itself. Use the
host's address (`http://192.168.1.20:11434/v1`) or `http://host.docker.internal:11434/v1`.

## Opening it

- **Ctrl+J** opens and closes the panel, **Esc** closes it (while an answer is being
  written, Esc stops it), **Ctrl+Shift+O** starts a new chat, **Ctrl+K** has "Ask the
  assistant…".
- From 1280 px the panel is docked on the right and the page gets narrower; drag its left
  edge (360–640 px). Narrower windows show it over the page, phones full screen.
- **Expand** opens the same chat on its own page, `#/assistant`, where you also manage your
  saved commands.
- **Ask the assistant** on a problem (Problems card, Home → Problems), on a failed action
  and on banners fills in a question. **Explain this alert** on an incident (check page and the
  stack's Logbook) does the same,
  and the link in a DOWN notification opens the page with the question filled in. Nothing
  is sent until you press Enter.

## Writing

- Enter sends, Shift+Enter adds a line, ↑ in an empty field edits your last message.
- `/` opens the commands: `/why` (why the stack on this page fails), `/logs` (recent errors
  in its logs), `/explain` (the notices on your screen), `/link` (where it runs), plus your
  own saved commands with `{stack}`, `{host}` and `{container}` from the page (50 per user,
  on every device). A command is only the text of a message: it changes no permission.
- Long pasted text is folded in the chat; a message can be up to 32 KiB.

## Answers

- Text appears word by word. **Stop** keeps what arrived, **Regenerate** asks again,
  **Edit** changes your last question; neither is offered once the answer changed
  something.
- **Read:** lists what the assistant really looked at (from its tool calls), e.g.
  `stack immich · Problems · compose.yaml (values hidden) · 200 log lines of immich
  (masked)`. Click an item to see exactly what was sent to the model.
- **Thinking** of models that return it is a collapsed block above the answer – never
  stored, and it can be turned off in the settings.
- **Screen notices:** the banners, errors and warnings you see go with your question as
  text (never a screenshot). Text quoted from containers counts as outside content.
- Markdown is shown safely (no HTML, no images); code blocks have **Copy**. Moorage pages
  are clickable links. Links to services are clickable only when the assistant got them
  from Moorage's service links in this chat – never guessed, never from logs – and come
  with **Open <service>** buttons and a QR code.
- When the next step is your decision, the assistant asks with buttons.
- Errors say what happened in plain words (provider unreachable, key refused, rate limit,
  no credit, unknown model, timeout) with **Retry** and, for admins, the settings link.

## What it may do

Per kind of action: **Off**, **Advise** (explains and links), **Propose** (default: an
action card with a coloured diff, the host and stack, what applying does, and **Apply**),
or **Auto** (does it and shows the result).

| Kind | Tool |
| --- | --- |
| Start / restart / stop / pull a stack | `stack_action` (never `down` or delete) |
| Compose edits & deploy | `propose_compose` – a new version with a diff; Auto needs a ticked confirmation |
| New stacks | `create_stack` – new secrets only through `@generate(...)` |

After reading logs or other outside content in the same answer, an Auto action becomes a
proposal (prompt-injection guard). A proposal is refused if the stack changed in the
meantime ("Ask to redo it"). Every action is in the audit log as `<user> via assistant`
and in the Logbook.

**Moorage's own stack:** when a fix needs a change of Moorage's compose file (for example
mounting `/opt/dockge` so Moorage can manage dockge's stacks), the assistant proposes it as
a card with **Apply & restart** – never automatically, admins only, with a warning for
every new access to the host it gives Moorage. Apply & restart rolls back if Moorage doesn't
come back. The assistant never changes the Moorage image (updates go through Moorage
updates and respect `MOORAGE_SELF_UPDATE=false`).

**Never possible** (the tools don't exist): deleting stacks, volumes or snapshots, `down`,
terminals, users, roles, tokens, sign-in and SSO, updating Moorage, restoring backups or
snapshots, disk cleanup, reading or changing existing `.env` values.

Viewers get only the reading tools. Read-only hosts have changes turned off.

## What it may read

Always: hosts and their addresses, stacks, containers, the Problems card, `compose.yaml`
with `.env` values masked, service links, the port map and the notices on your screen.
**Container logs** – also the log lines quoted in Problems: *Ask in chat* by default (an
Allow button appears; at most 200 lines; `.env` values and secret patterns are masked on
the server), *Never* or *Allowed*.

The system prompt is in [`internal/assistant/system_prompt.md`](../internal/assistant/system_prompt.md);
its static part is cached, the session context (version, origin, role, page, hosts,
Problems of the open stack, screen notices) follows it.

## MCP

The MCP server add-on offers the same read data to external agents – `get_service_links`,
`get_ports`, `get_problems`, `list_incidents`, `list_updates`, `get_stack_stats` – plus
prompts ("Diagnose stack", "Add a service") and resources (`moorage://hosts/<id>`,
`moorage://hosts/<id>/stacks/<name>`), all filtered by the token's access list. Logs over
MCP are masked like for the assistant.

**Client config:** Settings → Add-ons → MCP server shows the whole config file for Claude
Code (`.mcp.json`), Claude Desktop (`claude_desktop_config.json`, through `npx mcp-remote`),
Cursor (`~/.cursor/mcp.json`) and VS Code (`.vscode/mcp.json`) with **Copy JSON** and
**Download**. Right after **Generate token** the token is filled in; later it is a
placeholder, because tokens are shown only once. The file holds the token – keep it out of git.
