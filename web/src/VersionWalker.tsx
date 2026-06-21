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

// A single always-available control that opens the fact's history. Clicking it
// enters history mode anchored at the fact's NEWEST version commit, which
// rotates the left rail to the TimelineNav version picker. There is no
// prev/next stepping — the timeline is the picker. Rendered for single-version
// facts too, so their history (and the other facts in the same commit) is
// reachable.
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

  if (loading) return null;
  // Nothing to anchor at for a fact with no version commits.
  if (n === 0) return null;

  // Position label: newest (idx=0) → "v{n}"; newest has the highest version.
  const version = n - idx;
  // Open history at the newest version commit; isLatest=false keeps the anchor
  // in history mode (true would demote to live).
  const openHistory = () => onScrub(entries[0].commit, false);

  return (
    <button
      data-testid="version-walker"
      onClick={openHistory}
      title="Open history"
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4,
        background: 'none', border: '1px solid #333', borderRadius: 4,
        outline: 'none',
        padding: '1px 7px', cursor: 'pointer',
        color: '#7c9', fontFamily: 'monospace', fontSize: 11,
      }}
    >
      <span aria-hidden="true" style={{ fontSize: 10, opacity: 0.7 }}>⏱</span>
      v{version}
    </button>
  );
}
