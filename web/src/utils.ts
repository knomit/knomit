/** Format a date string (ISO) as a relative time label. */
export function relativeTime(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(dateStr).toLocaleDateString();
}

/** Format a unix epoch (seconds) as a relative time label. */
export function relativeTimeEpoch(epoch: number): string {
  if (!epoch) return '';
  return relativeTime(new Date(epoch * 1000).toISOString());
}

/** Fact type visual styles. Epistemic types use a cool palette (descriptive);
 *  pragmatic types use a warm palette to read as prescriptive at a glance. */
export const typeStyles: Record<string, { color: string; bg: string; label: string; icon: string }> = {
  // Epistemic (descriptive — "what is")
  observation:  { color: '#7c9', bg: '#1a2e1a', label: 'observation',  icon: '◉' },
  concept:      { color: '#8af', bg: '#1a1a2e', label: 'concept',      icon: '◎' },
  process:      { color: '#8cf', bg: '#1a2a2e', label: 'process',      icon: '⟳' },
  principle:    { color: '#da8', bg: '#2e2a1a', label: 'principle',    icon: '◈' },
  pattern:      { color: '#c8f', bg: '#2a1a2e', label: 'pattern',      icon: '⬡' },
  reference:    { color: '#888', bg: '#222',    label: 'reference',    icon: '▤' },
  synthesis:    { color: '#fa0', bg: '#2e2a1a', label: 'synthesis',    icon: '◆' },
  insight:      { color: '#ffcc33', bg: '#2e2814', label: 'insight',  icon: '✸' },
  hypothesis:   { color: '#f8a', bg: '#2e1a2a', label: 'hypothesis',  icon: '?' },
  methodology:  { color: '#af8', bg: '#1a2e2a', label: 'methodology', icon: '⚙' },
  // Pragmatic (prescriptive — "what to do")
  policy:       { color: '#f97',  bg: '#2e1f1a', label: 'policy',     icon: '⚖' },
  heuristic:    { color: '#fc7', bg: '#2e2614', label: 'heuristic',   icon: '☼' },
};

export const defaultTypeStyle = { color: '#666', bg: '#1a1a1a', label: 'unknown', icon: '·' };

/** Lens design tokens (verbatim from the design handoff). Consumed by the
 *  lens badges, dots, checkboxes and dropdown across the lens UI. */
export const LENS = {
  accent: '#a8a4f0',
  bg: '#1c1a2e',
  border: '#38345c',
  soft: '#231f38',
  text: '#141230',
} as const;

/** djb2 string hash → unsigned 32-bit. Stable and deterministic across
 *  calls and sessions (pure function of the input). */
function hashString(s: string): number {
  let h = 5381;
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) + h + s.charCodeAt(i)) >>> 0;
  }
  return h;
}

/** HSL (h in [0,360), s/l in [0,1]) → lowercase `#rrggbb`. */
function hslToHex(h: number, s: number, l: number): string {
  const c = (1 - Math.abs(2 * l - 1)) * s;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = l - c / 2;
  let r = 0, g = 0, b = 0;
  if (h < 60) { r = c; g = x; }
  else if (h < 120) { r = x; g = c; }
  else if (h < 180) { g = c; b = x; }
  else if (h < 240) { g = x; b = c; }
  else if (h < 300) { r = x; b = c; }
  else { r = c; b = x; }
  const hex = (v: number) => Math.round((v + m) * 255).toString(16).padStart(2, '0');
  return `#${hex(r)}${hex(g)}${hex(b)}`;
}

/** Deterministic per-repo accent hue. Pure function of the repo name — stable
 *  across calls and sessions. Fixed mid saturation / mid-high lightness keeps
 *  the result a muted pastel that stays readable on the dark UI (#141414). */
export function repoHue(name: string): string {
  const hue = hashString(name) % 360;
  return hslToHex(hue, 0.5, 0.66);
}

/** Repo hue as a translucent badge fill (hue + '1f' alpha, 8-digit hex). */
export function repoHueBg(name: string): string {
  return repoHue(name) + '1f';
}

/** Repo hue as a translucent badge border (hue + '44' alpha, 8-digit hex). */
export function repoHueBorder(name: string): string {
  return repoHue(name) + '44';
}

/** displayLensPath strips the `kb://<id12>/` qualifier from a read-mount lens
 *  path so the displayed breadcrumb never shows the opaque mount id — the
 *  source badge already names the mount. Bare write-repo paths pass through
 *  unchanged. Shared by the Library union list and the RightPanel meta line. */
