<script lang="ts">
  import type { Snippet } from 'svelte';
  let {
    open = $bindable(false),
    title,
    body,
    confirmLabel,
    danger = false,
    onConfirm,
    children,
  }: {
    open: boolean;
    title: string;
    body: string;
    confirmLabel: string;
    danger?: boolean;
    onConfirm: () => void;
    children?: Snippet;
  } = $props();

  let dlg: HTMLDialogElement;
  $effect(() => {
    if (open && !dlg.open) dlg.showModal();
    if (!open && dlg.open) dlg.close();
  });
</script>

<dialog bind:this={dlg} onclose={() => (open = false)} aria-labelledby="confirm-title">
  <div class="box">
    <h2 id="confirm-title">{title}</h2>
    <p>{body}</p>
    {@render children?.()}
    <div class="row">
      <button class="btn" onclick={() => (open = false)}>Cancel</button>
      <button class="btn {danger ? 'btn-danger' : 'btn-primary'}" onclick={() => { open = false; onConfirm(); }}>{confirmLabel}</button>
    </div>
  </div>
</dialog>

<style>
  dialog {
    width: min(440px, calc(100vw - 32px));
  }
  .box {
    padding: 22px 24px 20px;
    display: grid;
    gap: 10px;
  }
  h2 {
    font-size: 16px;
    font-weight: 600;
  }
  p {
    color: var(--mist);
  }
  .row {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    margin-top: 10px;
  }
</style>
