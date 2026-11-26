// Shared wording for checks.
import type { Check, CheckInput, CheckKind, CheckStatus, LocationResult, Quorum } from './api';

export const kindName: Record<CheckKind, string> = {
  http: 'HTTP',
  tcp: 'TCP',
  ping: 'Ping',
  dns: 'DNS',
  stack: 'Stack',
  log: 'Log',
};

export const checkStatusLabel: Record<CheckStatus, string> = {
  up: 'Up',
  down: 'Down',
  degraded: 'Partially down',
  pending: 'Failing, not alerted yet',
  paused: 'Paused',
  unknown: 'Waiting for the first result',
  maintenance: 'Maintenance',
};

/** pct formats an uptime share (0–1) like 99.95 %. */
export function pct(v: number | null | undefined): string {
  if (v === null || v === undefined) return '–';
  const p = v * 100;
  if (p === 100) return '100 %';
  return `${p >= 99 ? p.toFixed(2) : p.toFixed(1)} %`;
}

export function emptyCheck(): CheckInput {
  return {
    name: '',
    kind: 'http',
    target: '',
    hostId: '',
    stack: '',
    interval: 60,
    timeout: 10,
    retries: 2,
    settings: { threshold: 1, window: 300, status: '' },
    enabled: true,
    channels: [],
    locations: [],
    quorum: '',
  };
}

/** Kinds that can run from several locations (the hub and agents). */
export const remoteKinds: CheckKind[] = ['http', 'tcp', 'ping', 'dns'];

export const quorumText: Record<Quorum, string> = {
  majority: 'Most locations fail',
  any: 'Any location fails',
  all: 'Every location fails',
};

/** multiLocation mirrors the server: anything other than "only the hub". */
export function multiLocation(c: { kind: CheckKind; locations?: string[] }): boolean {
  const l = c.locations ?? [];
  return remoteKinds.includes(c.kind) && l.length > 0 && !(l.length === 1 && l[0] === 'hub');
}

export function voteText(r: LocationResult): string {
  return r.vote === 'up' ? 'Up' : r.vote === 'down' ? 'Down' : 'No answer';
}

/** statusText is the label of a check's state; unknown after a run means nobody could check. */
export function statusText(c: Check): string {
  if (c.state.status === 'unknown' && c.state.last) return 'Unknown, no location could check';
  return checkStatusLabel[c.state.status];
}

export function ago(iso?: string): string {
  if (!iso) return '';
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

export function duration(fromIso: string, toIso?: string): string {
  const s = Math.max(0, ((toIso ? Date.parse(toIso) : Date.now()) - Date.parse(fromIso)) / 1000);
  if (s < 60) return `${Math.round(s)} s`;
  if (s < 3600) return `${Math.round(s / 60)} min`;
  const h = Math.floor(s / 3600);
  return `${h} h ${Math.round((s % 3600) / 60)} min`;
}

/** Common HTTP status codes and ranges offered as chips. */
export const commonStatusCodes = ['200', '201', '204', '200-299', '301', '302', '300-399', '401', '403', '404'];

/** validStatusToken mirrors the server: a code 100–599 or a range a-b within it. */
export function validStatusToken(t: string): boolean {
  const m = /^(\d{3})(?:-(\d{3}))?$/.exec(t.trim());
  if (!m) return false;
  const a = Number(m[1]);
  const b = m[2] ? Number(m[2]) : a;
  return a >= 100 && a <= 599 && b >= a && b <= 599;
}

/** splitStatus turns "200,204, 300-399" into chips. */
export function splitStatus(spec?: string): string[] {
  return (spec ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
}

/** targetCount is how many URLs or stacks a check watches. */
export function targetCount(c: { kind: CheckKind; settings: { urls?: string[]; stacks?: unknown[] } }): number {
  if (c.kind === 'http') return Math.max(1, c.settings.urls?.length ?? 0);
  if (c.kind === 'stack') return Math.max(1, c.settings.stacks?.length ?? 0);
  return 1;
}

/** toInput keeps only the editable fields of a check. */
export function toInput(c: Check): CheckInput {
  const settings = { ...c.settings };
  if (settings.urls) settings.urls = [...settings.urls];
  if (settings.stacks) settings.stacks = settings.stacks.map((s) => ({ ...s }));
  settings.status ??= '';
  if (c.kind === 'log') {
    settings.threshold ??= 1;
    settings.window ??= 300;
  }
  return {
    name: c.name,
    kind: c.kind,
    target: c.target,
    hostId: c.hostId,
    stack: c.stack,
    interval: c.interval,
    timeout: c.timeout,
    retries: c.retries,
    settings,
    enabled: c.enabled,
    channels: [...c.channels],
    locations: [...(c.locations ?? [])],
    quorum: c.quorum ?? '',
  };
}

/** Labels of a target's status in the per-target breakdown of a multi-target check. */
export const targetStatus: Record<string, string> = {
  up: 'Up',
  down: 'Down',
  maintenance: 'In maintenance (skipped)',
  silenced: 'Just deployed (skipped)',
  paused: 'Stopped on purpose',
  unknown: 'Could not check',
};
