/**
 * Reconstructs Compose Spec indentation when pasted text arrives stripped of leading
 * whitespace (e.g. from web docs, chat widgets, or rich-text clipboards) or using tabs.
 *
 * Designed to be conservative: if any non-empty, non-comment line already has valid
 * leading spaces AND the snippet looks validly indented as YAML, we return it unchanged
 * so we never break hand-crafted formatting.
 */

import { parseDocument } from 'yaml';

/** Top-level Compose keys. */
const ROOT_KEYS = new Set([
  'name',
  'version',
  'services',
  'networks',
  'volumes',
  'configs',
  'secrets',
  'include',
  'x-moorage',
]);

/** Keys under a service (`services.<name>.*`, indent 4). */
const SERVICE_KEYS = new Set([
  'attach',
  'annotations',
  'blkio_config',
  'build',
  'cap_add',
  'cap_drop',
  'cgroup',
  'cgroup_parent',
  'command',
  'configs',
  'container_name',
  'cpu_count',
  'cpu_percent',
  'cpu_shares',
  'cpu_period',
  'cpu_quota',
  'cpu_rt_runtime',
  'cpu_rt_period',
  'cpus',
  'cpuset',
  'credential_spec',
  'depends_on',
  'deploy',
  'develop',
  'device_cgroup_rules',
  'devices',
  'dns',
  'dns_opt',
  'dns_search',
  'domainname',
  'entrypoint',
  'env_file',
  'environment',
  'expose',
  'extends',
  'external_links',
  'extra_hosts',
  'gpus',
  'group_add',
  'healthcheck',
  'hostname',
  'image',
  'init',
  'ipc',
  'isolation',
  'labels',
  'links',
  'logging',
  'mac_address',
  'mem_limit',
  'mem_reservation',
  'mem_swappiness',
  'memswap_limit',
  'network_mode',
  'networks',
  'oom_kill_disable',
  'oom_score_adj',
  'pid',
  'pids_limit',
  'platform',
  'ports',
  'post_start',
  'pre_stop',
  'privileged',
  'profiles',
  'pull_policy',
  'read_only',
  'restart',
  'runtime',
  'scale',
  'secrets',
  'security_opt',
  'shm_size',
  'stdin_open',
  'stop_grace_period',
  'stop_signal',
  'storage_opt',
  'sysctls',
  'tmpfs',
  'tty',
  'ulimits',
  'user',
  'userns_mode',
  'uts',
  'volumes',
  'volumes_from',
  'working_dir',
]);

/** Service keys whose value can be a nested mapping or sequence of items. */
const SERVICE_SECTION_KEYS = new Set([
  'annotations',
  'blkio_config',
  'build',
  'cap_add',
  'cap_drop',
  'command',
  'configs',
  'depends_on',
  'deploy',
  'develop',
  'devices',
  'dns',
  'dns_opt',
  'dns_search',
  'entrypoint',
  'env_file',
  'environment',
  'expose',
  'extra_hosts',
  'gpus',
  'group_add',
  'healthcheck',
  'labels',
  'links',
  'logging',
  'networks',
  'ports',
  'post_start',
  'pre_stop',
  'profiles',
  'secrets',
  'security_opt',
  'sysctls',
  'tmpfs',
  'ulimits',
  'volumes',
  'volumes_from',
]);

/** Keys under top-level `volumes.<name>.*`, `networks.<name>.*`, `configs.<name>.*`, `secrets.<name>.*`. */
const TOP_OBJ_KEYS = new Set([
  'attachable',
  'driver',
  'driver_opts',
  'enable_ipv6',
  'external',
  'file',
  'ipam',
  'internal',
  'labels',
  'name',
]);

/** Common sub-keys inside `healthcheck`, `deploy`, `build`, `logging`. */
const SUB_SECTION_KEYS = new Set([
  'test',
  'interval',
  'timeout',
  'retries',
  'start_period',
  'start_interval',
  'disable',
  'driver',
  'options',
  'context',
  'dockerfile',
  'args',
  'target',
  'replicas',
  'resources',
  'limits',
  'reservations',
  'restart_policy',
  'placement',
  'constraints',
  'condition',
  'max_attempts',
  'delay',
  'window',
  'memory',
  'cpus',
  'config',
  'subnet',
  'gateway',
]);

const SECTION_ROOT_CONTAINERS = new Set(['services', 'networks', 'volumes', 'configs', 'secrets']);

