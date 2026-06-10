import { useEffect, useState } from 'react';
import { api } from './api';
import type { CommitDetail } from './api';

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  currentCommit: string | null;
  onNavigateToCommit: (commit: string) => void;
  onFileClick: (path: string, commit: string) => void;
}

interface FactVersion {
  commit: string;
  message: string;
  operation?: string;
}

// Two commit refs match if either is a prefix of the other, so a full
// 40-char hash from one endpoint selects an abbreviated hash from another.
function sameCommit(a: string | null, b: string | null): boolean {
  return !!a && !!b && (a.startsWith(b) || b.startsWith(a));
}

export function FactHistoryPanel({ repo, branch, factPath, currentCommit, onNavigateToCommit, onFileClick }: Props) {
  const [detail, setDetail] = useState<CommitDetail | null>(null);
  const [factVersions, setFactVersions] = useState<FactVersion[]>([]);

  // The commit whose detail we show below and highlight above. Prefer the
  // version matching the commit we were opened at; if that commit isn't one
  // of this fact's versions (e.g. the fact's anchor points at an unrelated
  // batch commit), fall back to the fact's most recent version so the detail
  // always corresponds to a selectable row in the list above.
  const activeCommit =
    factVersions.find(v => sameCommit(v.commit, currentCommit))?.commit
    ?? factVersions[0]?.commit
    ?? currentCommit;

  // Fetch commit detail for the active commit.
  useEffect(() => {
    if (!activeCommit) { setDetail(null); return; }
    let cancelled = false;
    api.commitDetail(repo, branch, activeCommit).then(d => {
      if (!cancelled) setDetail(d);
    }).catch(() => { if (!cancelled) setDetail(null); });
    return () => { cancelled = true; };
  }, [activeCommit, repo, branch]);

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
      {/* TOP: selectable version history */}
      {factVersions.length > 0 && (
        <div
          data-testid="history-fact-versions"
          style={{
            flexShrink: 0, maxHeight: '45%',
            display: 'flex', flexDirection: 'column',
            borderBottom: '1px solid #1a1a1a',
          }}
        >
          {/* Panel title */}
          <div style={{
            flexShrink: 0, display: 'flex', alignItems: 'baseline', gap: 8,
            padding: '11px 14px 10px', borderBottom: '1px solid #1a1a1a',
          }}>
            <span style={{ fontSize: 13, fontWeight: 600, color: '#eee' }}>History</span>
            <span style={{ flex: 1 }} />
            <span style={{ fontSize: 10, color: '#666' }}>
              {factVersions.length} version{factVersions.length !== 1 ? 's' : ''}
            </span>
          </div>

          <div style={{ overflowY: 'auto', padding: '6px 8px' }}>
          {factVersions.map(v => {
            const isCurrent = sameCommit(v.commit, activeCommit);
            return (
              <button
                key={v.commit}
                data-testid="history-fact-version"
                data-current={isCurrent ? 'true' : undefined}
                aria-current={isCurrent ? 'true' : undefined}
                onClick={() => onNavigateToCommit(v.commit)}
                style={{
                  width: '100%',
                  display: 'flex', alignItems: 'center', gap: 8, padding: '5px 8px',
                  fontSize: 11, textAlign: 'left',
                  background: isCurrent ? '#1c1c1c' : 'none',
                  border: 'none', outline: 'none',
                  borderLeft: `2px solid ${isCurrent ? '#e5a23c' : 'transparent'}`,
                  borderRadius: 3, cursor: 'pointer', color: 'inherit',
                }}
              >
                <span style={{
                  flexShrink: 0, width: 44, fontFamily: 'monospace', fontSize: 9,
                  textAlign: 'center', padding: '1px 0', borderRadius: 2,
                  background: v.operation ? '#1a1a2a' : 'transparent',
                  color: v.operation ? '#aaf' : 'transparent',
                }}>
                  {v.operation || '·'}
                </span>
                <span style={{ flexShrink: 0, fontFamily: 'monospace', color: isCurrent ? '#9d6' : '#7c9' }}>{v.commit.slice(0, 7)}</span>
                <span style={{ flex: 1, minWidth: 0, color: isCurrent ? '#ddd' : '#aaa', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v.message}</span>
              </button>
            );
          })}
          </div>
        </div>
      )}

      {/* BOTTOM: detail for the selected entry */}
      <div style={{ flex: 1, overflowY: 'auto' }}>
        {activeCommit ? (
          detail ? (
            <>
              {/* Panel title for the selected commit */}
              <div style={{
                display: 'flex', alignItems: 'baseline', gap: 8,
                padding: '11px 14px 10px', borderBottom: '1px solid #1a1a1a',
              }}>
                <span
                  data-testid="history-op-chip"
                  style={{ fontSize: 13, fontWeight: 600, color: '#eee', textTransform: 'capitalize' }}
                >{detail.operation || 'Commit'}</span>
                <span style={{ fontFamily: 'monospace', fontSize: 11, color: '#7c9' }}>{activeCommit.slice(0, 7)}</span>
                <span style={{ flex: 1 }} />
                <span style={{ fontSize: 10, color: '#666' }}>{detail.date}</span>
              </div>

              <div style={{ padding: '12px 14px' }}>
              <div
                data-testid="history-message"
                style={{
                  marginBottom: 14, padding: '6px 10px',
                  borderLeft: '2px solid #2a3a2a',
                  fontSize: 11, color: '#bbb', fontStyle: 'italic',
                  lineHeight: 1.45, wordBreak: 'break-word',
                }}
              >
                {detail.message}
              </div>

              <div style={{ fontSize: 10, color: '#666', letterSpacing: 0.5, textTransform: 'uppercase', marginBottom: 6 }}>
                Files affected
              </div>
              {detail.files.map(f => {
                const glyph = f.action === 'added' ? '+' : f.action === 'deleted' ? '−' : '~';
                const color = f.action === 'added' ? '#7c9' : f.action === 'deleted' ? '#f66' : '#aaf';
                const isDeleted = f.action === 'deleted';
                const isSelf = f.path === factPath;
                const isDisabled = isDeleted || isSelf;
                return (
                  <button
                    key={f.path}
                    data-testid="history-file-row"
                    data-self={isSelf ? 'true' : undefined}
                    disabled={isDisabled}
                    onClick={() => { if (activeCommit) onFileClick(f.path, activeCommit); }}
                    style={{
                      width: '100%', display: 'flex', alignItems: 'flex-start', gap: 8, padding: '5px 4px',
                      background: 'none', border: 'none', outline: 'none', borderRadius: 3, textAlign: 'left',
                      cursor: isDisabled ? 'default' : 'pointer', color: 'inherit',
                      opacity: isSelf ? 0.55 : 1,
                    }}
                  >
                    <span style={{ color, fontFamily: 'monospace', width: 12, fontSize: 12, flexShrink: 0, lineHeight: 1.3 }}>{glyph}</span>
                    <span style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 1 }}>
                      <span style={{ fontSize: 11, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        {f.title || f.path.split('/').pop()}
                      </span>
                      <span style={{ fontSize: 9, color: '#555', fontFamily: 'monospace', lineHeight: 1.3, wordBreak: 'break-all' }}>
                        {f.path}
                      </span>
                    </span>
                  </button>
                );
              })}
              </div>
            </>
          ) : (
            <div style={{ color: '#555', fontSize: 11, padding: '12px 14px' }}>Loading…</div>
          )
        ) : null}
      </div>
    </div>
  );
}
