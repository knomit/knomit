import { useEffect, useState } from 'react';
import { api } from './api';
import type { CommitDetail } from './api';

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  currentCommit: string | null;
  onNavigateToCommit: (commit: string) => void;
}

interface FactVersion {
  commit: string;
  message: string;
  operation?: string;
}

export function FactHistoryPanel({ repo, branch, factPath, currentCommit, onNavigateToCommit }: Props) {
  const [detail, setDetail] = useState<CommitDetail | null>(null);
  const [factVersions, setFactVersions] = useState<FactVersion[]>([]);

  // Fetch commit detail for the currently-displayed commit.
  useEffect(() => {
    if (!currentCommit) { setDetail(null); return; }
    let cancelled = false;
    api.commitDetail(repo, branch, currentCommit).then(d => {
      if (!cancelled) setDetail(d);
    }).catch(() => { if (!cancelled) setDetail(null); });
    return () => { cancelled = true; };
  }, [currentCommit, repo, branch]);

  // Fetch per-fact version list.
  useEffect(() => {
    let cancelled = false;
    api.factCommits(repo, branch, factPath).then(r => {
      if (!cancelled) {
        setFactVersions((r.entries || []).map(e => ({
          commit: e.commit, message: e.message, operation: e.operation,
        })));
      }
    }).catch(() => { if (!cancelled) setFactVersions([]); });
    return () => { cancelled = true; };
  }, [factPath, repo, branch]);

  return (
    <div
      data-testid="fact-history-panel"
      style={{
        background: '#0f0f0f', borderLeft: '1px solid #1a1a1a',
        display: 'flex', flexDirection: 'column', overflow: 'hidden',
        fontFamily: 'system-ui, sans-serif', color: '#ddd',
        width: '100%', height: '100%',
      }}
    >
      <div style={{ padding: '12px 14px', borderBottom: '1px solid #1a1a1a', display: 'flex', alignItems: 'center', gap: 8 }}>
        <span
          data-testid="history-panel-commit"
          style={{
            color: '#7c9', background: '#1a2e1a', padding: '1px 6px', borderRadius: 3,
            fontFamily: 'monospace', fontSize: 11,
          }}
        >
          {currentCommit ? currentCommit.slice(0, 7) : 'HEAD'}
        </span>
      </div>

      <div style={{ flex: 1, overflowY: 'auto', padding: '12px 14px' }}>
        {currentCommit ? (
          detail ? (
            <>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12 }}>
                {detail.operation && (
                  <span
                    data-testid="history-op-chip"
                    style={{
                      fontFamily: 'monospace', fontSize: 10, padding: '1px 6px',
                      background: '#1a1a2a', color: '#aaf', borderRadius: 3,
                    }}
                  >{detail.operation}</span>
                )}
                <span style={{ color: '#555', fontSize: 11 }}>{detail.date}</span>
              </div>

              <div data-testid="history-message" style={{ marginBottom: 14, fontSize: 12, color: '#ddd' }}>
                {detail.message}
              </div>

              <div style={{ fontSize: 10, color: '#666', letterSpacing: 0.5, textTransform: 'uppercase', marginBottom: 6 }}>
                Files affected
              </div>
              {detail.files.map(f => {
                const glyph = f.action === 'added' ? '+' : f.action === 'deleted' ? '−' : '~';
                const color = f.action === 'added' ? '#7c9' : f.action === 'deleted' ? '#f66' : '#aaf';
                return (
                  <div
                    key={f.path}
                    data-testid="history-file-row"
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
            </>
          ) : (
            <div style={{ color: '#555', fontSize: 11 }}>Loading…</div>
          )
        ) : null}

        {factVersions.length > 0 && (
          <div data-testid="history-fact-versions" style={{ marginTop: currentCommit ? 16 : 0 }}>
            <div style={{ fontSize: 10, color: '#666', letterSpacing: 0.5, textTransform: 'uppercase', marginBottom: 6 }}>
              This fact · {factVersions.length} version{factVersions.length !== 1 ? 's' : ''}
            </div>
            {factVersions.map(v => {
              const isCurrent = v.commit === currentCommit;
              return (
                <button
                  key={v.commit}
                  data-testid="history-fact-version"
                  onClick={() => onNavigateToCommit(v.commit)}
                  style={{
                    width: '100%',
                    display: 'flex', alignItems: 'center', gap: 8, padding: '3px 4px',
                    fontSize: 11, textAlign: 'left',
                    background: 'none', border: 'none', outline: 'none',
                    borderRadius: 3, cursor: 'pointer', color: 'inherit',
                  }}
                >
                  {isCurrent
                    ? <span data-testid="history-current-dot" style={{ width: 5, height: 5, borderRadius: '50%', background: '#e5a23c' }} />
                    : <span style={{ width: 5 }} />}
                  {v.operation && (
                    <span style={{ fontFamily: 'monospace', fontSize: 9, padding: '0 4px', background: '#1a1a2a', color: '#aaf', borderRadius: 2 }}>
                      {v.operation}
                    </span>
                  )}
                  <span style={{ fontFamily: 'monospace', color: '#7c9' }}>{v.commit.slice(0, 7)}</span>
                  <span style={{ flex: 1, color: '#aaa', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v.message}</span>
                </button>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
