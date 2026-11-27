// What the user sees (M19I): banners, form and lint errors, recent error toasts and the
// last Apply / deploy result register here as structured text. The assistant gets them
// with the question; no screenshot or HTML ever leaves the page.

export interface PageNotice {
  id: string;
  severity: 'error' | 'warning' | 'info';
  title: string;
  text: string;
  hostId?: string;
  stack?: string;
  /** Suggested fix shown with the notice (for example the mount line to add). */
  fix?: string;
  /** The text quotes container output (logs): treated like logs (Auto → Propose). */
  foreign?: boolean;
}

const registry = $state<{ list: PageNotice[] }>({ list: [] });
const recent: { at: number; n: PageNotice }[] = [];

/** registerNotice adds a notice while it is on screen; call the result to remove it. */
export function registerNotice(n: PageNotice): () => void {
  registry.list = [...registry.list.filter((x) => x.id !== n.id), n];
  return () => {
    registry.list = registry.list.filter((x) => x.id !== n.id);
  };
}

/** A Svelte action: `use:notice={{ id, severity, title, text }}` registers while mounted. */
export function notice(_node: HTMLElement, n: PageNotice | null) {
  let off = n ? registerNotice(n) : () => {};
  return {
    update(next: PageNotice | null) {
      off();
      off = next ? registerNotice(next) : () => {};
    },
    destroy() {
      off();
    },
  };
}

/** noteToast keeps an error toast for 5 minutes. */
export function noteToast(n: PageNotice) {
  recent.push({ at: Date.now(), n });
  while (recent.length > 10) recent.shift();
}

/** currentNotices: what the user sees now (at most 20), errors first. */
export function currentNotices(): PageNotice[] {
  const cutoff = Date.now() - 5 * 60 * 1000;
  const toasts = recent.filter((r) => r.at >= cutoff).map((r) => r.n);
  const rank = { error: 0, warning: 1, info: 2 };
  return [...registry.list, ...toasts].sort((a, b) => rank[a.severity] - rank[b.severity]).slice(0, 20);
}

export function noticeCount(): number {
  return registry.list.length;
}
