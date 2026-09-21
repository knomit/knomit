// experimentText holds the pure text helpers the experiment surfaces share:
// the band under the top bar, the panel on the repository page, and the
// refusal dialog. They live in a .ts module rather than beside either
// component because react-refresh/only-export-components forbids non-component
// exports from a component file (the same split the log viewer makes between
// logLines.ts and LogView.tsx).
//
// Every one of them takes the timestamp as the server sent it and is total: an
// absent or unparseable value yields a placeholder, never a thrown error and
// never a misleading date. That matters most for expiry, where ABSENT MEANS
// NEVER — the server omits expires_at when experiments.expiry_days is 0 rather
// than sending a far-future date, so a helper that invented one would state a
// policy the sweeper does not enforce.

/** relAge renders an ISO timestamp as a short human age. */
export function relAge(iso?: string): string {
  if (!iso) return '—';
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '—';
  const mins = Math.max(0, Math.round((Date.now() - then) / 60000));
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

/**
 * daysUntil is the whole days remaining before an expiry, or null when there
 * is no expiry at all. Null and 0 are different answers — null is "never
 * expires", 0 is "expires now" — so callers must not collapse them with a
 * falsy test.
 */
export function daysUntil(iso?: string): number | null {
  if (!iso) return null;
  const when = Date.parse(iso);
  if (Number.isNaN(when)) return null;
  return Math.ceil((when - Date.now()) / 86_400_000);
}

/** expiryShort is the panel column's text: 'in 28 days', 'now', or '—' for never. */
export function expiryShort(iso?: string): string {
  const days = daysUntil(iso);
  if (days === null) return '—';
  if (days <= 0) return 'now';
  return `in ${days} ${days === 1 ? 'day' : 'days'}`;
}

/**
 * expiryLong is the band's phrasing, which names the CONDITION as well as the
 * date: an experiment is swept for inactivity, and "expires in 28 days" alone
 * reads as a deadline you cannot move. Empty string when nothing expires, so
 * the band can drop the segment rather than print "never".
 */
export function expiryLong(iso?: string): string {
  const days = daysUntil(iso);
  if (days === null) return '';
  if (days <= 0) return 'expires now without a commit';
  return `expires in ${days} ${days === 1 ? 'day' : 'days'} without a commit`;
}

/** changedSinceFork is the band's count segment. */
export function changedSinceFork(n: number): string {
  return `${n} ${n === 1 ? 'fact' : 'facts'} changed since the fork`;
}
