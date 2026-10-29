// Materialize options: clamping, the URL they live in, the export filename,
// and what a change of options means for a stream already being read.

import { DEFAULT_OPTIONS, MAX_DEPTH, MAX_FACTS } from './materializeApi';
import type { MatOptions } from './materializeApi';

export function clampDepth(n: number): number {
  return Number.isFinite(n) ? Math.min(MAX_DEPTH, Math.max(0, Math.trunc(n))) : DEFAULT_OPTIONS.depth;
}
export function clampFacts(n: number): number {
  return Number.isFinite(n) ? Math.min(MAX_FACTS, Math.max(1, Math.trunc(n))) : DEFAULT_OPTIONS.maxFacts;
}

// The options live in the page's query string so a view can be shared. They are
// NOT persisted anywhere else: a fresh visit with no query gets the defaults.
// Only values that differ from the defaults are written, so the common view's
// URL stays short. `materialize=1` says the view is open.
const P_OPEN = 'materialize';
const P_DEPTH = 'mat_depth';
const P_FACTS = 'mat_facts';
const P_XKB = 'mat_cross_kb';
const P_UNPIN = 'mat_unpin';

export interface MatUrlState { open: boolean; options: MatOptions }

export function parseMaterializeParams(search: string): MatUrlState {
  const p = new URLSearchParams(search);
  const num = (k: string, d: number, clamp: (n: number) => number) => {
    const raw = p.get(k);
    return raw === null || raw.trim() === '' ? d : clamp(Number(raw));
  };
  return {
    open: p.get(P_OPEN) === '1',
    options: {
      depth: num(P_DEPTH, DEFAULT_OPTIONS.depth, clampDepth),
      maxFacts: num(P_FACTS, DEFAULT_OPTIONS.maxFacts, clampFacts),
      crossKb: p.get(P_XKB) === '1',
      unpin: p.get(P_UNPIN) === '1',
    },
  };
}

/** The query string (with its leading '?', or '' when empty) that carries s,
 *  keeping every parameter that is not ours. */
export function writeMaterializeParams(search: string, s: MatUrlState): string {
  const p = new URLSearchParams(search);
  for (const k of [P_OPEN, P_DEPTH, P_FACTS, P_XKB, P_UNPIN]) p.delete(k);
  if (s.open) {
    p.set(P_OPEN, '1');
    if (s.options.depth !== DEFAULT_OPTIONS.depth) p.set(P_DEPTH, String(s.options.depth));
    if (s.options.maxFacts !== DEFAULT_OPTIONS.maxFacts) p.set(P_FACTS, String(s.options.maxFacts));
    if (s.options.crossKb) p.set(P_XKB, '1');
    if (s.options.unpin) p.set(P_UNPIN, '1');
  }
  const q = p.toString();
  return q ? `?${q}` : '';
}

export function sameOptions(a: MatOptions, b: MatOptions): boolean {
  return a.depth === b.depth && a.maxFacts === b.maxFacts && a.crossKb === b.crossKb && a.unpin === b.unpin;
}

/** How a change of options meets a stream being read.
 *  - 'same': nothing to do.
 *  - 'continue': ONLY the count went up; the walk order is the same, so what
 *    is on screen stays and the stream carries on past it.
 *  - 'restart': anything else, which needs a new first call (a cursor refuses
 *    every other argument). That includes LOWERING the count. */
export function optionChange(prev: MatOptions, next: MatOptions): 'same' | 'continue' | 'restart' {
  if (sameOptions(prev, next)) return 'same';
  if (next.maxFacts > prev.maxFacts && prev.depth === next.depth && prev.crossKb === next.crossKb && prev.unpin === next.unpin) return 'continue';
  return 'restart';
}

export const SLUG_MAX = 48;

/** A title as a filename stem: lower-case ASCII words joined by '-', at most
 *  SLUG_MAX characters, cut at a word boundary when it has to be cut. */
export function slugTitle(title: string): string {
  const full = title.normalize('NFKD').replace(/[̀-ͯ]/g, '').toLowerCase()
    .replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  if (full.length <= SLUG_MAX) return full;
  let cut = full.slice(0, SLUG_MAX);
  if (full[SLUG_MAX] !== '-') {
    const at = cut.lastIndexOf('-');
    if (at > 0) cut = cut.slice(0, at);
  }
  return cut.replace(/-+$/, '');
}

/** The first 8 characters of a fact's id: its file name without ".md". */
export function id8(path: string): string {
  const base = path.split('/').pop() ?? path;
  return base.replace(/\.md$/, '').slice(0, 8);
}

/** `<slugged title, up to 48 chars>-<id8>.md` */
export function exportFilename(title: string, path: string): string {
  const slug = slugTitle(title) || 'materialized';
  return `${slug}-${id8(path)}.md`;
}

/** The heading level for a section at walk depth d: the root is an h2 (the
 *  view's own title is the h1), each hop a level down, h6 at the floor. */
export function headingLevel(depth: number): 2 | 3 | 4 | 5 | 6 {
  return Math.min(6, Math.max(2, 2 + Math.trunc(depth))) as 2 | 3 | 4 | 5 | 6;
}
