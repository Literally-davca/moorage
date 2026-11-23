// App-wide reactive state: session, hash route, theme.
import type { HostEntry, User } from './api';

export type SettingsTab = 'general' | 'hosts' | 'notifications' | 'audit' | 'users' | 'templates' | 'addons';
export type MonitoringTab = 'checks' | 'status-pages' | 'maintenance';

export type Route =
  | { page: 'home' }
  | { page: 'new' }
  | { page: 'deps' }
  | { page: 'templates' }
  | { page: 'template'; source: string; id: string }
  | { page: 'stack'; name: string; tab?: 'history'; at?: number }
  | { page: 'settings'; tab: SettingsTab }
  | { page: 'monitoring'; tab: MonitoringTab }
  | { page: 'check'; id: number }
  | { page: 'checkForm'; id: number | null; host?: string; stack?: string }
  | { page: 'statusPageForm'; id: number | null }
  | { page: 'updates' }
  | { page: 'account' }
  | { page: 'maintenanceForm'; id: number | null };

const globalPages = ['settings', 'monitoring', 'hosts', 'checks', 'status-pages', 'maintenance', 'updates', 'notifications', 'audit', 'template-sources', 'users', 'account'];

// Routes: #/h/<host>, #/h/<host>/new, #/h/<host>/stacks/<name>, #/settings/hosts.
// Older links without a host (#/stacks/<name>) open on the current host.
function parse(hash: string): { route: Route; host?: string } {
  let h = hash.replace(/^#\/?/, '');
  const [path, query = ''] = h.split('?');
  const q = new URLSearchParams(query);
  if (path === 'settings' || path === 'settings/hosts' || path === 'hosts') return { route: { page: 'settings', tab: 'hosts' } };
  if (path === 'settings/general') return { route: { page: 'settings', tab: 'general' } };
  if (path === 'settings/notifications' || path === 'notifications') return { route: { page: 'settings', tab: 'notifications' } };
  if (path === 'settings/audit' || path === 'audit') return { route: { page: 'settings', tab: 'audit' } };
  if (path === 'settings/users' || path === 'users') return { route: { page: 'settings', tab: 'users' } };
  if (path === 'settings/templates' || path === 'template-sources') return { route: { page: 'settings', tab: 'templates' } };
  if (path === 'settings/addons') return { route: { page: 'settings', tab: 'addons' } };
  if (path === 'monitoring' || path === 'monitoring/checks' || path === 'checks') return { route: { page: 'monitoring', tab: 'checks' } };
  if (path === 'monitoring/status-pages' || path === 'status-pages') return { route: { page: 'monitoring', tab: 'status-pages' } };
  if (path === 'monitoring/maintenance' || path === 'maintenance') return { route: { page: 'monitoring', tab: 'maintenance' } };
  if (path === 'updates') return { route: { page: 'updates' } };
  if (path === 'account') return { route: { page: 'account' } };
  if (path === 'maintenance/new') return { route: { page: 'maintenanceForm', id: null } };
  const mm = /^maintenance\/(\d+)$/.exec(path);
  if (mm) return { route: { page: 'maintenanceForm', id: Number(mm[1]) } };
  if (path === 'status-pages/new') return { route: { page: 'statusPageForm', id: null } };
  const sm = /^status-pages\/(\d+)(\/edit)?$/.exec(path);
  if (sm) return { route: { page: 'statusPageForm', id: Number(sm[1]) } };
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

// A hub self-update the UI follows. The restart overlay only appears once the hub job
// reaches "restarting" (an "Update all" first updates the agents, page still usable).
export interface HubRestartState {
  fromVersion: string;
  toVersion: string;
  label: string;
  startedAt: number;
  jobId: number;
}

export const app = $state<{
  user: User | null;
  route: Route;
  host: string;
  hosts: HostEntry[];
  theme: 'dark' | 'light';
  version: string;
  restarting: HubRestartState | null;
}>({
  user: null,
  route: first.route,
  host: first.host ?? savedHost(),
  hosts: [],
  theme: 'dark',
  version: '',
  restarting: null,
});

export function startHubRestart(fromVersion: string, toVersion: string, label: string, jobId: number) {
  app.restarting = { fromVersion, toVersion, label, startedAt: Date.now(), jobId };
}

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

/** The token of an invite or reset link (#/invite/<token>), or ''. The token stays in the
 * URL fragment, which browsers never send to the server or in a Referer. */
export function inviteToken(): string {
  const m = /^#\/invite\/([A-Za-z0-9]+)$/.exec(location.hash);
  return m ? m[1] : '';
}

/** Query parameters of the login route (#/login?sso=denied&local=1). */
export function loginParams(): URLSearchParams {
  const m = /^#\/login\?(.*)$/.exec(location.hash);
  return new URLSearchParams(m ? m[1] : '');
}

/** The host shown in the UI, once the host list has loaded. */
export const currentHost = (): HostEntry | undefined => app.hosts.find((h) => h.id === app.host);

/** Admins can change stacks unless the host's agent runs read-only. */
export const canEdit = () => isAdmin() && !currentHost()?.readOnly;
