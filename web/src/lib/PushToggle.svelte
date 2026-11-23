<script lang="ts">
  import { untrack } from 'svelte';
  import { api } from './api';

  // Web Push needs a service worker, a secure origin (https or localhost) and PushManager.
  const supported =
    typeof window !== 'undefined' && window.isSecureContext && 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;

  let on = $state(false);
  let busy = $state(false);
  let error = $state('');
  let note = $state('');

  function b64ToBytes(s: string): Uint8Array<ArrayBuffer> {
    const pad = '='.repeat((4 - (s.length % 4)) % 4);
    const bin = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
    const out = new Uint8Array(new ArrayBuffer(bin.length));
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  async function registration() {
    return navigator.serviceWorker.register('/sw.js', { scope: '/' });
  }

  async function load() {
    if (!supported) return;
    try {
      const reg = await navigator.serviceWorker.getRegistration('/');
      const sub = await reg?.pushManager.getSubscription();
      if (!sub) return;
      const k = await api.pushKey();
      on = k.endpoints.includes(sub.endpoint);
    } catch {
      /* stays off */
    }
  }

  $effect(() => {
    untrack(() => void load());
  });

  async function enable() {
    error = note = '';
    busy = true;
    try {
      const perm = await Notification.requestPermission();
      if (perm !== 'granted') {
        error = 'The browser blocked notifications for this site. Allow them in the site settings and try again.';
        return;
      }
      const k = await api.pushKey();
      const reg = await registration();
      await navigator.serviceWorker.ready;
      let sub = await reg.pushManager.getSubscription();
      const want = b64ToBytes(k.publicKey);
      const cur = sub?.options.applicationServerKey;
      if (sub && cur && new Uint8Array(cur).join() !== want.join()) {
        await sub.unsubscribe(); // subscribed with an older hub key
        sub = null;
      }
      sub ??= await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: want });
      const j = sub.toJSON();
      await api.pushSubscribe({ endpoint: sub.endpoint, keys: { p256dh: j.keys?.p256dh ?? '', auth: j.keys?.auth ?? '' } });
      on = true;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function disable() {
    error = note = '';
    busy = true;
    try {
      const reg = await navigator.serviceWorker.getRegistration('/');
      const sub = await reg?.pushManager.getSubscription();
      if (sub) {
        await api.pushUnsubscribe(sub.endpoint).catch(() => {});
        await sub.unsubscribe();
      }
      on = false;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  async function test() {
    error = note = '';
    busy = true;
    try {
      const r = await api.pushTest();
      note = r.sent === 1 ? 'Sent to 1 device.' : `Sent to ${r.sent} devices.`;
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

<section class="card card-pad grid1" aria-label="Push notifications">
  <p class="section-title">Push notifications on this device</p>
  {#if !supported}
    <p class="meta">
      This browser can't receive push notifications here. They need HTTPS (or localhost); on iPhone and iPad, add Moorage to the
      home screen first.
    </p>
  {:else}
    <p class="meta">
      Checks going down or back up, hosts going offline, failed stack operations and automatic rollbacks, even when Moorage isn't
      open.
    </p>
    <div class="row">
      {#if on}
        <button type="button" class="btn" disabled={busy} onclick={disable}>Turn off</button>
        <button type="button" class="btn btn-ghost" disabled={busy} onclick={test}>Send a test</button>
      {:else}
        <button type="button" class="btn" disabled={busy} onclick={enable}>{busy ? 'Turning on…' : 'Turn on'}</button>
      {/if}
    </div>
  {/if}
  {#if note}<p class="meta">{note}</p>{/if}
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
</section>

<style>
  .row {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }
</style>
