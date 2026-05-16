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
  hypothesis:   { color: '#f8a', bg: '#2e1a2a', label: 'hypothesis',  icon: '?' },
  methodology:  { color: '#af8', bg: '#1a2e2a', label: 'methodology', icon: '⚙' },
  // Pragmatic (prescriptive — "what to do")
  policy:       { color: '#f9b4', bg: '#2e1f1a', label: 'policy',     icon: '⚖' },
  heuristic:    { color: '#fc7', bg: '#2e2614', label: 'heuristic',   icon: '☼' },
};

export const defaultTypeStyle = { color: '#666', bg: '#1a1a1a', label: 'unknown', icon: '·' };

export const chipColors: Record<string, { bg: string; text: string; close: string }> = {
  domain: { bg: '#2a3a2a', text: '#7c9', close: '#5a7a5a' },
  entity: { bg: '#3a2a2a', text: '#f8a', close: '#8a5a5a' },
  type:   { bg: '#2a2a3a', text: '#8af', close: '#5a5a8a' },
  kind:   { bg: '#3a2a1a', text: '#fc7', close: '#8a6a3a' },
  ep:     { bg: '#3a3a2a', text: '#fa8', close: '#8a7a5a' },
  path:   { bg: '#333',   text: '#aaa', close: '#666' },
};
