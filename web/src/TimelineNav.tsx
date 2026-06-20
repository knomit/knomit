import { useEffect, useState } from 'react';
import { api } from './api';
import type { CommitAuthor, CommitDetail } from './api';
import { BotIcon, UserIcon } from './icons';

// Agent commits are authored under the agents.knomit.io domain; everyone else
// (humans, PR merges) is shown as a person.
const AGENT_EMAIL_DOMAIN = '@agents.knomit.io';

// CommitAuthorLine renders the commit author as an icon + identity: a bot for
// agent authors, a person for humans. It shows the username, falling back to
// the email when no name was recorded. Renders nothing when neither is present.
function CommitAuthorLine({ author }: { author?: CommitAuthor }) {
  if (!author || (!author.name && !author.email)) return null;
  const isAgent = author.email.endsWith(AGENT_EMAIL_DOMAIN);
  const label = author.name || author.email;
  return (
    <div
      data-testid="timeline-author"
      data-kind={isAgent ? 'agent' : 'human'}
      style={{
        marginBottom: 8, display: 'flex', alignItems: 'center', gap: 6,
        fontSize: 11, color: '#888', lineHeight: 1.45, minWidth: 0,
      }}
    >
      {isAgent ? <BotIcon color="#888" size={13} /> : <UserIcon color="#888" size={13} />}
      <span
        title={author.email || undefined}
        style={{ color: '#bbb', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
      >
        {label}
      </span>
    </div>
  );
}

// Two commit refs match if either is a prefix of the other, so a full
// 40-char hash from one endpoint selects an abbreviated hash from another.
function sameCommit(a: string | null, b: string | null): boolean {
  return !!a && !!b && (a.startsWith(b) || b.startsWith(a));
}

interface FactEntry {
  commit: string;
  message: string;
  operation?: string;
  date: string;
}

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  activeCommit: string;
  onScrub: (commit: string, isLatest: boolean) => void;
  onOpenFileAt: (path: string, commit: string) => void;
}

const AMBER = '#e5a23c';

export function TimelineNav({ repo, branch, factPath, activeCommit, onScrub, onOpenFileAt }: Props) {
  const [entries, setEntries] = useState<FactEntry[]>([]);
  const [detail, setDetail] = useState<CommitDetail | null>(null);

  // Fetch per-fact version list (newest first from api).
  useEffect(() => {
    let cancelled = false;
    api.factCommits(repo, branch, factPath).then(r => {
      if (!cancelled) {
        setEntries((r.entries || []).map(e => ({
          commit: e.commit, message: e.message, operation: e.operation, date: e.date,
        })));
      }
    }).catch(() => { if (!cancelled) setEntries([]); });
    return () => { cancelled = true; };
  }, [factPath, repo, branch]);

  // Fetch commit detail for the active commit.
  useEffect(() => {
    if (!activeCommit) { setDetail(null); return; }
    let cancelled = false;
    api.commitDetail(repo, branch, activeCommit).then(d => {
      if (!cancelled) setDetail(d);
    }).catch(() => { if (!cancelled) setDetail(null); });
    return () => { cancelled = true; };
  }, [activeCommit, repo, branch]);

  // Determine if this fact is live at HEAD: the newest entry matches activeCommit.
  const newestCommit = entries[0]?.commit ?? null;
  const isLiveAtHead = sameCommit(newestCommit, activeCommit);

  return (
    <div
      data-testid="timeline-nav"
      style={{
        background: '#0f0f0f',
        display: 'flex', flexDirection: 'column', overflow: 'hidden',
        fontFamily: 'system-ui, sans-serif', color: '#ddd',
        width: '100%', height: '100%',
      }}
    >
      {/* Header */}
      <div style={{
        flexShrink: 0, display: 'flex', alignItems: 'baseline', gap: 8,
        padding: '9px 12px', borderBottom: '1px solid #1a1a1a',
      }}>
        <span style={{ fontSize: 10, color: AMBER, fontFamily: 'monospace', letterSpacing: 1, textTransform: 'uppercase' }}>
          Timeline · {entries.length} {entries.length === 1 ? 'version' : 'versions'}
        </span>
        <span style={{ flex: 1 }} />
        <span style={{ fontSize: 9, color: '#555', fontFamily: 'monospace' }}>click to scrub</span>
      </div>

      <div style={{ flex: 1, overflowY: 'auto', padding: '4px 0' }}>
        {entries.map((entry, index) => {
          const isNewest = index === 0;
          const isActive = sameCommit(entry.commit, activeCommit);

          // Badge logic: newest row = HEAD (live) or LAST (retracted); active row = AT
          let badge: { label: string; color: string; borderColor: string } | null = null;
          if (isNewest && isLiveAtHead) {
            badge = { label: 'HEAD', color: '#6a9', borderColor: '#6a955' };
          } else if (isNewest && !isLiveAtHead) {
            badge = { label: 'LAST', color: '#f88', borderColor: '#f8855' };
          }
          // AT badge shows for active (even if also newest)
          if (isActive) {
            badge = { label: 'AT', color: AMBER, borderColor: AMBER + '55' };
          }

          return (
            <div key={entry.commit}>
              <button
                data-testid="timeline-row"
                onClick={() => onScrub(entry.commit, isNewest)}
                style={{
                  width: '100%', display: 'block', textAlign: 'left',
                  padding: '8px 12px',
                  background: isActive ? 'rgba(229,162,60,0.10)' : 'none',
                  border: 'none', outline: 'none',
                  borderLeft: `2px solid ${isActive ? AMBER : 'transparent'}`,
                  cursor: 'pointer', color: 'inherit',
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 3 }}>
                  {/* Op chip */}
                  <span style={{
                    flexShrink: 0, fontFamily: 'monospace', fontSize: 9, padding: '1px 6px',
                    borderRadius: 8, background: entry.operation ? '#1a1a2a' : 'transparent',
                    color: entry.operation ? '#aaf' : 'transparent',
                    letterSpacing: 0.4,
                  }}>
                    {entry.operation || '·'}
                  </span>
                  {/* 7-char hash */}
                  <span style={{ flexShrink: 0, fontFamily: 'monospace', fontSize: 11, color: '#8af' }}>
                    {entry.commit.slice(0, 7)}
                  </span>
                  {/* Badge */}
                  {badge && (
                    <span style={{
                      fontSize: 8, fontFamily: 'monospace', letterSpacing: 0.6,
                      padding: '0 4px', border: `1px solid ${badge.borderColor}`,
                      borderRadius: 2, color: badge.color, flexShrink: 0,
                    }}>
                      {badge.label}
                    </span>
                  )}
                  {/* Relative time */}
                  {entry.date && (
                    <span style={{ marginLeft: 'auto', fontSize: 9.5, color: '#555', flexShrink: 0 }}>
                      {entry.date}
                    </span>
                  )}
                </div>
                {/* 2-line message */}
                <div style={{
                  fontSize: 11, color: isActive ? '#ddd' : '#999', lineHeight: 1.4,
                  display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical', overflow: 'hidden',
                }}>
                  {entry.message}
                </div>
              </button>

              {/* Inline commit detail card for the active row */}
              {isActive && detail && (
                <div style={{
                  margin: '0 12px 10px 30px', padding: '10px 12px',
                  background: '#111', border: '1px solid #1a1a1a', borderRadius: 6,
                }}>
                  <CommitAuthorLine author={detail.author} />
                  <div style={{ fontSize: 9, color: '#555', fontFamily: 'monospace', letterSpacing: 1, textTransform: 'uppercase', marginBottom: 6 }}>
                    Files affected · {detail.files.length}
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
                        data-testid="timeline-file-row"
                        data-self={isSelf ? 'true' : undefined}
                        disabled={isDisabled}
                        onClick={() => { if (!isDisabled) onOpenFileAt(f.path, activeCommit); }}
                        style={{
                          width: '100%', display: 'flex', alignItems: 'flex-start', gap: 8, padding: '5px 4px',
                          background: 'none', border: 'none', outline: 'none', borderRadius: 3, textAlign: 'left',
                          cursor: isDisabled ? 'default' : 'pointer', color: 'inherit',
                          opacity: isSelf ? 0.55 : 1,
                        }}
                      >
                        <span style={{ color, fontFamily: 'monospace', width: 12, fontSize: 12, flexShrink: 0, lineHeight: 1.3 }}>{glyph}</span>
                        <span style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 1 }}>
                          <span style={{
                            fontSize: 11, color: isDeleted ? '#666' : '#ddd',
                            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                            textDecoration: isDeleted ? 'line-through' : 'none',
                          }}>
                            {f.title || f.path.split('/').pop()}
                          </span>
                          <span style={{ fontSize: 9, color: '#555', fontFamily: 'monospace', lineHeight: 1.3, wordBreak: 'break-all' }}>
                            {f.path}
                          </span>
                        </span>
                        {isSelf && (
                          <span style={{ fontSize: 8, color: AMBER, fontFamily: 'monospace', flexShrink: 0 }}>this</span>
                        )}
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
