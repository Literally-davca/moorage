// Rich notifications (M17B): content presets, rich / plain format, logo base URL and the
// preview of a channel's message.
import { request, type ChannelConfig, type ChannelKind } from './api';

export type NotifyPreset = 'minimal' | 'standard' | 'detailed' | 'custom';
export type NotifyPart = 'host' | 'stack' | 'cause' | 'duration' | 'logs' | 'image' | 'checkDetails' | 'link' | 'timestamps';

export const presets: { k: NotifyPreset; label: string; hint: string }[] = [
  { k: 'minimal', label: 'Minimal', hint: 'Status, title, host, link' },
  { k: 'standard', label: 'Standard', hint: '+ stack, cause, duration, logs' },
  { k: 'detailed', label: 'Detailed', hint: '+ image change, check details, times' },
  { k: 'custom', label: 'Custom', hint: 'Pick the parts' },
];

export const parts: { k: NotifyPart; label: string }[] = [
  { k: 'host', label: 'Host' },
  { k: 'stack', label: 'Stack' },
  { k: 'cause', label: 'Cause' },
  { k: 'duration', label: 'Duration' },
  { k: 'logs', label: 'Last log lines' },
  { k: 'image', label: 'Image change' },
  { k: 'checkDetails', label: 'Check details' },
  { k: 'link', label: 'Link to Moorage' },
  { k: 'timestamps', label: 'Start and end time' },
];

/** Parts a preset shows (mirrors notify.Config.Parts). */
export function presetParts(p: NotifyPreset | undefined): Record<NotifyPart, boolean> {
  const all = (on: NotifyPart[]) => Object.fromEntries(parts.map(({ k }) => [k, on.includes(k)])) as Record<NotifyPart, boolean>;
  switch (p) {
    case 'minimal':
      return all(['host', 'link']);
    case 'detailed':
      return all(parts.map((x) => x.k));
    default:
      return all(['host', 'stack', 'cause', 'duration', 'logs', 'link']);
  }
}

const richKinds = new Set<ChannelKind>(['discord', 'slack', 'telegram', 'matrix', 'gotify', 'ntfy', 'pushover', 'smtp']);
export const supportsRich = (k: ChannelKind) => richKinds.has(k);

export const richLabel: Partial<Record<ChannelKind, string>> = {
  discord: 'Embed',
  slack: 'Attachment with fields',
  telegram: 'Formatted (HTML)',
  matrix: 'Formatted (HTML)',
  gotify: 'Markdown',
  ntfy: 'Markdown',
  pushover: 'Formatted (HTML)',
  smtp: 'HTML e-mail',
};

export interface NotifyField {
  name: string;
  value: string;
  inline?: boolean;
}

export interface NotifyView {
  status: string;
  event?: string;
  sender: string;
  senderHost?: string;
  senderRole: string;
  iconUrl: string;
  title: string;
  description?: string;
  fields?: NotifyField[];
  logs?: string[];
  url?: string;
  timestamp?: string;
  footer: string;
  preset: NotifyPreset;
}

export interface NotifySettings {
  iconBase: string;
  defaultIconBase: string;
  richKinds: string[];
  partKeys: string[];
  maxLogLines: number;
}

export const notifications = {
  settings: () => request<NotifySettings>('GET', '/api/notifications/settings'),
  saveSettings: (iconBase: string) => request<NotifySettings>('PUT', '/api/notifications/settings', { iconBase }),
  preview: (kind: ChannelKind, config: ChannelConfig) =>
    request<{ view: NotifyView; payload?: string; rich: boolean }>('POST', '/api/channels/preview', { kind, config }),
};
