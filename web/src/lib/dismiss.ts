// dismiss closes a popup (menu, dropdown) when the user clicks or taps outside the
// element it is attached to, or presses Escape. Attach it to the wrapper that holds
// both the toggle button and the popup, so clicking the toggle still toggles.
//
//   <div class="more" use:dismiss={() => (open = false)}>…</div>
//
// The toggle is the first element with aria-expanded inside the wrapper. While it says
// aria-expanded="false" the popup is closed and Escape is left alone: with several
// popups on a page, only the open one reacts and takes focus back to its toggle.
export function dismiss(node: HTMLElement, close: () => void) {
  let onClose = close;
  const toggle = () => node.querySelector<HTMLElement>('[aria-expanded]');
  const isOpen = () => toggle()?.getAttribute('aria-expanded') !== 'false';
  // pointerdown, not click: a drag that starts inside and ends outside must not close,
  // and it closes before another control's click runs (one tap = one action).
  const onPointer = (e: PointerEvent) => {
    if (!node.contains(e.target as Node) && isOpen()) onClose();
  };
  const onKey = (e: KeyboardEvent) => {
    if (e.key !== 'Escape' || !isOpen()) return;
    onClose();
    // Return focus to the toggle so keyboard users don't lose their place, but only
    // when focus was inside this popup (an Escape elsewhere must not move focus here).
    if (node.contains(document.activeElement)) toggle()?.focus();
  };
  document.addEventListener('pointerdown', onPointer, true);
  document.addEventListener('keydown', onKey);
  return {
    update(next: () => void) {
      onClose = next;
    },
    destroy() {
      document.removeEventListener('pointerdown', onPointer, true);
      document.removeEventListener('keydown', onKey);
    },
  };
}

// menuKeys gives a role="menu" container arrow-key navigation between its menu items
// (Up/Down/Home/End), focusing the first item when the menu opens.
//
//   {#if open}<div role="menu" use:menuKeys>…</div>{/if}
export function menuKeys(node: HTMLElement) {
  const items = () =>
    Array.from(node.querySelectorAll<HTMLElement>('[role^="menuitem"]')).filter((el) => !el.hasAttribute('disabled'));
  // Focus after mount, so a click that opened the menu doesn't steal it back.
  queueMicrotask(() => items()[0]?.focus());
  const onKey = (e: KeyboardEvent) => {
    const list = items();
    if (!list.length) return;
    const i = list.indexOf(document.activeElement as HTMLElement);
    let next = -1;
    if (e.key === 'ArrowDown') next = (i + 1) % list.length;
    else if (e.key === 'ArrowUp') next = (i - 1 + list.length) % list.length;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = list.length - 1;
    if (next < 0) return;
    e.preventDefault();
    list[next].focus();
  };
  node.addEventListener('keydown', onKey);
  return {
    destroy() {
      node.removeEventListener('keydown', onKey);
    },
  };
}
