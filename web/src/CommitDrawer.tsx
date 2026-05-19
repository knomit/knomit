import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { CommitDetail } from './api';
import type { AppState, Action } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function CommitDrawer({ state, dispatch }: Props) {
  const { commitDrawer } = state;
  const [detail, setDetail] = useState<CommitDetail | null>(null);

  // Close on Escape — only when open.
  useEffect(() => {
    if (!commitDrawer.open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') dispatch({ type: 'CLOSE_COMMIT_DRAWER' });
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [commitDrawer.open, dispatch]);

  // Fetch commit detail when the open commit changes.
  useEffect(() => {
    if (!commitDrawer.open) { setDetail(null); return; }
    let cancelled = false;
    api.commitDetail(state.repo, state.branch, commitDrawer.commit).then(d => {
      if (!cancelled) setDetail(d);
    }).catch(() => { if (!cancelled) setDetail(null); });
    return () => { cancelled = true; };
  }, [commitDrawer.open && commitDrawer.commit, state.repo, state.branch]);

  if (!commitDrawer.open) return null;

  return (
    <div
      data-testid="commit-drawer"
      style={{
        position: 'fixed', top: 0, right: 0, bottom: 0, width: 420,
        background: '#0f0f0f', borderLeft: '1px solid #1a1a1a', zIndex: 50,
        display: 'flex', flexDirection: 'column', overflow: 'hidden',
        fontFamily: 'system-ui, sans-serif', color: '#ddd',
      }}
    >
      <div style={{ padding: '12px 14px', borderBottom: '1px solid #1a1a1a', display: 'flex', alignItems: 'center', gap: 8 }}>
        <span style={{ fontFamily: 'monospace', fontSize: 12 }}>{commitDrawer.commit.slice(0, 7)}</span>
        <div style={{ flex: 1 }} />
        <button
          data-testid="drawer-close"
          onClick={() => dispatch({ type: 'CLOSE_COMMIT_DRAWER' })}
          style={{ background: 'none', border: 'none', color: '#888', cursor: 'pointer', fontSize: 11 }}
        >esc ✕</button>
      </div>
      <div style={{ flex: 1, overflowY: 'auto', padding: '12px 14px' }}>
        {detail ? (
          <>
            <div data-testid="drawer-message" style={{ marginBottom: 12, fontSize: 12 }}>{detail.message}</div>
            <div data-testid="drawer-files" style={{ fontSize: 11, color: '#888' }}>
              {detail.files.length} file{detail.files.length !== 1 ? 's' : ''} affected
            </div>
          </>
        ) : (
          <div style={{ color: '#555', fontSize: 11 }}>Loading…</div>
        )}
      </div>
    </div>
  );
}
