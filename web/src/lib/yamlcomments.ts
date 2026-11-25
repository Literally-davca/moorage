// A lossless "Clean (no comments)" view of a compose file. The clean text hides full-line
// and inline # comments and runs of blank lines; edits made in it are merged back into the
// original so every line the user didn't touch keeps its comments.

type Line = { text: string; clean: string | null; kind: 'content' | 'comment' | 'blank' }; // clean: null = hidden in clean mode

/** commentStart finds the # that starts an inline comment, -1 when there is none. */
function commentStart(line: string): number {
  let q: string | null = null;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (q) {
      if (q === '"' && c === '\\') {
        i++;
        continue;
      }
      if (c === q) {
        if (q === "'" && line[i + 1] === "'") {
          i++;
          continue;
        }
        q = null;
      }
      continue;
    }
    if (c === '"' || c === "'") {
      // a quote opens a string only at the start of a scalar
      const prev = line.slice(0, i).trimEnd();
      if (prev === '' || /[:\-,[{?]$/.test(prev)) q = c;
      continue;
    }
    if (c === '#' && (i === 0 || line[i - 1] === ' ' || line[i - 1] === '\t')) return i;
  }
  return -1;
}

const indentOf = (s: string) => s.length - s.trimStart().length;

function analyse(text: string): Line[] {
  const src = text.replace(/\r\n/g, '\n').split('\n');
  const out: Line[] = [];
  let blockIndent = -1; // inside a block scalar (| or >) more indented than this line
  for (let i = 0; i < src.length; i++) {
    const text = src[i];
    const t = text.trim();
    if (blockIndent >= 0) {
      if (t === '') {
        // Keep a blank line inside a block scalar only when the scalar actually continues afterwards.
        const nextContent = src.slice(i + 1).find((s) => s.trim() !== '');
        if (nextContent !== undefined && indentOf(nextContent) > blockIndent) {
          out.push({ text, clean: text, kind: 'content' });
          continue;
        }
        blockIndent = -1;
      } else if (indentOf(text) > blockIndent) {
        out.push({ text, clean: text, kind: 'content' }); // literal content: # belongs to the scalar
        continue;
      } else {
        blockIndent = -1;
      }
    }
    if (t === '') {
      out.push({ text, clean: null, kind: 'blank' });
      continue;
    }
    if (t.startsWith('#')) {
      out.push({ text, clean: null, kind: 'comment' });
      continue;
    }
    const hash = commentStart(text);
    const body = hash >= 0 ? text.slice(0, hash).trimEnd() : text;
    if (body.trim() === '' && hash >= 0) {
      out.push({ text, clean: null, kind: 'comment' });
    } else {
      out.push({ text, clean: body, kind: 'content' });
    }
    if (/[:-]\s*[|>][-+0-9]*\s*$/.test(body)) blockIndent = indentOf(text);
  }
  return out;
}

/** hasComments reports whether the clean view would hide comments or blank lines. */
export function hasComments(text: string): boolean {
  return analyse(text).some((l) => l.clean === null || l.clean !== l.text);
}

/** toClean returns the text without comments and without blank lines (outside block scalars). */
export function toClean(text: string): string {
  return analyse(text)
    .filter((l) => l.clean !== null)
    .map((l) => l.clean as string)
    .join('\n');
}

/**
 * mergeClean applies an edit of the clean view to the original:
 * - lines that are unchanged come back with their inline comments and the comment lines above them;
 * - original blank lines are removed (Clean mode strips empty lines to save vertical space), while any
 *   blank lines typed by the user in `editedClean` stay as part of `editedClean`;
 * - changed and new lines are taken as typed; comment lines above a deleted line stay.
 */
export function mergeClean(original: string, editedClean: string): string {
  const lines = analyse(original);
  // clean lines of the original, each with the hidden comment lines just above it (blank lines dropped)
  const anchors: { clean: string; text: string; before: string[] }[] = [];
  let pending: string[] = [];
  for (const l of lines) {
    if (l.clean === null) {
      if (l.kind === 'comment') pending.push(l.text);
      continue;
    }
    anchors.push({ clean: l.clean, text: l.text, before: pending });
    pending = [];
  }
  const tail = pending;
  const a = anchors.map((x) => x.clean);
  const b = editedClean.replace(/\r\n/g, '\n').split('\n');
  // LCS table (files are at most a few thousand lines)
  const n = a.length;
  const m = b.length;
  const dp: Uint32Array[] = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const out: string[] = [];
  let i = 0;
  let j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && a[i] === b[j]) {
      out.push(...anchors[i].before, anchors[i].text);
      i++;
      j++;
    } else if (j < m && (i >= n || dp[i][j + 1] >= dp[i + 1][j])) {
      out.push(b[j]);
      j++;
    } else {
      out.push(...anchors[i].before); // the line is gone, its comments stay
      i++;
    }
  }
  out.push(...tail);
  return out.join('\n');
}
