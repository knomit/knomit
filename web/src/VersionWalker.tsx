import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { HistoryEntryWithTags } from './api';
import type { Action } from './state';

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  currentCommit: string | null;
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
  const idx = currentCommit ? versions.findIndex(v => v.commit === currentCommit) : -1;
  const total = versions.length;
  const versionNumber = total - idx;          // 1 = oldest. Display vN of M.
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
    if (currentCommit) {
      dispatch({ type: 'OPEN_COMMIT_DRAWER', commit: currentCommit });
    }
  };

  return (
    <span data-testid="version-walker" style={{ display: 'inline-flex', alignItems: 'center', gap: 6, fontFamily: 'monospace', fontSize: 11 }}>
      <button
        data-testid="walker-prev"
        disabled={isOldest || total < 2 || idx < 0}
        onClick={handlePrev}
        style={{ background: 'none', border: 'none', color: '#888', cursor: 'pointer', padding: '0 4px' }}
      >← prev</button>
      <span style={{ color: '#aaa' }}>
        v{versionNumber > 0 ? versionNumber : '?'} of {loading ? '…' : total}
      </span>
      <button
        data-testid="walker-next"
        disabled={isNewest || total < 2 || idx < 0}
        onClick={handleNext}
        style={{ background: 'none', border: 'none', color: '#888', cursor: 'pointer', padding: '0 4px' }}
      >next →</button>
      <span style={{ color: '#333' }}>│</span>
      {currentCommit && (
        <span
          data-testid="walker-commit-chip"
          onClick={handleChipClick}
          style={{
            color: '#7c9', background: '#1a2e1a', padding: '1px 5px', borderRadius: 3,
            cursor: 'pointer', userSelect: 'none',
          }}
        >{currentCommit.slice(0, 7)}</span>
      )}
    </span>
  );
}
