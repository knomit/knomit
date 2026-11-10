// What a chat tool result's JSON view shows, worked out before CodeMirror sees
// it: the text (pretty-printed JSON, or the result as it came), the blocks that
// start folded, a folded block's count, the kb:// fact paths in it, and the date
// a `*_at` number stands for. Pure, so each rule is tested without an editor.

import { foldedCount } from './chatCopy';

/**
 * unpackJson replaces every string that is itself JSON (an object or an array:
 * it starts with `{` or `[` once trimmed, and parses) with what it parses to,
 * recursively, at any depth. A tool that returns a JSON document inside a
 * string field (a review's `response`) is then read as structure, not as one
 * long escaped line. Anything else is left exactly as it is.
 */
export function unpackJson(v: unknown): unknown {
  if (typeof v === 'string') {
    const s = v.trim();
    if (s.startsWith('{') || s.startsWith('[')) {
      try { return unpackJson(JSON.parse(s)); } catch { /* not JSON: the string stays */ }
    }
    return v;
  }
  if (Array.isArray(v)) return v.map(unpackJson);
  if (v && typeof v === 'object') {
    // defineProperty, not out[k] = …: assigning "__proto__" on a plain object
    // sets its prototype instead of adding the key, and the key was dropped.
    const out: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v)) {
      Object.defineProperty(out, k, { value: unpackJson(x), enumerable: true, writable: true, configurable: true });
    }
    return out;
  }
  return v;
}

/** The JSON view's text: `json` says whether it is JSON (highlighted, folded). */
export interface ViewText { text: string; json: boolean }

/**
 * resultText is what the view shows for a raw result: the parsed JSON, unpacked
 * and pretty-printed with 2-space indents; or, when the result is not JSON, the
 * result exactly as it came, as plain text.
 */
export function resultText(raw: string): ViewText {
  // Inside the try too: a structure deep enough to overflow the stack in
  // unpackJson or JSON.stringify (a RangeError) is shown as the text it came as,
  // rather than crashing the panel.
  try {
    return { text: JSON.stringify(unpackJson(JSON.parse(raw)), null, 2), json: true };
  } catch {
    return { text: raw, json: false };
  }
}

/** jsonText is the view's text for a value the page already holds (a call's arguments). */
export function jsonText(v: unknown): ViewText {
  try {
    return { text: JSON.stringify(unpackJson(v ?? null), null, 2) ?? 'null', json: true };
  } catch {
    // Too deep to walk (a RangeError): the compact form, as plain text, if even
    // that fits. String(v) recurses through nested arrays too (it overflowed on
    // node 22's smaller stack), so it is guarded as well; past it, nothing is shown.
    try { return { text: JSON.stringify(v) ?? 'null', json: false }; } catch { /* still too deep */ }
    try { return { text: String(v), json: false }; } catch { return { text: '', json: false }; }
  }
}

/** One `{…}` or `[…]` of a JSON text: the range CodeMirror folds (just inside the
 *  brackets, as lang-json's fold service gives it) and how deep it sits (the
 *  outermost block is 0). */
export interface JsonBlock { from: number; to: number; depth: number; kind: 'object' | 'array' }

/** jsonBlocks lists the blocks of a JSON text that span lines, outermost first. */
export function jsonBlocks(text: string): JsonBlock[] {
  const out: JsonBlock[] = [];
  const open: { at: number; kind: 'object' | 'array' }[] = [];
  let inString = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inString) {
      if (c === '\\') i++;
      else if (c === '"') inString = false;
      continue;
    }
    if (c === '"') inString = true;
    else if (c === '{' || c === '[') open.push({ at: i, kind: c === '{' ? 'object' : 'array' });
    else if (c === '}' || c === ']') {
      const o = open.pop();
      if (o && text.slice(o.at + 1, i).includes('\n')) out.push({ from: o.at + 1, to: i, depth: open.length, kind: o.kind });
    }
  }
  return out.sort((a, b) => a.from - b.from);
}

/** The depth a block starts folded at: anything deeper than two levels. */
export const FOLD_DEPTH = 3;

/**
 * initialFolds is the blocks the view opens with folded: every block at depth
 * FOLD_DEPTH or more that is not inside one already folded (its own folds stay
 * open, so unfolding a block shows it, not another row of pills).
 */
export function initialFolds(text: string, depth = FOLD_DEPTH): { from: number; to: number }[] {
  const out: { from: number; to: number }[] = [];
  let until = -1;
  for (const b of jsonBlocks(text)) {
    if (b.depth < depth || b.from < until) continue;
    out.push({ from: b.from, to: b.to });
    until = b.to;
  }
  return out;
}

/**
 * foldLabel is a folded block's pill: how many keys (an object) or items (an
 * array) the text between `from` and `to` holds at its own level. The block is
 * the one whose opening bracket sits just before `from`. Null when it is not a
 * JSON block (the view folds nothing else).
 */
export function foldLabel(text: string, from: number, to: number): string | null {
  const opener = text[from - 1];
  if (opener !== '{' && opener !== '[') return null;
  let n = 0;
  let any = false;
  let depth = 0;
  let inString = false;
  for (let i = from; i < to; i++) {
    const c = text[i];
    if (inString) {
      if (c === '\\') i++;
      else if (c === '"') inString = false;
      continue;
    }
    if (c === '"') { inString = true; any = true; }
    else if (c === '{' || c === '[') { depth++; any = true; }
    else if (c === '}' || c === ']') depth--;
    else if (c === ',' && depth === 0) n++;
    else if (!/\s/.test(c)) any = true;
  }
  return foldedCount(opener === '{' ? 'object' : 'array', any ? n + 1 : 0);
}

/** A kb:// fact path in the text: the store (a repo id or name), then a path. */
const KB_REF = /kb:\/\/[\w.-]+\/[\w./-]+/g;

/** kbRefs lists every kb:// fact path in a text, in order, with where it is. A
 *  path's trailing dots and slashes (the end of a sentence) are not part of it. */
export function kbRefs(text: string): { from: number; to: number; ref: string }[] {
  const out: { from: number; to: number; ref: string }[] = [];
  for (const m of text.matchAll(KB_REF)) {
    const ref = m[0].replace(/[./]+$/, '');
    const from = m.index ?? 0;
    out.push({ from, to: from + ref.length, ref });
  }
  return out;
}

/**
 * dateAt is the date a number under a `*_at` key stands for, when `col` (an
 * offset into the line) is on that number: Unix seconds, or milliseconds for a
 * 13-digit one. Null anywhere else. The view's lines are pretty-printed, so a
 * property is one line: `"committed_at": 1794274206,`.
 */
export function dateAt(line: string, col: number): { from: number; to: number; date: Date } | null {
  const m = /^(\s*)"([^"\\]*_at)":\s*(\d{9,13})\b/.exec(line);
  if (!m) return null;
  const to = m[0].length;
  const from = to - m[3].length;
  if (col < from || col > to) return null;
  const n = Number(m[3]);
  return { from, to, date: new Date(m[3].length >= 13 ? n : n * 1000) };
}