export function displayLensPath(path: string): string {
  return path.replace(/^kb:\/\/[^/]+\//, '');
}

/** rowPath is the part of a fact's path a list row still has to say.
 *
 *  Every row answers "where is this fact", and the panel header answers part of
 *  it already. Repeating the answered part is what made the two lens views
 *  disagree inside a directory: the tree row printed nothing under the title
 *  while the Recent row printed the breadcrumb again, verbatim, on all ten rows
 *  — the same location stated eleven times. Same location, same list, opposite
 *  failures, because each view had decided independently.
 *
 *  So: strip the mount qualifier, then strip the directory the header names.
 *  At the ontology root the header says "All facts" and names no location, so
 *  nothing is stripped and the row carries the whole path — which is the state
 *  the flat union list is almost always in.
 *
 *  The cut is on `dir + '/'`, never the bare prefix: ".../supply-chain-notes"
 *  starts with ".../supply-chain" as a string but is a different directory, and
 *  cutting loosely would leave a row reading "-notes/x.md". */
export function rowPath(path: string, dir: string, ontologyRoot: string): string {
  const shown = displayLensPath(path);
  if (!dir || dir === ontologyRoot) return shown;
  const prefix = dir.endsWith('/') ? dir : dir + '/';
  if (!shown.startsWith(prefix)) return shown;
  // A row must always name itself; an exact match on the directory would
  // otherwise blank the line.
  return shown.slice(prefix.length) || shown;
}

/** shortBranch trims an agent branch to the part that identifies the machine.
 *
 *  identity.go builds them as `agent/<sanitized-hostname>-<fp8>`, where the
 *  prefix is constant across every agent branch and fp8 is the SSH key
 *  fingerprint — which only tells two agents on the SAME host apart. So the
 *  hostname is the only informative part, and it is the part that fits: 68px
 *  against 196px for the whole string. Callers keep the full name in `title`.
 *
 *  Only a trailing `-<exactly 8 hex>` is treated as a fingerprint. Hostnames
 *  routinely contain hyphens of their own — sanitizeHostname replaces spaces
 *  and git-illegal characters with them — so a greedy cut would eat the name.
 *  Anything that is not an agent branch passes through untouched. */
export function shortBranch(branch: string): string {
  // The trim is gated on the prefix, not just applied after stripping it: only
  // an agent branch has a fingerprint to drop. `hotfix-20260807` ends in eight
  // hex digits by coincidence, and an ungated cut would show it as `hotfix`.
  if (!branch.startsWith('agent/')) return branch;
  const after = branch.slice('agent/'.length);
  // A prefix with nothing after it is not an agent branch in any useful sense;
  // returning '' would blank the slot where a branch name has to appear.
  if (!after) return branch;
  return after.replace(/-[0-9a-f]{8}$/, '');
}

/** Provenance glyphs, keyed by fact origin. Shared by the fact body's origin
 *  ghost chip, the filter picker's Origin rows and the chip they produce — one
 *  definition so the three cannot drift apart. `authored` is the default and is
 *  elided on the wire, so it rarely renders. */
export const originGlyphs: Record<string, string> = {
  authored: '✎',
  distilled: '⚗',
  discovered: '◇',
};

// `entity` is blue because entities are blue EVERYWHERE else — the summary's
// Entities column and the fact body's tag cloud both use 136,170,255, which is
// what #8af expands to. It reads as a colour change here only because this
// palette was the outlier. The blue was free to take because `type` no longer
// holds a single colour: see chipStyle.
export const chipColors: Record<string, { bg: string; text: string; close: string }> = {
  domain: { bg: '#2a3a2a', text: '#7c9', close: '#5a7a5a' },
  entity: { bg: '#2a2a3a', text: '#8af', close: '#5a5a8a' },
  // Fallback for a type with no typeStyles entry; a known type never reaches it.
  type:   { bg: '#222',    text: '#888', close: '#666' },
  kind:   { bg: '#3a2a1a', text: '#fc7', close: '#8a6a3a' },
  origin: { bg: '#1a3434', text: '#7dd', close: '#4a8a8a' },
  ep:     { bg: '#3a3a2a', text: '#fa8', close: '#8a7a5a' },
  path:   { bg: '#333',   text: '#aaa', close: '#666' },
};

/** The visual for one filter value — the chip it becomes, and the row that
 *  offers it in the picker, drawn from ONE place so the two always agree.
 *
 *  Category-coloured, with two exceptions that are per-VALUE because the value
 *  already owns a look elsewhere in the app:
 *
 *  - `type` takes that type's own colour, background and glyph from typeStyles
 *    — the same ones a Library row and the summary's Types column wear. One
 *    blue shared by all twelve types said less than the glyph already says,
 *    and holding that blue was what kept `entity` pink.
 *  - `origin` keeps the category colour but carries its provenance glyph.
 *
 *  `repo` is deliberately NOT here: its hue is computed from the name
 *  (repoHue), so the caller builds it. */
export function chipStyle(category: string, value: string):
  { bg: string; text: string; close: string; glyph?: string } {
  if (category === 'type') {
    const ts = typeStyles[value];
    if (ts) return { bg: ts.bg, text: ts.color, close: ts.color, glyph: ts.icon };
  }
  if (category === 'origin' && originGlyphs[value]) {
    return { ...chipColors.origin, glyph: originGlyphs[value] };
  }
  return chipColors[category] || chipColors.path;
}

// Edge direction presentation. Lives here rather than beside ConnectionsCell
// because a module that exports both a component and constants breaks fast
// refresh — and these are consumed by the panel as well as the cell.
export type EdgeDir = 'in' | 'out';
export const EDGE_ACCENT: Record<EdgeDir, string> = { in: '#8af', out: '#fa8' };
export const EDGE_GLYPH: Record<EdgeDir, string> = { in: '↙', out: '↗' };
// A direction whose edges could not be fetched, on the cell and in the panel
// body. Shared so the warning in the header and the message it opens read as
// the same failure rather than two unrelated red things.
export const EDGE_ERROR = '#f66';

/**
 * noMouseFocus stops a MOUSE press from focusing a control, while leaving
 * keyboard focus completely intact.
 *
 * `:focus-visible` is specified to match the ALREADY-FOCUSED element as soon as
 * the user touches the keyboard. So a button you clicked keeps DOM focus, and
 * the next keypress — Escape to leave Manage, `/` to search, an arrow to move
 * the list — retroactively paints a focus ring around it. The control you
 * clicked a moment ago lights up for a reason that has nothing to do with it.
 *
 * Preventing mousedown's default suppresses only the focus, not the click, so
 * the handler still runs. Tab still focuses these controls and still shows the
 * ring, which is the case the ring exists for.
 *
 * For SELECTION controls specifically — rail rows, contents-rail entries, the
 * Manage toggle — the selected state is already drawn, so a focus ring on top
 * of it says nothing the highlight has not.
 */
export const noMouseFocus = (e: React.MouseEvent) => e.preventDefault();
