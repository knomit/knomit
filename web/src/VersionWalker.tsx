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
  const idx = entries.findIndex(e => e.commit === currentCommit);

  // Position label: newest (idx=0) → "v1 of N"; idx+1 counts from newest.
  const posLabel = idx >= 0 && n > 0 ? `v${idx + 1} of ${n}` : null;

  // prev = older = entries[idx+1]; next = newer = entries[idx-1]
  const canPrev = idx >= 0 && idx < n - 1;
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
