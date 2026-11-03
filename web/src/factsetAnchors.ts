// Where a FactSet link leads, and what its stub says. Pure: no React, no fetch.
//
// A FactSet writes each fact ONCE; every other appearance is a heading-only
// link stub. Ruling 2026-11-02 (replacing Q-UI7): a stub, forward or back, shows
// "see" and a very short form of the target's TITLE, never a number ("Users
// really do not care about the numbers"). A click scrolls to the target when it
// is in the loaded run, and otherwise seeks to the link's unit (forward links
// name the unit where the fact will be written, back links the unit that first
// wrote it) and then scrolls to its anchor.
//
// Anchors are `f-<16 hex>` of FactHash(repo, path): the repo is part of the
// hash, so on a lens (anchors per member) one map keyed by anchor_id is enough.

import type { FactSetLink, FactSetNode } from './factsetApi';
import { NESTING, clipTitle } from './materializeOptions';
import type { PathItem } from './materializeOptions';

/** How many characters of the target's title a link stub keeps.
 *  PLACEHOLDER: the user asked for "a very short form" and set no length. */
export const STUB_TITLE_CHARS = 32; // PLACEHOLDER copy

/** What a cut title ends with. PLACEHOLDER copy (the user wrote "..."). */
export const STUB_ELLIPSIS = '…'; // PLACEHOLDER copy

/** The stub's label: "see" (the user's word) and the target's title, cut to
 *  STUB_TITLE_CHARS at a word boundary when it can be. */
export function stubLabel(title: string): string {
  const t = title.trim();
  if (t.length <= STUB_TITLE_CHARS) return `see ${t}`;
  let cut = t.slice(0, STUB_TITLE_CHARS);
  const sp = cut.lastIndexOf(' ');
  if (sp > STUB_TITLE_CHARS / 2) cut = cut.slice(0, sp);
  return `see ${cut.trimEnd()}${STUB_ELLIPSIS}`;
}

/** What a click on a link does. */
export type LinkMove =
  | { kind: 'scroll'; anchor: string }
  | { kind: 'seek'; unit: number; anchor: string }
  | { kind: 'none' };

/** Every written section ever loaded in this FactSet: anchor -> its unit. It
 *  outlives the run on screen (a seek replaces the run), so a target read
 *  earlier is still known by unit. */
export class FactSetAnchors {
  private units = new Map<string, number>();

  /** Records a node that is a scroll target: written, or an unavailable root. */
  register(n: FactSetNode): void {
    if (n.written || n.unavailable) this.units.set(n.anchor_id, n.unit);
  }

  unitOf(anchor: string): number | undefined {
    return this.units.get(anchor);
  }

  /** Where clicking link goes. onScreen says whether an anchor's written
   *  section is in the run being shown. */
  resolve(link: FactSetLink, onScreen: (anchor: string) => boolean): LinkMove {
    if (onScreen(link.anchor_id)) return { kind: 'scroll', anchor: link.anchor_id };
    const unit = link.unit ?? this.units.get(link.anchor_id);
    if (unit && unit > 0) return { kind: 'seek', unit, anchor: link.anchor_id };
    return { kind: 'none' };
  }
}

// ── The FactSet view's sticky path and its Recent-list notice ───────────────

/** The unit root's title in the sticky path is clipped to this many
 *  characters (the book prototype's PATH_UNIT_CHARS): long enough to
 *  recognise the fact, short enough that the chain keeps room at 900px. */
export const PATH_UNIT_CHARS = 28;

/** The notice when the list holds more results than the FactSet (user ruling
 *  2026-11-03 (c): a Recent list is not capped; say so). PLACEHOLDER copy. */
export function moreThanFactSet(units: number, listTotal: number): string {
  const f = (x: number) => x.toLocaleString('en-US');
  return `Showing the first ${f(units)} of ${f(listTotal)} results.`; // PLACEHOLDER copy
}

/** The sticky path of section (unit, n): the unit root's title first (clipped
 *  to PATH_UNIT_CHARS, no number: the progress names the unit), the
 *  ancestors, and the section itself last; numbers as the headings show them.
 *  On the unit root itself the path is the root alone. */
export function fsPathTo(unit: number, n: number, byUnitN: Map<string, FactSetNode>): PathItem[] {
  const chain: FactSetNode[] = [];
  const seen = new Set<number>();
  for (let c = byUnitN.get(`${unit}.${n}`); c && !seen.has(c.n); c = c.tree_depth > 0 ? byUnitN.get(`${unit}.${c.parent || 1}`) : undefined) {
    seen.add(c.n);
    chain.unshift(c);
  }
  return chain.map(c => {
    const t = c.title || c.path;
    if (c.tree_depth === 0) return { n: c.n, unit, full: t, label: clipTitle(t, PATH_UNIT_CHARS) };
    const num = c.number && c.tree_depth <= NESTING.NUMBER_LEVELS ? `${c.number} ` : '';
    return { n: c.n, unit, full: `${c.number ? `${c.number} ` : ''}${t}`, label: num + clipTitle(t, NESTING.PATH_TITLE_CHARS) };
  });
}
