// Host network (M17B): port map of a host and its named addresses (Ethernet / Wi-Fi /
// VPN / domain / custom). Kept apart from api.ts so the feature stays in one place.
import { request } from './api';

export type NetMode = 'native' | 'host' | 'bridge';
export type AddressKind = 'ethernet' | 'wifi' | 'vpn' | 'domain' | 'custom';

export interface HostAddress {
  id: string;
  label: string;
  kind: AddressKind;
  address: string;
  interface?: string;
  isDefault?: boolean;
  autoDetected?: boolean;
  /** From MOORAGE_HOST_IP on that host: always the default, can't be edited here. */
  locked?: boolean;
  /** A saved auto-detected default the host doesn't report right now. */
  missing?: boolean;
}

export interface AltUrl {
  label: string;
  kind: AddressKind;
  address: string;
  url: string;
}

export type PortSource = 'stack' | 'container' | 'compose' | 'host';

export interface PortEntry {
  port: number;
  proto: string;
  /** Bind address, empty = every interface. */
  ip?: string;
  target?: number;
  source: PortSource;
  stack?: string;
  service?: string;
  container?: string;
  /** running | reserved (declared by a stopped stack) | listening (host process) | exited … */
  state: string;
  note?: string;
}

export interface PortReport {
  ports: PortEntry[];
  netMode?: NetMode;
  /** The host's own listening sockets were read (native / host-network mode only). */
  native: boolean;
  hostNetwork?: string[];
}

export const addressKinds: { k: AddressKind; label: string }[] = [
  { k: 'ethernet', label: 'Ethernet' },
  { k: 'wifi', label: 'Wi-Fi' },
  { k: 'vpn', label: 'VPN' },
  { k: 'domain', label: 'Domain' },
  { k: 'custom', label: 'Custom' },
];

export function kindLabel(k: string): string {
  return addressKinds.find((a) => a.k === k)?.label ?? 'Custom';
}

export const hostnet = {
  ports: (hostId: string, fresh = false) =>
    request<PortReport>('GET', `/api/hosts/${encodeURIComponent(hostId)}/ports${fresh ? '?fresh=1' : ''}`),
  addresses: (hostId: string) =>
    request<{ addresses: HostAddress[]; default: string; netMode?: NetMode; connected: boolean }>(
      'GET',
      `/api/hosts/${encodeURIComponent(hostId)}/addresses`,
    ),
  saveAddresses: (hostId: string, addresses: HostAddress[]) =>
    request<void>('PUT', `/api/hosts/${encodeURIComponent(hostId)}/addresses`, { addresses }),
};

/** Who holds a port, for the Ports table. */
export function portOwner(p: PortEntry): string {
  if (p.source === 'host') return 'Host process';
  if (p.stack && p.container) return `${p.stack} / ${p.container}`;
  if (p.stack && p.service) return `${p.stack} / ${p.service}`;
  return p.stack || p.container || '–';
}

export function netModeNote(mode?: NetMode): string {
  switch (mode) {
    case 'native':
      return 'Moorage runs directly on this host: it sees the host network, its interfaces and every listening port.';
    case 'host':
      return 'Moorage runs with network_mode: host: it sees the host network, its interfaces and every listening port.';
    default:
      return 'Moorage runs in its own container network here, so it sees Docker ports only. Add the host’s addresses by hand, or run Moorage with network_mode: host to detect them.';
  }
}
