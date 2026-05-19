import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { HistoryEntryWithTags } from './api';
import type { Action } from './state';

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  currentCommit: string;
  dispatch: Dispatch<Action>;
}

export function VersionWalker({ repo, branch, factPath, currentCommit, dispatch }: Props) {
  const [versions, setVersions] = useState<HistoryEntryWithTags[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api.factCommits(repo, branch, factPath).then(r => {
      if (!cancelled) {
        setVersions(r.entries || []);
        setLoading(false);
      }
    }).catch(() => { if (!cancelled) { setVersions([]); setLoading(false); } });
    return () => { cancelled = true; };
  }, [repo, branch, factPath]);

  // versions are newest-first from the backend.
  const idx = versions.findIndex(v => v.commit === currentCommit);
  const total = versions.length;
  const versionDisplay = idx < 0 ? '?' : String(total - idx);  // 1 = oldest. Display vN of M.
  const isOldest = idx === total - 1;
  const isNewest = idx === 0;

  const handlePrev = () => {
    // older = larger idx
    const target = versions[idx + 1];
    if (target) {
      dispatch({ type: 'SET_AS_OF', asOf: { mode: 'scrubbed', commit: target.commit } });
    }
  };
  const handleNext = () => {
    const target = versions[idx - 1];
    if (!target) return;
    // If target is the newest (idx-1 === 0), go live.
    if (idx - 1 === 0) {
      dispatch({ type: 'SET_AS_OF', asOf: { mode: 'live' } });
    } else {
      dispatch({ type: 'SET_AS_OF', asOf: { mode: 'scrubbed', commit: target.commit } });
    }
  };
  const handleChipClick = () => {
    // Open Explain "on this fact" — anchor null = live HEAD so outgoing refs
    // reflect the fact's current state. Commit-anchored outgoing returns the
    // refs *written at that exact commit*, which is empty for path-touching
    // commits that didn't author the fact (merges, re-indexing). Specific
    // versions are still reachable via the row clicks in the history panel.
    dispatch({ type: 'OPEN_EXPLAIN', path: factPath, commit: null });
  };

  const prevDisabled = isOldest || total < 2 || idx < 0;
  const nextDisabled = isNewest || total < 2 || idx < 0;

  return (
    <span data-testid="version-walker" style={{ display: 'inline-flex', alignItems: 'center', gap: 6, fontFamily: 'monospace', fontSize: 11 }}>
      <button
        data-testid="walker-prev"
        disabled={prevDisabled}
        onClick={handlePrev}
        style={{
          background: 'none', border: 'none',
          color: prevDisabled ? '#444' : '#888',
          cursor: prevDisabled ? 'default' : 'pointer',
          padding: '0 4px',
        }}
      >← prev</button>
      <span style={{ color: '#aaa' }}>
        v{versionDisplay} of {loading ? '…' : total}
      </span>
      <button
        data-testid="walker-next"
        disabled={nextDisabled}
        onClick={handleNext}
        style={{
          background: 'none', border: 'none',
          color: nextDisabled ? '#444' : '#888',
          cursor: nextDisabled ? 'default' : 'pointer',
          padding: '0 4px',
        }}
      >next →</button>
      <span style={{ color: '#333' }}>│</span>
      {currentCommit && (
        <button
          data-testid="walker-commit-chip"
          onClick={handleChipClick}
          style={{
            color: '#7c9', background: '#1a2e1a', padding: '1px 5px', borderRadius: 3,
            cursor: 'pointer', userSelect: 'none', border: 'none',
            fontFamily: 'monospace', fontSize: 11,
          }}
        >{currentCommit.slice(0, 7)}</button>
      )}
    </span>
  );
}
