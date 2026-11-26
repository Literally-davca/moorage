<script lang="ts">
  // Logo base URL of notifications (M17B): Discord, Slack, ntfy and webhooks show the
  // Moorage logo of the sender's role from <base>/hub.png, agent.png and watchdog.png.
  import { untrack } from 'svelte';
  import Icon from '../lib/Icon.svelte';
  import { notifications, type NotifySettings } from '../lib/notifications';

  let settings = $state<NotifySettings | null>(null);
  let base = $state('');
  let saving = $state(false);
  let msg = $state<{ ok: boolean; text: string } | null>(null);

  async function load() {
    try {
      settings = await notifications.settings();
      base = settings.iconBase;
    } catch {
      /* optional */
    }
  }
  $effect(() => {
    untrack(load);
  });

  const effective = $derived((base.trim() || settings?.defaultIconBase || '').replace(/\/+$/, ''));

  async function save(e: SubmitEvent) {
    e.preventDefault();
    saving = true;
    msg = null;
    try {
      settings = await notifications.saveSettings(base.trim());
      base = settings.iconBase;
      msg = { ok: true, text: 'Saved. New alerts use these logos.' };
    } catch (err) {
      msg = { ok: false, text: (err as Error).message };
    } finally {
      saving = false;
    }
  }
</script>

{#if settings}
  <section class="card card-pad brand">
    <h2 class="section-title">Sender logo</h2>
    <p class="meta">
      Alerts come from “<span class="mono">&lt;host&gt; • Moorage agent</span>”, “… hub” or “… watchdog” with the Moorage logo of that role.
      Point to your own copies of <span class="mono">hub.png</span>, <span class="mono">agent.png</span> and <span class="mono">watchdog.png</span> (PNG, 256 × 256) if the default CDN isn't reachable for your services.
    </p>
    <form class="row" onsubmit={save}>
      <input class="field mono" bind:value={base} placeholder={settings.defaultIconBase} aria-label="Logo base URL" />
      <button class="btn" disabled={saving || base.trim() === settings.iconBase} aria-busy={saving}>Save</button>
    </form>
    <div class="logos" aria-label="Logo preview">
      {#each ['hub', 'agent', 'watchdog'] as role (role)}
        <figure>
          <img src={`${effective}/${role}.png`} alt={`${role} logo`} width="32" height="32" loading="lazy" referrerpolicy="no-referrer" />
          <figcaption class="meta">{role}</figcaption>
        </figure>
      {/each}
    </div>
    <p class="meta">If a logo can't be loaded, the service shows its default avatar; the alert is still delivered.</p>
    {#if msg}
      <div class="msg {msg.ok ? 'ok' : 'bad'}" role="status"><Icon name={msg.ok ? 'check' : 'alert'} /><span>{msg.text}</span></div>
    {/if}
  </section>
{/if}

<style>
  .brand {
    display: grid;
    gap: 10px;
  }
  .row {
    display: flex;
    gap: 8px;
  }
  .row .field {
    flex: 1;
    min-width: 0;
    height: 32px;
  }
  .logos {
    display: flex;
    gap: 16px;
  }
  figure {
    margin: 0;
    display: grid;
    justify-items: center;
    gap: 4px;
  }
  figure img {
    border-radius: 7px;
    background: var(--well);
  }
  .msg {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 13px;
  }
  .msg.ok {
    color: var(--afloat-text);
  }
  .msg.bad {
    color: var(--adrift-text);
  }
</style>
