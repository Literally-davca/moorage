<script lang="ts">
  import Icon from '../lib/Icon.svelte';
  import AssistantChat from '../lib/AssistantChat.svelte';
  import ModelPicker from '../lib/ModelPicker.svelte';
  import { chat, reset, loadStatus, loadCommands, open } from '../lib/assistantStore.svelte';
  import { api } from '../lib/api';

  // #/assistant (M19E): the same chat as the panel, on a whole page for longer work.
  // Below the chat: the user's saved commands (M19I).

  $effect(() => {
    void loadStatus();
    void loadCommands();
    open();
  });

  let name = $state('');
  let text = $state('');
  let err = $state('');
  async function saveCmd() {
    err = '';
    try {
      chat.commands = (await api.saveAssistantCommand(name.trim().toLowerCase(), text)).commands;
      name = text = '';
    } catch (e) {
      err = (e as Error).message;
    }
  }
  async function delCmd(n: string) {
    try {
      await api.deleteAssistantCommand(n);
      chat.commands = chat.commands.filter((c) => c.name !== n);
    } catch (e) {
      err = (e as Error).message;
    }
  }
</script>

<section class="page">
  <header class="head">
    <h1><Icon name="wand" /> Assistant</h1>
    <ModelPicker />
    <button type="button" class="btn btn-ghost btn-sm" onclick={reset} disabled={chat.items.length === 0}>New chat</button>
    <button type="button" class="btn btn-ghost btn-sm" onclick={() => history.back()}><Icon name="minimize" /> Back to the panel</button>
  </header>
  {#if chat.info && !chat.info.enabled}
    <p class="meta">The assistant add-on is off. An admin turns it on in <a href="#/settings/addons">Settings → Add-ons</a>.</p>
  {:else}
    <div class="card card-pad chatbox"><AssistantChat /></div>
    <details class="card card-pad cmds">
      <summary><strong>Your commands</strong> <span class="meta">({chat.commands.length} / 50) – run with /name</span></summary>
      <p class="meta">Built in: <code>/why</code>, <code>/logs</code>, <code>/explain</code>, <code>/link</code>. Your own commands can use {'{stack}'}, {'{host}'} and {'{container}'} from the page. A command is only the text of a message: it changes no permission.</p>
      {#each chat.commands as c (c.name)}
        <div class="cmd">
          <code>/{c.name}</code>
          <span class="meta grow">{c.text}</span>
          <button type="button" class="btn btn-ghost btn-sm" onclick={() => { name = c.name; text = c.text; }}>Edit</button>
          <button type="button" class="btn btn-ghost btn-sm" onclick={() => delCmd(c.name)} aria-label="Delete /{c.name}"><Icon name="trash" /></button>
        </div>
      {/each}
      <form class="cmd-form" onsubmit={(e) => { e.preventDefault(); void saveCmd(); }}>
        <input class="field" placeholder="name (e.g. disk)" bind:value={name} aria-label="Command name" maxlength="32" />
        <textarea class="field" rows="2" placeholder="Text, e.g. Is {'{host}'} running out of disk space?" bind:value={text} aria-label="Command text" maxlength="2000"></textarea>
        <button type="submit" class="btn btn-primary" disabled={!name.trim() || !text.trim()}>Save</button>
      </form>
      {#if err}<p class="error-text">{err}</p>{/if}
    </details>
  {/if}
</section>

<style>
  .page {
    display: grid;
    gap: 12px;
    max-width: 900px;
    margin: 0 auto;
    width: 100%;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .head h1 {
    display: inline-flex;
    gap: 8px;
    align-items: center;
    font-size: 20px;
    margin-right: auto;
  }
  .chatbox {
    height: calc(100vh - 220px);
    min-height: 420px;
  }
  .cmds summary {
    cursor: pointer;
  }
  .cmds {
    display: grid;
    gap: 8px;
  }
  .cmd {
    display: flex;
    gap: 8px;
    align-items: center;
    margin-top: 6px;
  }
  .grow {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .cmd-form {
    display: grid;
    grid-template-columns: 160px 1fr auto;
    gap: 6px;
    margin-top: 8px;
    align-items: start;
  }
  @media (max-width: 640px) {
    .cmd-form {
      grid-template-columns: 1fr;
    }
  }
</style>
