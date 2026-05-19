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
  const [factVersions, setFactVersions] = useState<{ commit: string; message: string; operation?: string }[]>([]);

  // Close on Escape — only when open.
  useEffect(() => {
    if (!commitDrawer.open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') dispatch({ type: 'CLOSE_COMMIT_DRAWER' });
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [commitDrawer.open, dispatch]);

  const openCommit = commitDrawer.open ? commitDrawer.commit : null;

  // Fetch commit detail when the open commit changes.
  useEffect(() => {
    if (!openCommit) { setDetail(null); return; }
    let cancelled = false;
    api.commitDetail(state.repo, state.branch, openCommit).then(d => {
      if (!cancelled) setDetail(d);
    }).catch(() => { if (!cancelled) setDetail(null); });
    return () => { cancelled = true; };
  }, [openCommit, state.repo, state.branch]);

  // Fetch fact version history when a fact is open.
  useEffect(() => {
    if (!commitDrawer.open || !state.factPath) { setFactVersions([]); return; }
    let cancelled = false;
    api.factCommits(state.repo, state.branch, state.factPath).then(r => {
      if (!cancelled) {
        setFactVersions((r.entries || []).map(e => ({ commit: e.commit, message: e.message, operation: e.operation })));
      }
    }).catch(() => { if (!cancelled) setFactVersions([]); });
    return () => { cancelled = true; };
  }, [commitDrawer.open, state.factPath, state.repo, state.branch]);

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
            {/* Header line: op chip + relative date — commit hash is already in the top bar */}
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12 }}>
              {detail.operation && (
                <span
                  data-testid="drawer-op-chip"
                  style={{
                    fontFamily: 'monospace', fontSize: 10, padding: '1px 6px',
                    background: '#1a1a2a', color: '#aaf', borderRadius: 3,
                  }}
                >{detail.operation}</span>
              )}
              <span style={{ color: '#555', fontSize: 11 }}>{detail.date}</span>
            </div>

            {/* Message */}
            <div data-testid="drawer-message" style={{ marginBottom: 14, fontSize: 12, color: '#ddd' }}>
              {detail.message}
            </div>

            {/* Files affected */}
            <div style={{ fontSize: 10, color: '#666', letterSpacing: 0.5, textTransform: 'uppercase', marginBottom: 6 }}>
              Files affected
            </div>
            {detail.files.map(f => {
              const glyph = f.action === 'added' ? '+' : f.action === 'deleted' ? '−' : '~';
              const color = f.action === 'added' ? '#7c9' : f.action === 'deleted' ? '#f66' : '#aaf';
              return (
                <div
                  key={f.path}
                  data-testid="drawer-file-row"
                  style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '4px 0', fontSize: 11 }}
                >
                  <span style={{ color, fontFamily: 'monospace', width: 12 }}>{glyph}</span>
                  <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {f.title || f.path.split('/').pop()}
                  </span>
                  <span style={{ color: '#555', fontFamily: 'monospace', fontSize: 10 }}>{f.path}</span>
                </div>
              );
            })}

            {state.factPath && factVersions.length > 0 && (
              <div data-testid="drawer-fact-versions" style={{ marginTop: 16 }}>
                <div style={{ fontSize: 10, color: '#666', letterSpacing: 0.5, textTransform: 'uppercase', marginBottom: 6 }}>
                  This fact · {factVersions.length} version{factVersions.length !== 1 ? 's' : ''}
                </div>
                {factVersions.map(v => {
                  const isCurrent = commitDrawer.open && v.commit === commitDrawer.commit;
                  return (
                    <div
                      key={v.commit}
                      data-testid="drawer-fact-version"
                      style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '3px 0', fontSize: 11 }}
                    >
                      {isCurrent && <span data-testid="drawer-current-dot" style={{ width: 5, height: 5, borderRadius: '50%', background: '#e5a23c' }} />}
                      {!isCurrent && <span style={{ width: 5 }} />}
                      {v.operation && (
                        <span style={{ fontFamily: 'monospace', fontSize: 9, padding: '0 4px', background: '#1a1a2a', color: '#aaf', borderRadius: 2 }}>
                          {v.operation}
                        </span>
                      )}
                      <span style={{ fontFamily: 'monospace', color: '#7c9' }}>{v.commit.slice(0, 7)}</span>
                      <span style={{ flex: 1, color: '#aaa', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v.message}</span>
                    </div>
                  );
                })}
              </div>
            )}
          </>
        ) : (
          <div style={{ color: '#555', fontSize: 11 }}>Loading…</div>
        )}
      </div>
    </div>
  );
}
