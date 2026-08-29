import { displayLensPath } from './utils';

/** What a fact is ABOUT, for the purpose of showing how unlike each other a
 *  motif's carriers are: the third segment of its path, `kb/<topic>/<SUBJECT>/…`.
 *
 *  Deliberately NOT the `domain` field, and the difference is not academic.
 *  Measured across this repository's knowledge base: `domain[0]` disagrees with
 *  this segment on 209 of 1,183 facts, 82 facts carry no domain at all, and
 *  taking every domain of one motif's 26 carriers yields 53 distinct labels —
 *  more variety than the list has rows, which reads as noise rather than as
 *  spread. `domain` is a multi-valued field with no first among equals; picking
 *  one is arbitrary and picking all of them over-disperses.
 *
 *  The path segment is single-valued, always present on a real fact, and is what
 *  every figure in the design was measured from.
 *
 *  Lens rows arrive qualified (`kb://<id12>/kb/...`), so the mount prefix is
 *  stripped first — otherwise the segments shift by one and every lens row
 *  reports its topic where a repo row reports its subject. */
export function factSubject(path: string): string {
  const segs = displayLensPath(path).split('/');
  // Fewer than four segments means root/topic/file — there is no subject level
  // yet. Returning '' lets a caller render nothing; returning `undefined` would
  // let one render the word "undefined".
  return segs.length >= 4 ? segs[2] : '';
}

export interface SubjectSummary {
  /** Whole names, most-carried first. Never truncated. */
  shown: string[];
  /** How many distinct subjects are not in `shown`. */
  more: number;
  /** Distinct subjects in the whole list — subjects, not facts. */
  total: number;
}

/** The "across a · b · c · +N more" line under a pivot heading.
 *
 *  Whole names or none, the same rule the fact header's motif cells follow. A
 *  subject is only a word and would survive clipping where a motif name would
 *  not — but one truncation rule beside a different one is how two surfaces
 *  drift apart, and the count carries real information that an ellipsis does
 *  not: how many areas were left out.
 *
 *  Ordered by carrier count then name. The name tie-break is the FacetPanel's,
 *  and it is load-bearing for the same reason: without it, two renders of the
 *  same list are free to disagree about which of two equally-common subjects
 *  leads.
 *
 *  Computed from the FULL landed rows, never from a cluster's capped `carriers`
 *  preview — the pivot's list IS the result, so this line can be complete where
 *  the panel's version of it is an approximation. */
export function subjectSummary(paths: string[], limit: number): SubjectSummary {
  const counts = new Map<string, number>();
  for (const p of paths) {
    const s = factSubject(p);
    // A path with no subject level is skipped, not counted as a blank one: a
    // '' entry would sort into the run and render as a gap between separators.
    if (!s) continue;
    counts.set(s, (counts.get(s) ?? 0) + 1);
  }
  const ranked = [...counts.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([name]) => name);
  return {
    shown: ranked.slice(0, limit),
    more: Math.max(0, ranked.length - limit),
    total: ranked.length,
  };
}
