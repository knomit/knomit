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
