// A small Markdown parser for assistant answers (M19E). It produces plain data that
// Markdown.svelte renders as Svelte nodes: no HTML from the input is ever rendered and
// there are no images. Links are kept only when the caller says they are safe.

export type Inline =
  | { t: 'text'; text: string }
  | { t: 'code'; text: string }
  | { t: 'b'; text: string }
  | { t: 'i'; text: string }
  | { t: 'a'; text: string; href: string; external: boolean };

export type Block =
  | { t: 'p'; inl: Inline[] }
  | { t: 'h'; level: number; inl: Inline[] }
  | { t: 'code'; lang: string; text: string }
  | { t: 'ul' | 'ol'; items: Inline[][]; start?: number }
  | { t: 'quote'; inl: Inline[] }
  | { t: 'table'; head: Inline[][]; rows: Inline[][][] }
  | { t: 'hr' };

/** linkCheck returns the href to use (internal route or a verified external URL) or null. */
export type LinkCheck = (url: string) => { href: string; external: boolean } | null;

const inlineRe = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*|__[^_\n]+__)|(\*[^*\s][^*\n]*\*|_[^_\s][^_\n]*_)|(\[[^\]\n]+\]\([^)\s]+\))|((?:https?:\/\/|#\/)[^\s<>()`'"]+[^\s<>()`'".,;:!?])/g;

export function parseInline(src: string, check: LinkCheck): Inline[] {
  const out: Inline[] = [];
  let last = 0;
  let m: RegExpExecArray | null;
  inlineRe.lastIndex = 0;
  const text = (s: string) => {
    if (!s) return;
    const prev = out[out.length - 1];
    if (prev && prev.t === 'text') prev.text += s;
    else out.push({ t: 'text', text: s });
  };
  while ((m = inlineRe.exec(src))) {
    text(src.slice(last, m.index));
    const [all, code, bold, ital, mdLink, bare] = m;
    if (code) out.push({ t: 'code', text: code.slice(1, -1) });
    else if (bold) out.push({ t: 'b', text: bold.slice(2, -2) });
    else if (ital) out.push({ t: 'i', text: ital.slice(1, -1) });
    else if (mdLink) {
      const lm = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(mdLink)!;
      const ok = check(lm[2]);
      if (ok) out.push({ t: 'a', text: lm[1], href: ok.href, external: ok.external });
      else text(lm[1] + ' (' + lm[2] + ')');
    } else if (bare) {
      const ok = check(bare);
      if (ok) out.push({ t: 'a', text: bare, href: ok.href, external: ok.external });
      else text(bare);
    } else text(all);
    last = m.index + all.length;
  }
  text(src.slice(last));
  return out;
}

function splitRow(line: string): string[] {
  let l = line.trim();
  if (l.startsWith('|')) l = l.slice(1);
  if (l.endsWith('|')) l = l.slice(0, -1);
  return l.split('|').map((c) => c.trim());
}

export function parseMarkdown(src: string, check: LinkCheck): Block[] {
  const lines = src.replace(/\r\n/g, '\n').split('\n');
  const out: Block[] = [];
  let para: string[] = [];
  const flush = () => {
    if (para.length) out.push({ t: 'p', inl: parseInline(para.join('\n'), check) });
    para = [];
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const fence = /^\s*(```|~~~)\s*([\w+-]*)\s*$/.exec(line);
    if (fence) {
      flush();
      const body: string[] = [];
      i++;
      while (i < lines.length && !new RegExp('^\\s*' + fence[1] + '\\s*$').test(lines[i])) body.push(lines[i++]);
      out.push({ t: 'code', lang: fence[2] || '', text: body.join('\n') });
      continue;
    }
    if (/^\s*$/.test(line)) {
      flush();
      continue;
    }
    const h = /^(#{1,4})\s+(.*)$/.exec(line);
    if (h) {
      flush();
      out.push({ t: 'h', level: h[1].length, inl: parseInline(h[2], check) });
      continue;
    }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) {
      flush();
      out.push({ t: 'hr' });
      continue;
    }
    if (/^\s*\|.*\|\s*$/.test(line) && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}/.test(lines[i + 1])) {
      flush();
      const head = splitRow(line).map((c) => parseInline(c, check));
      i += 2;
      const rows: Inline[][][] = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) rows.push(splitRow(lines[i++]).map((c) => parseInline(c, check)));
      i--;
      out.push({ t: 'table', head, rows });
      continue;
    }
    const li = /^\s*([-*+]|(\d+)[.)])\s+(.*)$/.exec(line);
    if (li) {
      flush();
      const ordered = !!li[2];
      const items: Inline[][] = [];
      const start = ordered ? Number(li[2]) : undefined;
      while (i < lines.length) {
        const m = /^\s*([-*+]|(\d+)[.)])\s+(.*)$/.exec(lines[i]);
        if (m && !!m[2] === ordered) {
          items.push(parseInline(m[3], check));
          i++;
        } else if (items.length && /^\s{2,}\S/.test(lines[i])) {
          // continuation line of the last item
          items[items.length - 1].push({ t: 'text', text: ' ' }, ...parseInline(lines[i].trim(), check));
          i++;
        } else break;
      }
      i--;
      out.push(ordered ? { t: 'ol', items, start } : { t: 'ul', items });
      continue;
    }
    const q = /^\s*>\s?(.*)$/.exec(line);
    if (q) {
      flush();
      out.push({ t: 'quote', inl: parseInline(q[1], check) });
      continue;
    }
    para.push(line);
  }
  flush();
  return out;
}
