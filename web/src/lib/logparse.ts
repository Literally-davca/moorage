// Safe ANSI SGR parser, structured JSON log detector, and log-level classifier.
// Never produces raw HTML — all tokens render as plain text inside typed spans.

export type AnsiColor =
  | 'gray'
  | 'red'
  | 'green'
  | 'yellow'
  | 'blue'
  | 'magenta'
  | 'cyan'
  | 'white';

export interface AnsiSegment {
  text: string;
  fg?: AnsiColor;
  bold?: boolean;
  dim?: boolean;
  italic?: boolean;
  underline?: boolean;
}

export type LogLevel = 'error' | 'warn' | 'info' | 'debug' | 'other';

export interface JsonLogField {
  key: string;
  value: string;
}

export interface ParsedLogLine {
  level: LogLevel;
  isJson: boolean;
  summary: string;
  fields?: JsonLogField[];
}

const FG_MAP: Record<number, AnsiColor> = {
  30: 'gray',
  31: 'red',
  32: 'green',
  33: 'yellow',
  34: 'blue',
  35: 'magenta',
  36: 'cyan',
  37: 'white',
  90: 'gray',
  91: 'red',
  92: 'green',
  93: 'yellow',
  94: 'blue',
  95: 'magenta',
  96: 'cyan',
  97: 'white',
};

