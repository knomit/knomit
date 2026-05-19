import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { Action } from './state';

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  currentCommit: string;
  dispatch: Dispatch<Action>;
}

export function VersionWalker({ repo, branch, factPath, currentCommit, dispatch }: Props) {
  const [count, setCount] = useState(0);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api.factCommits(repo, branch, factPath).then(r => {
      if (!cancelled) {
        setCount((r.entries || []).length);
        setLoading(false);
      }
    }).catch(() => { if (!cancelled) { setCount(0); setLoading(false); } });
    return () => { cancelled = true; };
  }, [repo, branch, factPath]);

  const handleChipClick = () => {
    // Anchor null = live HEAD; specific versions are browsable inside Explain
    // via the history panel's row clicks.
    dispatch({ type: 'OPEN_EXPLAIN', path: factPath, commit: null });
  };

  return (
    <span data-testid="version-walker" style={{ display: 'inline-flex', alignItems: 'baseline', gap: 6, fontFamily: 'monospace', fontSize: 11 }}>
      <button
        data-testid="walker-commit-chip"
        onClick={handleChipClick}
        title="Open Explain"
        style={{
          color: '#7c9', background: '#1a2e1a', padding: '1px 6px', borderRadius: 3,
          cursor: 'pointer', userSelect: 'none', border: 'none',
          fontFamily: 'monospace', fontSize: 11,
        }}
      >{currentCommit.slice(0, 7)}</button>
      {!loading && count > 1 && (
        <span data-testid="walker-version-count" style={{ color: '#555', fontSize: 10 }}>
          {count}v
        </span>
      )}
    </span>
  );
}
