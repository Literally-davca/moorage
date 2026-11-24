// A lossless model of a compose .env file for the Variables view: comments, blank
// lines, order, `export`, quotes and multi-line values survive a round trip, because a
// line nobody edited is written back exactly as it was read.

export type EnvVar = {
  kind: 'var';
  key: string;
  value: string;
  /** Original text (one or more lines); dropped when the row is edited. */
  raw?: string;
  exported?: boolean;
};
export type EnvOther = { kind: 'other'; raw: string };
export type EnvLine = EnvVar | EnvOther;

const assignRe = /^(\s*)(export\s+)?([^=\s#][^=]*?)\s*=(.*)$/;

/** parse splits .env text into lines; KEY=VALUE pairs become variables. */
export function parseEnv(text: string): EnvLine[] {
  const src = text.replace(/\r\n/g, '\n').split('\n');
  // A trailing newline is not an extra blank line.
  if (src.length && src[src.length - 1] === '') src.pop();
  const out: EnvLine[] = [];
  for (let i = 0; i < src.length; i++) {
    const line = src[i];
    // A lone "=" (an empty assignment, e.g. from an old "Add variable") is no variable and
    // breaks the .env syntax: drop it. "=value" becomes a row without a name to fix.
    const orphan = /^\s*=(.*)$/.exec(line);
    if (orphan) {
      if (orphan[1].trim() !== '') out.push({ kind: 'var', key: '', value: orphan[1].trim() });
      continue;
    }
    const m = assignRe.exec(line);
    if (!m || line.trimStart().startsWith('#')) {
      out.push({ kind: 'other', raw: line });
      continue;
    }
    const key = m[3].trim();
    let rest = m[4].trimStart();
    let raw = line;
    let value: string;
    const q = rest[0];
    if (q === '"' || q === "'") {
      // A quoted value may span lines until its closing quote.
      let body = rest.slice(1);
      let end = closingQuote(body, q);
      while (end < 0 && i + 1 < src.length) {
        i++;
        raw += '\n' + src[i];
        body += '\n' + src[i];
        end = closingQuote(body, q);
      }
      value = end < 0 ? body : body.slice(0, end);
      if (q === '"') value = value.replace(/\\(["\\])/g, '$1').replace(/\\n/g, '\n');
    } else {
      // Unquoted: an inline comment starts at " #".
      const hash = rest.search(/\s#/);
      if (hash >= 0) rest = rest.slice(0, hash);
      value = rest.trim();
    }
    out.push({ kind: 'var', key, value, raw, exported: !!m[2] });
  }
  return out;
}

function closingQuote(s: string, q: string): number {
  for (let i = 0; i < s.length; i++) {
    if (q === '"' && s[i] === '\\') {
      i++;
      continue;
    }
    if (s[i] === q) return i;
  }
  return -1;
}

/** formatValue quotes a value only when it needs it. */
export function formatValue(v: string): string {
  if (v === '') return '';
  if (!/[\s#"'\\$`]/.test(v)) return v;
  // Single quotes keep everything literal (no ${} interpolation), unless the value has one.
  if (!v.includes("'") && !v.includes('\n')) return `'${v}'`;
  return '"' + v.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n') + '"';
}

/** serializeEnv writes the lines back; untouched lines keep their original text. */
export function serializeEnv(lines: EnvLine[]): string {
  if (!lines.length) return '';
  return (
    lines
      .flatMap((l) => {
        if (l.kind === 'other') return [l.raw];
        if (l.raw !== undefined) return [l.raw];
        // A row without a name is never written as "=value" (invalid .env): an empty one
        // is dropped, one with a value is kept as a comment so the value isn't lost.
        if (!l.key) return l.value ? [`# =${l.value}`] : [];
        return [`${l.exported ? 'export ' : ''}${l.key}=${formatValue(l.value)}`];
      })
      .join('\n') + '\n'
  );
}

/**
 * unusedVars lists the .env keys compose.yaml never references. Empty when compose loads
 * the whole file into containers (env_file: .env), where every key counts as used.
 */
export function unusedVars(env: string, compose: string): string[] {
  if (!compose.trim()) return [];
  const used = composeVars(compose);
  if (used.envFile) return [];
  const keys = parseEnv(env)
    .filter((l): l is EnvVar => l.kind === 'var' && !!l.key)
    .map((l) => l.key);
  return [...new Set(keys.filter((k) => !used.all.has(k)))];
}

/** removeVars drops the given keys from .env text, keeping every other line as it was. */
export function removeVars(env: string, keys: string[]): string {
  const drop = new Set(keys);
  return serializeEnv(parseEnv(env).filter((l) => l.kind !== 'var' || !drop.has(l.key)));
}

/** A key compose accepts in .env: letters, digits, underscore, dot and dash, not a digit first. */
export const validKey = (k: string) => /^[A-Za-z_][A-Za-z0-9_.-]*$/.test(k);

/** looksSecret decides whether a value is masked by default. */
export const looksSecret = (k: string) => /pass|secret|token|key|private|credential|auth|salt|cert/i.test(k);

/**
 * composeVars lists the variables a compose file interpolates: required ones (no
 * default) and the rest, skipping `$$` escapes. envFile is true when the compose file
 * also loads .env into containers (env_file: .env), so every variable counts as used.
 */
export function composeVars(compose: string): {
  required: Set<string>;
  all: Set<string>;
  envFile: boolean;
  servicesByVar: Map<string, string[]>;
  defaults: Map<string, string>;
} {
  const required = new Set<string>();
  const all = new Set<string>();
  const servicesByVar = new Map<string, string[]>();
  const defaults = new Map<string, string>();
  const text = compose.replace(/\$\$/g, '');
  const re = /\$(?:\{([A-Za-z_][A-Za-z0-9_]*)([^}]*)\}|([A-Za-z_][A-Za-z0-9_]*))/g;

  let inServices = false;
  let svcIndent = -1;
  let currentSvc = '';

  for (const rawLine of text.split(/\r?\n/)) {
    const trimmed = rawLine.trim();
    if (!trimmed || trimmed.startsWith('#')) continue;
    const indent = rawLine.search(/\S/);
    if (indent === 0) {
      inServices = /^services\s*:/.test(trimmed);
      svcIndent = -1;
      currentSvc = '';
    } else if (inServices) {
      if (svcIndent < 0 || indent <= svcIndent) {
        const svcMatch = /^([A-Za-z0-9_.-]+)\s*:/.exec(trimmed);
        if (svcMatch) {
          svcIndent = indent;
          currentSvc = svcMatch[1];
        }
      }
    }

    for (const m of rawLine.matchAll(re)) {
      const name = m[1] ?? m[3];
      all.add(name);
      const mod = m[2] ?? '';
      // ${VAR:-x} / ${VAR-x} have a default; ${VAR:+x} only applies when set.
      if (!/^:?[-+]/.test(mod)) {
        required.add(name);
      } else if (/^:?-(.*)$/.test(mod)) {
        const def = mod.replace(/^:?-/, '');
        if (def && !defaults.has(name)) defaults.set(name, def);
      }
      if (currentSvc) {
        const list = servicesByVar.get(name) ?? [];
        if (!list.includes(currentSvc)) {
          list.push(currentSvc);
          servicesByVar.set(name, list);
        }
      }
    }
  }

  const envFile = /env_file\s*:[\s\S]*?(^|[\s'"/-])\.env(['"\s]|$)/m.test(compose);
  return { required, all, envFile, servicesByVar, defaults };
}
