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
    settings: { threshold: 1, window: 300 },
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

/** toInput keeps only the editable fields of a check. */
export function toInput(c: Check): CheckInput {
  const settings = { ...c.settings };
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