function extractKey(trimmed: string): string | null {
  if (!trimmed || trimmed.startsWith('#') || trimmed.startsWith('-')) return null;
  const idx = trimmed.indexOf(':');
  if (idx <= 0) return null;
  const after = trimmed.slice(idx + 1);
  if (after !== '' && !/^\s/.test(after)) return null;
  const raw = trimmed.slice(0, idx).trim().replace(/^["']|["']$/g, '');
  return raw || null;
}

function hasValueAfterColon(trimmed: string): boolean {
  const idx = trimmed.indexOf(':');
  if (idx < 0) return false;
  const rest = trimmed.slice(idx + 1).trim();
  return rest !== '' && !rest.startsWith('#');
}

/**
 * Returns true if the text appears to be an unindented or tab-indented Compose snippet
 * that would fail to parse or lose its hierarchy without indentation repair.
 */
export function needsComposeIndent(text: string): boolean {
  const lines = text.replace(/\r\n/g, '\n').split('\n');
  let nonEmpty = 0;
  let indented = 0;
  let hasTabs = false;
  let hasRoot = false;
  let hasServiceOrList = false;

  for (const line of lines) {
    const t = line.trim();
    if (!t || t.startsWith('#')) continue;
    nonEmpty++;
    if (/^\t+/.test(line)) hasTabs = true;
    if (/^[ ]+/.test(line)) indented++;
    const key = extractKey(t);
    if (key && ROOT_KEYS.has(key)) hasRoot = true;
    if ((key && SERVICE_KEYS.has(key)) || t.startsWith('- ')) hasServiceOrList = true;
  }

  if (nonEmpty < 2) return false;
  if (hasTabs) return true;
  if (indented === 0 && (hasRoot || hasServiceOrList)) return true;

  // Also trigger if YAML parses cleanly into null/broken top-level keys where `services:` has scalar null
  // and `image:` / `ports:` ended up at indent 0.
  try {
    const doc = parseDocument(text);
    if (doc.errors.length > 0 && hasRoot) return true;
    const js = doc.toJS() as Record<string, unknown> | null;
    if (js && typeof js === 'object' && 'services' in js && js.services == null && hasServiceOrList) {
      return true;
    }
  } catch {
    return true;
  }
  return false;
}

/**
 * Reconstructs standard 2-space Compose YAML indentation for an unindented or tab-indented snippet.
 * Returns `{ text, changed }`. Never drops comments or blank lines.
 */
export function autoIndentCompose(raw: string): { text: string; changed: boolean } {
  const normalized = raw.replace(/\r\n/g, '\n');
  const lines = normalized.split('\n');
  const hasTabs = lines.some((l) => /^\t+/.test(l));

  // Fast path: pure tab-to-2-spaces conversion if lines already have consistent tab/space hierarchy
  if (hasTabs && lines.some((l) => /^\t+/.test(l)) && !lines.every((l) => l.trim() === '' || !/^\s/.test(l))) {
    const converted = lines.map((l) => l.replace(/^\t+/, (m) => '  '.repeat(m.length))).join('\n');
    try {
      const doc = parseDocument(converted);
      if (doc.errors.length === 0) {
        return { text: converted, changed: converted !== normalized };
      }
    } catch {
      // fall through to structural reconstruction
    }
  }

  let rootSection: string | null = null;
  let objIndent = 2; // level of service name / top-level network/volume name
  let currentSubSection: string | null = null;
  let subSectionIndent = 6;
  let prevWasListOwnerIndent = -1;
  const out: string[] = [];

  for (let i = 0; i < lines.length; i++) {
    const rawLine = lines[i];
    const t = rawLine.trim();
    if (!t) {
      out.push('');
      continue;
    }

    // Preserve comment aligned to the next or current context
    if (t.startsWith('#')) {
      const nextLine = lines.slice(i + 1).find((l) => l.trim() && !l.trim().startsWith('#'))?.trim() ?? '';
      const nextKey = extractKey(nextLine);
      if (nextKey && ROOT_KEYS.has(nextKey)) {
        out.push(t);
      } else if (prevWasListOwnerIndent >= 0 && nextLine.startsWith('-')) {
        out.push(' '.repeat(prevWasListOwnerIndent + 2) + t);
      } else if (rootSection === 'services' && nextKey && SERVICE_KEYS.has(nextKey)) {
        out.push('    ' + t);
      } else if (rootSection) {
        out.push('  ' + t);
      } else {
        out.push(t);
      }
      continue;
    }

    // List item (`- foo` or `-`)
    if (/^-\s*/.test(t)) {
      const indent = prevWasListOwnerIndent >= 0 ? prevWasListOwnerIndent + 2 : 6;
      out.push(' '.repeat(indent) + t);
      continue;
    }

    const key = extractKey(t);
    const hasVal = hasValueAfterColon(t);

    // Explicit top-level Compose key
    if (key && ROOT_KEYS.has(key)) {
      // Edge case: `volumes:`, `networks:`, `configs:`, `secrets:` can appear BOTH at root (indent 0) AND under a service (indent 4)!
      // Disambiguate using context + lookahead:
      const isAmbiguous =
        key === 'volumes' || key === 'networks' || key === 'configs' || key === 'secrets' || key === 'name';
      if (isAmbiguous && rootSection === 'services') {
        const nextNonEmpty = lines.slice(i + 1).find((l) => l.trim() && !l.trim().startsWith('#'))?.trim() ?? '';
        const nextKey = extractKey(nextNonEmpty);
        // Under a service: `volumes:` / `networks:` / `configs:` / `secrets:` is followed by `- ` items or stops before another service;
        // At root: `volumes:` / `networks:` at the end is followed by `<volName>:` with optional `driver:`/`external:` or blank end.
        const looksLikeServiceProp =
          key === 'name'
            ? false
            : nextNonEmpty.startsWith('- ') ||
              hasVal ||
              (nextKey !== null && SERVICE_KEYS.has(nextKey));
        const looksLikeTopObj =
          key !== 'name' &&
          nextKey !== null &&
          !SERVICE_KEYS.has(nextKey) &&
          !ROOT_KEYS.has(nextKey) &&
          !nextNonEmpty.startsWith('- ');
        if (looksLikeServiceProp && !looksLikeTopObj) {
          out.push('    ' + t);
          currentSubSection = key;
          subSectionIndent = 6;
          prevWasListOwnerIndent = hasVal ? -1 : 4;
          continue;
        }
      }

      rootSection = SECTION_ROOT_CONTAINERS.has(key) ? key : null;
      currentSubSection = null;
      objIndent = 2;
      prevWasListOwnerIndent = hasVal ? -1 : 0;
      out.push(t);
      continue;
    }

    // Inside `services:`
    if (rootSection === 'services') {
      if (key && SERVICE_KEYS.has(key)) {
        out.push('    ' + t);
        currentSubSection = SERVICE_SECTION_KEYS.has(key) ? key : null;
        subSectionIndent = 6;
        prevWasListOwnerIndent = hasVal ? -1 : 4;
        continue;
      }

      if (key && SUB_SECTION_KEYS.has(key) && currentSubSection) {
        const lvl =
          currentSubSection === 'deploy' && (key === 'limits' || key === 'reservations')
            ? 8
            : currentSubSection === 'deploy' && (key === 'memory' || key === 'cpus' || key === 'max_attempts' || key === 'delay' || key === 'window' || key === 'condition')
              ? 10
              : 6;
        out.push(' '.repeat(lvl) + t);
        prevWasListOwnerIndent = hasVal ? -1 : lvl;
        continue;
      }

      // Check if we are inside a map-style service section (`environment:`, `labels:`, `depends_on:`, `networks:`, `build:`, `deploy:`, `logging:`)
      if (currentSubSection && currentSubSection !== 'healthcheck') {
        const nextNonEmpty = lines.slice(i + 1).find((l) => l.trim() && !l.trim().startsWith('#'))?.trim() ?? '';
        const nextKey = extractKey(nextNonEmpty);
        // If `key:` has NO value and next line IS a SERVICE_KEY (`image:`, `ports:`, ...), then THIS `key:` is a new service name!
        if (!hasVal && nextKey && SERVICE_KEYS.has(nextKey)) {
          out.push('  ' + t);
          currentSubSection = null;
          prevWasListOwnerIndent = -1;
          continue;
        }
        // Otherwise it's a key=val or map entry under `currentSubSection`
        if (currentSubSection === 'environment' || currentSubSection === 'labels' || currentSubSection === 'args' || currentSubSection === 'driver_opts' || currentSubSection === 'options' || currentSubSection === 'sysctls' || currentSubSection === 'annotations' || currentSubSection === 'extra_hosts' || currentSubSection === 'networks' || currentSubSection === 'depends_on' || currentSubSection === 'build' || currentSubSection === 'deploy' || currentSubSection === 'logging' || currentSubSection === 'blkio_config' || currentSubSection === 'ulimits' || currentSubSection === 'develop') {
          out.push(' '.repeat(subSectionIndent) + t);
          prevWasListOwnerIndent = hasVal ? -1 : subSectionIndent;
          continue;
        }
      }

      // Otherwise treat `foo:` as a service name under `services:`
      if (key && !hasVal) {
        out.push('  ' + t);
        currentSubSection = null;
        prevWasListOwnerIndent = -1;
        continue;
      }

      // Fallback for key=value / ENV_VAR: value when inside an unnamed/unknown block
      out.push('      ' + t);
      continue;
    }

    // Inside top-level `volumes:` / `networks:` / `configs:` / `secrets:`
    if (rootSection) {
      if (key && TOP_OBJ_KEYS.has(key)) {
        out.push('    ' + t);
        currentSubSection = key;
        prevWasListOwnerIndent = hasVal ? -1 : 4;
        continue;
      }
      if (currentSubSection === 'driver_opts' || currentSubSection === 'labels' || currentSubSection === 'ipam') {
        out.push('      ' + t);
        prevWasListOwnerIndent = hasVal ? -1 : 6;
        continue;
      }
      if (key) {
        out.push(' '.repeat(objIndent) + t);
        currentSubSection = null;
        prevWasListOwnerIndent = hasVal ? -1 : objIndent;
        continue;
      }
    }

    // No root section seen yet: maybe user pasted a single service block (`app:\nimage: nginx\nports:\n- "80:80"`)
    if (key && SERVICE_KEYS.has(key)) {
      out.push('  ' + t);
      currentSubSection = SERVICE_SECTION_KEYS.has(key) ? key : null;
      prevWasListOwnerIndent = hasVal ? -1 : 2;
      continue;
    }
    if (key && !hasVal) {
      out.push(t);
      currentSubSection = null;
      prevWasListOwnerIndent = -1;
      continue;
    }
    out.push('  ' + t);
  }

  const result = out.join('\n');
  return { text: result, changed: result !== normalized };
}
