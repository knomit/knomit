// Materialize options: clamping, the URL they live in, the export filename,
// and what a change of options means for a stream already being read.

import { DEFAULT_OPTIONS, MAX_DEPTH, MAX_REFERENCES } from './materializeApi';
import type { MatOptions } from './materializeApi';

export function clampDepth(n: number): number {
  return Number.isFinite(n) ? Math.min(MAX_DEPTH, Math.max(0, Math.trunc(n))) : DEFAULT_OPTIONS.depth;
}
export function clampReferences(n: number): number {
  return Number.isFinite(n) ? Math.min(MAX_REFERENCES, Math.max(0, Math.trunc(n))) : DEFAULT_OPTIONS.references;
}

// The options live in the page's query string so a view can be shared. They are
// NOT persisted anywhere else: a fresh visit with no query gets the defaults.
// Only values that differ from the defaults are written, so the common view's
// URL stays short. `materialize=1` says the view is open.
const P_OPEN = 'materialize';
const P_DEPTH = 'mat_depth';
const P_REFS = 'mat_refs';
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
      references: num(P_REFS, DEFAULT_OPTIONS.references, clampReferences),
      crossKb: p.get(P_XKB) === '1',
      unpin: p.get(P_UNPIN) === '1',
    },
  };
}

/** The query string (with its leading '?', or '' when empty) that carries s,
 *  keeping every parameter that is not ours. */
export function writeMaterializeParams(search: string, s: MatUrlState): string {
  const p = new URLSearchParams(search);
  for (const k of [P_OPEN, P_DEPTH, P_REFS, P_XKB, P_UNPIN]) p.delete(k);
  if (s.open) {
    p.set(P_OPEN, '1');
    if (s.options.depth !== DEFAULT_OPTIONS.depth) p.set(P_DEPTH, String(s.options.depth));
    if (s.options.references !== DEFAULT_OPTIONS.references) p.set(P_REFS, String(s.options.references));
    if (s.options.crossKb) p.set(P_XKB, '1');
    if (s.options.unpin) p.set(P_UNPIN, '1');
  }
  const q = p.toString();
  return q ? `?${q}` : '';
}

export function sameOptions(a: MatOptions, b: MatOptions): boolean {
  return a.depth === b.depth && a.references === b.references && a.crossKb === b.crossKb && a.unpin === b.unpin;
}

/** How a change of options meets a stream being read.
 *  - 'same': nothing to do.
 *  - 'continue': ONLY the count went up; the walk order is the same, so what
 *    is on screen stays and the stream carries on past it.
 *  - 'restart': anything else, which needs a new first call (a cursor refuses
 *    every other argument). That includes LOWERING the count. */
export function optionChange(prev: MatOptions, next: MatOptions): 'same' | 'continue' | 'restart' {
  if (sameOptions(prev, next)) return 'same';
  if (next.references > prev.references && prev.depth === next.depth && prev.crossKb === next.crossKb && prev.unpin === next.unpin) return 'continue';
  return 'restart';
}

export const SLUG_MAX = 48;

/** A title as a filename stem: lower-case ASCII words joined by '-', at most
 *  SLUG_MAX characters, cut at a word boundary when it has to be cut. */
export function slugTitle(title: string): string {
  // Same rule as the server's materialize.ExportFileName (its Content-Disposition
  // name): other letters are separators, not transliterated.
  const full = title.toLowerCase()
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
  const slug = slugTitle(title);
  return slug ? `${slug}-${id8(path)}.md` : `${id8(path)}.md`;
}

/** The heading level for a section at TREE depth d (not the payload's BFS `depth`): the root is
 *  an h2 (the view's own title is the h1), each level of nesting one down, h6 at the floor. */
export function headingLevel(depth: number): 2 | 3 | 4 | 5 | 6 {
  return Math.min(6, Math.max(2, 2 + Math.trunc(depth))) as 2 | 3 | 4 | 5 | 6;
}

// ── Nesting and the sticky section path (shared by both materialize views) ──

/** The knobs of the nesting presentation (from the deep-nesting prototype, variant C). */
export const NESTING = {
  /** Only tree depths 1..NUMBER_LEVELS print a number ("1.", "1.1", "1.1.1"). */
  NUMBER_LEVELS: 3,
  /** Most ticks in the depth gutter; a deeper section shows this many, the last in the cap colour. */
  GUTTER_CAP: 6,
  /** The heading ladder: level = tree depth + 1; levels past this repeat its style. */
  LADDER_MAX_LEVEL: 5,
  /** Each title in the section path is clipped to this many characters. */
  PATH_TITLE_CHARS: 30,
  /** The path's character budget (also limited by the pane width). */
  PATH_MAX_CHARS: 190,
  /** First ancestors that survive the path's middle truncation. */
  PATH_KEEP_HEAD: 1,
  /** Average glyph width at the path's 11px size, to turn pane width into characters. */
  CHAR_PX: 6.3,
  /** Characters a " › " separator costs. */
  SEP_COST: 3,
} as const;

/** One item of the section path: the section's ordinal, its full label (the
 *  tooltip), the clipped label shown, or the "…" gap of a middle truncation. */
export interface PathItem { n: number; unit?: number; full: string; label: string; gap?: boolean }

export const clipTitle = (t: string, n: number) => (t.length > n ? t.slice(0, n - 1).trimEnd() + '…' : t);

/** Middle-truncate a path: keep the first PATH_KEEP_HEAD items and as many of the
 *  last as fit the budget (always the last one), with a gap marker between. */
export function midTruncate(items: PathItem[], budget: number): PathItem[] {
  const cost = (i: PathItem) => i.label.length + NESTING.SEP_COST;
  const total = items.reduce((t, i) => t + cost(i), 0);
  const keep = NESTING.PATH_KEEP_HEAD;
  if (total <= budget || items.length <= keep + 1) return items;
  const head = items.slice(0, keep);
  let used = head.reduce((t, i) => t + cost(i), 0) + 1 + NESTING.SEP_COST;
  const tail: PathItem[] = [];
  for (let i = items.length - 1; i >= keep; i--) {
    const c = cost(items[i]);
    if (used + c > budget && tail.length) break;
    tail.unshift(items[i]);
    used += c;
  }
  // Nothing was dropped after all: no gap.
  if (head.length + tail.length === items.length) return items;
  return [...head, { n: -1, full: '', label: '…', gap: true }, ...tail];
}

/** The path's character budget for a pane `width` px wide. */
export function pathBudget(width: number): number {
  return Math.max(20, Math.min(NESTING.PATH_MAX_CHARS, Math.floor((width - 90) / NESTING.CHAR_PX)));
}

// The unit ribbon of a FactSet unit root (book design §5).
/** The ribbon's width: a base (padding and the point) plus one mono glyph per
 *  digit (6.6px = 0.6em at 11px), so "130" (30px) fits the 44px gutter and the
 *  34px narrow one. The book prototype's numbers. */
export const RIBBON = { BASE_PX: 10, DIGIT_PX: 6.6 } as const;
export function ribbonWidth(unit: number): number {
  return Math.round((RIBBON.BASE_PX + RIBBON.DIGIT_PX * String(unit).length) * 10) / 10;
}
