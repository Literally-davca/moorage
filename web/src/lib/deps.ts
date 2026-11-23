// Helpers for the stack dependency graph (R5): node status text, edge labels, layout.
import { statusLabel, type DepDetection, type DepNode, type StackDep, type StackRef } from './api';

export const refKey = (r: StackRef) => `${r.hostId}/${r.stack}`;

/** Status class (colour) of a node: checks that are down win over the container status. */
export function nodeClass(n: DepNode | undefined): string {
  if (!n) return 'unknown';
  if (n.checks === 'down' || n.status === 'down') return 'down';
  if (n.checks === 'pending' || n.status === 'degraded') return 'degraded';
  if (n.checks === 'maintenance') return 'maintenance';
  if (n.status === 'up') return 'up';
  return 'unknown';
}

/** Status text of a node; colour is never the only signal. */
export function nodeText(n: DepNode | undefined): string {
  if (!n) return 'Unknown';
  const base = n.status ? statusLabel[n.status] : 'Unknown';
  if (n.checks === 'down') return n.status === 'down' ? 'Down · check down' : 'Check down';
  if (n.checks === 'pending') return `${base} · check failing`;
  if (n.checks === 'maintenance') return `${base} · maintenance`;
  return base;
}

/** Short description of how an edge came about. */
export function edgeText(e: StackDep): string {
  if (e.source === 'manual') return e.createdBy ? `manual · added by ${e.createdBy}` : 'manual';
  const how = e.reason === 'hostname' ? 'detected (hostname)' : e.reason === 'network' ? 'detected (network)' : 'detected';
  return e.ignored ? `${how} · ignored` : how;
}

export function edgeKind(e: StackDep): 'manual' | 'detected' | 'ignored' {
  return e.ignored ? 'ignored' : e.source;
}

export function detectionText(d: DepDetection | undefined): string {
  if (!d) return '';
  if (d.running) return 'Detecting dependencies…';
  if (d.unsupported) return "This host's agent is too old to detect dependencies. Update it; dependencies you add by hand work anyway.";
  if (d.error) return `Detection failed: ${d.error}.`;
  if (!d.at) return 'Dependencies are detected from the compose files shortly after start and every 10 minutes.';
  const mins = Math.max(0, Math.round((Date.now() - new Date(d.at).getTime()) / 60000));
  const when = mins < 1 ? 'just now' : `${mins} min ago`;
  let t = `Detected ${when} from ${d.stacks} compose ${d.stacks === 1 ? 'file' : 'files'}.`;
  if (d.broken?.length) t += ` Couldn't read ${d.broken.join(', ')}; kept their earlier dependencies.`;
  return t;
}

export interface Placed {
  node: DepNode;
  x: number;
  y: number;
}

/**
 * Layered layout: stacks that depend on nothing in view sit in the top row, each
 * dependent one row below its lowest dependency. Ignored edges and loops don't shape the
 * layers (the walk is bounded so a loop can't spin). Within a row, nodes follow the
 * average position of their dependencies to keep edges short.
 */
export function layout(nodes: DepNode[], edges: StackDep[], w: number, h: number, gapX: number, gapY: number) {
  const byKey = new Map(nodes.map((n) => [refKey(n), n]));
  const ups = new Map<string, string[]>();
  for (const e of edges) {
    if (e.ignored) continue;
    const f = refKey(e.from);
    const t = refKey(e.to);
    if (!byKey.has(f) || !byKey.has(t) || f === t) continue;
    ups.set(f, [...(ups.get(f) ?? []), t]);
  }
  const layer = new Map<string, number>();
  for (const k of byKey.keys()) layer.set(k, 0);
  // longest path, at most nodes.length passes (loops just stop growing)
  for (let pass = 0; pass < nodes.length; pass++) {
    let changed = false;
    for (const [f, ts] of ups) {
      for (const t of ts) {
        const want = (layer.get(t) ?? 0) + 1;
        if ((layer.get(f) ?? 0) < want && want < nodes.length) {
          layer.set(f, want);
          changed = true;
        }
      }
    }
    if (!changed) break;
  }
  const rows: string[][] = [];
  for (const [k, l] of layer) (rows[l] ??= []).push(k);
  const xOf = new Map<string, number>();
  const placed: Placed[] = [];
  let width = 0;
  rows.forEach((row, i) => {
    if (!row) return;
    const score = (k: string) => {
      const ts = (ups.get(k) ?? []).filter((t) => xOf.has(t));
      return ts.length ? ts.reduce((s, t) => s + (xOf.get(t) ?? 0), 0) / ts.length : Number.MAX_SAFE_INTEGER;
    };
    row.sort((a, b) => score(a) - score(b) || a.localeCompare(b));
    row.forEach((k, j) => {
      const x = j * (w + gapX);
      xOf.set(k, x);
      placed.push({ node: byKey.get(k)!, x, y: i * (h + gapY) });
      width = Math.max(width, x + w);
    });
  });
  const height = rows.length ? rows.length * (h + gapY) - gapY : 0;
  return { placed, width, height };
}
