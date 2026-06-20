import { useEffect, useState } from 'react';
import { api } from './api';

interface Entry {
  commit: string;
  date: string;
  message: string;
}

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  currentCommit: string;
  onScrub: (commit: string, isLatest: boolean) => void;
}

export function VersionWalker({ repo, branch, factPath, currentCommit, onScrub }: Props) {
  const [entries, setEntries] = useState<Entry[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api.factCommits(repo, branch, factPath)
      .then(r => {
        if (!cancelled) {
          setEntries(r.entries || []);
          setLoading(false);
        }
      })
      .catch(() => { if (!cancelled) { setEntries([]); setLoading(false); } });
    return () => { cancelled = true; };
  }, [repo, branch, factPath]);

  const n = entries.length;
  // In LIVE mode the HEAD fact read carries as_of.commit = the branch tip, which
  // is not one of this fact's own version commits, so an exact match fails. The
  // live view always shows the newest version, so fall back to idx 0 (newest)
  // when currentCommit isn't found.
  const found = entries.findIndex(e => e.commit === currentCommit);
  const idx = found >= 0 ? found : 0;

  // Position label: newest (idx=0) → "v{n} of N"; newest has highest version number.
  const posLabel = n > 0 ? `v${n - idx} of ${n}` : null;

  // prev = older = entries[idx+1]; next = newer = entries[idx-1]
  const canPrev = idx < n - 1;
  const canNext = idx > 0;

  const handlePrev = () => {
    if (!canPrev) return;
    const idxAfter = idx + 1;
    onScrub(entries[idxAfter].commit, idxAfter === 0);
  };

  const handleNext = () => {
    if (!canNext) return;
    const idxAfter = idx - 1;
    onScrub(entries[idxAfter].commit, idxAfter === 0);
  };

  if (loading) return null;
  // Nothing to navigate for a single-version fact.
  if (n <= 1) return null;

  return (
    <span
      data-testid="version-walker"
      style={{ display: 'inline-flex', alignItems: 'baseline', gap: 4, fontFamily: 'monospace', fontSize: 11 }}
    >
      <button
        data-testid="walker-prev"
        onClick={handlePrev}
        disabled={!canPrev}
        title="Previous (older) version"
        style={{
          background: 'none', border: 'none', padding: '0 2px',
          color: canPrev ? '#7c9' : '#444', cursor: canPrev ? 'pointer' : 'default',
          fontFamily: 'monospace', fontSize: 11,
        }}
      >← prev</button>
      {posLabel && (
        <span style={{ color: '#555', fontSize: 10 }}>{posLabel}</span>
      )}
      <button
        data-testid="walker-next"
        onClick={handleNext}
        disabled={!canNext}
        title="Next (newer) version"
        style={{
          background: 'none', border: 'none', padding: '0 2px',
          color: canNext ? '#7c9' : '#444', cursor: canNext ? 'pointer' : 'default',
          fontFamily: 'monospace', fontSize: 11,
        }}
      >next →</button>
    </span>
  );
}
