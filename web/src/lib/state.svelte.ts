// App-wide reactive state: session, hash route, theme.
import type { HostEntry, User } from './api';

export type Route =
  | { page: 'home' }
  | { page: 'new' }
  | { page: 'deps' }
  | { page: 'templates' }
  | { page: 'template'; source: string; id: string }
  | { page: 'templateSources' }
  | { page: 'stack'; name: string; tab?: 'history'; at?: number }
  | { page: 'hosts' }
  | { page: 'checks' }
  | { page: 'check'; id: number }
  | { page: 'checkForm'; id: number | null; host?: string; stack?: string }
  | { page: 'statusPages' }
  | { page: 'statusPageForm'; id: number | null }
  | { page: 'updates' }
  | { page: 'notifications' }
  | { page: 'audit' }
  | { page: 'maintenance' }
  | { page: 'maintenanceForm'; id: number | null };

const globalPages = ['hosts', 'checks', 'status-pages', 'maintenance', 'updates', 'notifications', 'audit', 'template-sources'];

// Routes: #/h/<host>, #/h/<host>/new, #/h/<host>/stacks/<name>, #/hosts.
// Older links without a host (#/stacks/<name>) open on the current host.
function parse(hash: string): { route: Route; host?: string } {
  let h = hash.replace(/^#\/?/, '');
  const [path, query = ''] = h.split('?');
  const q = new URLSearchParams(query);
  if (path === 'hosts') return { route: { page: 'hosts' } };
  if (path === 'updates') return { route: { page: 'updates' } };
  if (path === 'notifications') return { route: { page: 'notifications' } };
  if (path === 'audit') return { route: { page: 'audit' } };
  if (path === 'template-sources') return { route: { page: 'templateSources' } };
  if (path === 'maintenance') return { route: { page: 'maintenance' } };
  if (path === 'maintenance/new') return { route: { page: 'maintenanceForm', id: null } };
  const mm = /^maintenance\/(\d+)$/.exec(path);
  if (mm) return { route: { page: 'maintenanceForm', id: Number(mm[1]) } };
  if (path === 'status-pages') return { route: { page: 'statusPages' } };
  if (path === 'status-pages/new') return { route: { page: 'statusPageForm', id: null } };
  const sm = /^status-pages\/(\d+)(\/edit)?$/.exec(path);
  if (sm) return { route: { page: 'statusPageForm', id: Number(sm[1]) } };
  if (path === 'checks') return { route: { page: 'checks' } };
  if (path === 'checks/new') {
    return { route: { page: 'checkForm', id: null, host: q.get('host') ?? undefined, stack: q.get('stack') ?? undefined } };
  }
  const cm = /^checks\/(\d+)(\/edit)?$/.exec(path);
  if (cm) return { route: cm[2] ? { page: 'checkForm', id: Number(cm[1]) } : { page: 'check', id: Number(cm[1]) } };
  let host: string | undefined;
  const hm = /^h\/([^/]+)\/?(.*)$/.exec(h);
  if (hm) {
    host = decodeURIComponent(hm[1]);
    h = hm[2];
  }
  if (h === 'new') return { route: { page: 'new' }, host };
  if (h === 'deps') return { route: { page: 'deps' }, host };
  if (h === 'templates') return { route: { page: 'templates' }, host };
  const tm = /^templates\/([^/?]+)\/([^/?]+)$/.exec(h);
  if (tm) return { route: { page: 'template', source: decodeURIComponent(tm[1]), id: decodeURIComponent(tm[2]) }, host };
  // #/h/<host>/stacks/<name>/history?at=<unix> opens the timeline at the 2 h before that time.
  const m = /^stacks\/([^/?]+)(\/history)?(?:\?.*)?$/.exec(h);
  if (m) {
    const at = Number(q.get('at'));
    const route: Route = { page: 'stack', name: decodeURIComponent(m[1]) };
    if (m[2]) route.tab = 'history';
    if (m[2] && Number.isInteger(at) && at > 0) route.at = at;
    return { route, host };
  }
  return { route: { page: 'home' }, host };
}

function savedHost(): string {
  try {
    return localStorage.getItem('moorage-host') || 'local';
  } catch {
    return 'local';
  }
}

const first = parse(location.hash);

export const app = $state<{
  user: User | null;
  route: Route;
  host: string;
  hosts: HostEntry[];
  theme: 'dark' | 'light';
  version: string;
}>({
  user: null,
  route: first.route,
  host: first.host ?? savedHost(),
  hosts: [],
  theme: 'dark',
  version: '',
});

window.addEventListener('hashchange', () => {
  const p = parse(location.hash);
  app.route = p.route;
  if (p.host && p.host !== app.host) setHost(p.host, false);
});

export function setHost(id: string, navigate = true) {
  app.host = id;
  try {
    localStorage.setItem('moorage-host', id);
  } catch {
    /* ignore */
  }
  if (navigate) go('');
}

/** link builds a hash URL on the current host; 'hosts' is global. */
export function link(path: string): string {
  if (globalPages.includes(path.split(/[/?]/)[0])) return '#/' + path;
  return '#/h/' + encodeURIComponent(app.host) + (path ? '/' + path : '');
}

export function go(path: string) {
  location.hash = link(path);
}

// Theme is a per-browser convenience; storage may be unavailable (private mode).
try {
  const t = localStorage.getItem('moorage-theme');
  if (t === 'light' || t === 'dark') app.theme = t;
} catch {
  /* ignore */
}

export function toggleTheme() {
  app.theme = app.theme === 'dark' ? 'light' : 'dark';
  try {
    localStorage.setItem('moorage-theme', app.theme);
  } catch {
    /* ignore */
  }
}

export const isAdmin = () => app.user?.role === 'admin';

/** The host shown in the UI, once the host list has loaded. */
export const currentHost = (): HostEntry | undefined => app.hosts.find((h) => h.id === app.host);

/** Admins can change stacks unless the host's agent runs read-only. */
export const canEdit = () => isAdmin() && !currentHost()?.readOnly;
