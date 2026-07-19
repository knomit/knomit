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

export const opStyles: Record<string, { color: string; bg: string; label: string }> = {
  learn:   { color: '#7c9', bg: '#1a2e1a', label: 'learn' },
  update:  { color: '#8af', bg: '#1a1a2e', label: 'update' },
  retract: { color: '#f88', bg: '#2e1a1a', label: 'retract' },
  subsume: { color: '#fa0', bg: '#2e2a1a', label: 'subsume' },
  sync:    { color: '#888', bg: '#222',    label: 'sync' },
  other:   { color: '#666', bg: '#1a1a1a', label: 'other' },
};

export const defaultOpStyle = { color: '#555', bg: '#222', label: '' };

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
  policy:       { color: '#f9b4', bg: '#2e1f1a', label: 'policy',     icon: '⚖' },
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

export const chipColors: Record<string, { bg: string; text: string; close: string }> = {
  domain: { bg: '#2a3a2a', text: '#7c9', close: '#5a7a5a' },
  entity: { bg: '#3a2a2a', text: '#f8a', close: '#8a5a5a' },
  type:   { bg: '#2a2a3a', text: '#8af', close: '#5a5a8a' },
  kind:   { bg: '#3a2a1a', text: '#fc7', close: '#8a6a3a' },
  origin: { bg: '#1a3434', text: '#7dd', close: '#4a8a8a' },
  ep:     { bg: '#3a3a2a', text: '#fa8', close: '#8a7a5a' },
  path:   { bg: '#333',   text: '#aaa', close: '#666' },
};