// Matches CSI sequences: ESC [ params final-byte
// eslint-disable-next-line no-control-regex
const CSI_RE = /\x1b\[([0-9;]*)([A-Za-z])/g;

export function stripAnsi(input: string): string {
  if (!input.includes('\x1b')) return input;
  return input.replace(CSI_RE, '');
}

export function parseAnsi(input: string): AnsiSegment[] {
  if (!input.includes('\x1b')) {
    return input ? [{ text: input }] : [];
  }
  const out: AnsiSegment[] = [];
  let fg: AnsiColor | undefined;
  let bold = false;
  let dim = false;
  let italic = false;
  let underline = false;
  let last = 0;

  const pushText = (text: string) => {
    if (!text) return;
    const seg: AnsiSegment = { text };
    if (fg) seg.fg = fg;
    if (bold) seg.bold = true;
    if (dim) seg.dim = true;
    if (italic) seg.italic = true;
    if (underline) seg.underline = true;
    out.push(seg);
  };

  CSI_RE.lastIndex = 0;
  for (const m of input.matchAll(CSI_RE)) {
    const idx = m.index ?? 0;
    if (idx > last) {
      pushText(input.slice(last, idx));
    }
    last = idx + m[0].length;
    // Ignore non-SGR CSI sequences (cursor movement, clear screen, etc.)
    if (m[2] !== 'm') continue;
    const rawParams = m[1];
    const codes = rawParams === '' ? [0] : rawParams.split(';').map((s) => Number(s || '0'));
    for (let i = 0; i < codes.length; i++) {
      const c = codes[i];
      if (c === 0) {
        fg = undefined;
        bold = false;
        dim = false;
        italic = false;
        underline = false;
      } else if (c === 1) {
        bold = true;
      } else if (c === 2) {
        dim = true;
      } else if (c === 3) {
        italic = true;
      } else if (c === 4) {
        underline = true;
      } else if (c === 22) {
        bold = false;
        dim = false;
      } else if (c === 23) {
        italic = false;
      } else if (c === 24) {
        underline = false;
      } else if (c === 39) {
        fg = undefined;
      } else if (c === 38) {
        // 256-color (38;5;n) or truecolor (38;2;r;g;b): skip extra params safely
        if (codes[i + 1] === 5) i += 2;
        else if (codes[i + 1] === 2) i += 4;
      } else if (c === 48) {
        if (codes[i + 1] === 5) i += 2;
        else if (codes[i + 1] === 2) i += 4;
      } else if (FG_MAP[c]) {
        fg = FG_MAP[c];
      }
    }
  }
  if (last < input.length) {
    pushText(input.slice(last));
  }
  return out;
}

function normalizeLevelValue(val: unknown): LogLevel | null {
  if (typeof val === 'number') {
    if (val >= 50) return 'error';
    if (val >= 40) return 'warn';
    if (val >= 30) return 'info';
    if (val >= 10) return 'debug';
    return null;
  }
  if (typeof val !== 'string') return null;
  const s = val.trim().toLowerCase();
  if (s === 'error' || s === 'err' || s === 'fatal' || s === 'panic' || s === 'crit' || s === 'critical') return 'error';
  if (s === 'warn' || s === 'warning') return 'warn';
  if (s === 'info' || s === 'information' || s === 'notice') return 'info';
  if (s === 'debug' || s === 'trace' || s === 'verbose' || s === 'dbug') return 'debug';
  return null;
}

const TEXT_LEVEL_RE =
  /(?:^|[\s[\]()=|":])(?:level=)?(error|err|fatal|panic|critical|warn|warning|info|debug|trace)(?:$|[\s[\]()=|":])/i;

export function detectTextLevel(clean: string, stream: string): LogLevel {
  const m = TEXT_LEVEL_RE.exec(clean.slice(0, 240));
  if (m) {
    const lv = normalizeLevelValue(m[1]);
    if (lv) return lv;
  }
  if (stream === 'stderr' && /\b(exception|traceback|failed|unhandled)\b/i.test(clean)) {
    return 'error';
  }
  return 'other';
}

function formatFieldValue(v: unknown): string {
  if (v === null) return 'null';
  if (v === undefined) return '';
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

export function parseLogLine(raw: string, stream: string): ParsedLogLine {
  const clean = stripAnsi(raw).trim();
  if (clean.length >= 2 && clean.charCodeAt(0) === 123 && clean.charCodeAt(clean.length - 1) === 125) {
    try {
      const parsed = JSON.parse(clean) as unknown;
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        const obj = parsed as Record<string, unknown>;
        const keys = Object.keys(obj);
        if (keys.length > 0) {
          const rawLvl = obj.level ?? obj.lvl ?? obj.severity ?? obj['log.level'] ?? obj.loglevel;
          const level = normalizeLevelValue(rawLvl) ?? detectTextLevel(clean, stream);

          const msgVal = obj.msg ?? obj.message ?? obj.err ?? obj.error ?? obj.event;
          let summary = '';
          if (typeof msgVal === 'string' && msgVal.trim() !== '') {
            summary = msgVal;
          } else if (msgVal && typeof msgVal === 'object') {
            summary = formatFieldValue(msgVal);
          } else {
            // Compact key=value preview for structured JSON lines without msg
            const previewKeys = keys.filter(
              (k) => !['time', 'ts', 'timestamp', '@timestamp', 'level', 'lvl', 'severity'].includes(k),
            );
            summary = (previewKeys.length ? previewKeys : keys)
              .slice(0, 4)
              .map((k) => `${k}=${formatFieldValue(obj[k])}`)
              .join(' ');
          }

          const fields: JsonLogField[] = keys.map((k) => ({
            key: k,
            value: formatFieldValue(obj[k]),
          }));
          return {
            level,
            isJson: true,
            summary,
            fields,
          };
        }
      }
    } catch {
      // Not valid JSON; fall through to plain/ANSI text
    }
  }
  return {
    level: detectTextLevel(clean, stream),
    isJson: false,
    summary: raw,
  };
}

export function ansiClass(seg: AnsiSegment): string {
  const classes: string[] = [];
  if (seg.fg) classes.push(`ansi-${seg.fg}`);
  if (seg.bold) classes.push('ansi-bold');
  if (seg.dim) classes.push('ansi-dim');
  if (seg.italic) classes.push('ansi-italic');
  if (seg.underline) classes.push('ansi-underline');
  return classes.join(' ');
}

export function sourceBadgeTone(label: string): number {
  let h = 0;
  for (let i = 0; i < label.length; i++) {
    h = (h * 31 + label.charCodeAt(i)) >>> 0;
  }
  return h % 6;
}
