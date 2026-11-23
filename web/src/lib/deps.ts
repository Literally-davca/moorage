// Helpers for the stack dependency graph (R5): node status text, edge labels, layout.
import { statusLabel, type DepDetection, type DepHost, type DepNode, type StackDep, type StackRef } from './api';

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

export interface HostBox {
  host: DepHost;
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface HostLayoutOpts {
  nodeW: number;
  nodeH: number;
  gapX: number;
  gapY: number;
  boxPad?: number;
  boxHead?: number;
  boxGap?: number;
  maxRowWidth?: number;
}

/**
 * Groups linked nodes inside visual device containers (Hub / Agents) and arranges the
 * device containers in a wrapping grid.
 */
export function layoutByHost(
  nodes: DepNode[],
  edges: StackDep[],
  hosts: DepHost[],
  opts: HostLayoutOpts,
): { placed: Placed[]; boxes: HostBox[]; width: number; height: number } {
  const boxPad = opts.boxPad ?? 16;
  const boxHead = opts.boxHead ?? 36;
  const boxGap = opts.boxGap ?? 24;
  const maxRowWidth = opts.maxRowWidth ?? 960;

  const nodesByHost = new Map<string, DepNode[]>();
  for (const n of nodes) {
    const list = nodesByHost.get(n.hostId) ?? [];
    list.push(n);
    nodesByHost.set(n.hostId, list);
  }

  const hostMap = new Map(hosts.map((h) => [h.id, h]));
  for (const [hid, list] of nodesByHost) {
    if (!hostMap.has(hid)) {
      hostMap.set(hid, {
        id: hid,
        name: list[0]?.hostName || hid,
        kind: hid === 'local' ? 'local' : 'agent',
        connected: false,
      });
    }
  }

  const orderedHosts = [...hostMap.values()]
    .filter((h) => nodesByHost.has(h.id))
    .sort((a, b) => {
      if (a.kind !== b.kind) return a.kind === 'local' ? -1 : 1;
      return a.name.localeCompare(b.name);
    });

  const subLayouts = orderedHosts.map((host) => {
    const hNodes = nodesByHost.get(host.id) ?? [];
    const hEdges = edges.filter((e) => e.from.hostId === host.id && e.to.hostId === host.id);
    const sub = layout(hNodes, hEdges, opts.nodeW, opts.nodeH, opts.gapX, opts.gapY);
    const boxW = Math.max(sub.width + boxPad * 2, opts.nodeW + boxPad * 2, 220);
    const boxH = sub.height + boxHead + boxPad * 2;
    return { host, sub, boxW, boxH };
  });

  const boxes: HostBox[] = [];
  const placed: Placed[] = [];
  let curX = 0;
  let curY = 0;
  let rowH = 0;
  let totalW = 0;
  let totalH = 0;
  let rowCount = 0;

  for (const item of subLayouts) {
    if (rowCount > 0 && (curX + item.boxW > maxRowWidth || rowCount >= 3)) {
      curX = 0;
      curY += rowH + boxGap;
      rowH = 0;
      rowCount = 0;
    }
    boxes.push({
      host: item.host,
      x: curX,
      y: curY,
      width: item.boxW,
      height: item.boxH,
    });
    for (const p of item.sub.placed) {
      placed.push({
        node: p.node,
        x: curX + boxPad + p.x,
        y: curY + boxHead + boxPad + p.y,
      });
    }
    totalW = Math.max(totalW, curX + item.boxW);
    totalH = Math.max(totalH, curY + item.boxH);
    curX += item.boxW + boxGap;
    rowH = Math.max(rowH, item.boxH);
    rowCount++;
  }

  return { placed, boxes, width: totalW, height: totalH };
}
