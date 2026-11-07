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
  onScrub: (commit: string) => void;
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
  // Open history at the newest version commit. Scrubbing always enters history
  // mode — returning to live is the timeline's return-to-live control.
  const openHistory = () => onScrub(entries[0].commit);

  return (
    <button
      data-testid="version-walker"
      onClick={openHistory}
      title="Open history"
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4,
        // NO BORDER OF ITS OWN. This sits inside the fact header's control
        // strip, which draws the border and the dividers; a chip border here
        // would nest one inside the other. The strip's cells are separated by
        // hairlines, not by each child re-stating its own outline.
        background: 'none', border: 'none', borderRadius: 0,
        outline: 'none',
        padding: '2px 7px', cursor: 'pointer',
        color: '#7c9', fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1.4,
      }}
    >
      <span aria-hidden="true" style={{ fontSize: 10, opacity: 0.7 }}>⏱</span>
      v{version}
    </button>
  );
}
