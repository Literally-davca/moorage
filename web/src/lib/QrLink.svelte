<script lang="ts">
  import qrcode from 'qrcode-generator';
  import { dismiss } from './dismiss';
  import Icon from './Icon.svelte';

  // QR code of a service link (M19J), to open it on a phone. Generated in the browser,
  // nothing goes over the network. Hidden for localhost/127.0.0.1: a phone can't open it.
  let { url }: { url: string } = $props();

  let open = $state(false);
  const usable = $derived.by(() => {
    try {
      const h = new URL(url).hostname;
      return !(h === 'localhost' || h.endsWith('.localhost') || /^127\./.test(h) || h === '[::1]' || h === '::1');
    } catch {
      return false;
    }
  });
  const img = $derived.by(() => {
    if (!open) return '';
    try {
      const q = qrcode(0, 'M');
      q.addData(url);
      q.make();
      return q.createDataURL(4, 2);
    } catch {
      return '';
    }
  });
</script>

{#if usable}
  <span class="qr" use:dismiss={() => (open = false)}>
    <button type="button" class="btn btn-ghost btn-sm btn-icon" aria-label="QR code of {url}" aria-expanded={open} title="QR code – open on a phone" onclick={() => (open = !open)}>
      <Icon name="qr" />
    </button>
    {#if open}
      <span class="pop card" role="dialog" aria-label="QR code">
        {#if img}<img src={img} alt="QR code of {url}" width="164" height="164" />{/if}
        <span class="mono addr">{url}</span>
      </span>
    {/if}
  </span>
{/if}

<style>
  .qr {
    position: relative;
    display: inline-flex;
  }
  .pop {
    position: absolute;
    z-index: 70;
    top: calc(100% + 4px);
    right: 0;
    display: grid;
    gap: 6px;
    justify-items: center;
    padding: 10px;
    width: 196px;
    background: var(--surface, var(--well));
    box-shadow: var(--lift-raised);
  }
  .pop img {
    image-rendering: pixelated;
    background: #fff;
    border-radius: var(--r-sm);
  }
  .addr {
    font-size: 11px;
    overflow-wrap: anywhere;
    text-align: center;
  }
</style>
