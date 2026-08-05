import { useEffect, useState } from 'react';
import { api } from './api';
import type { CommitAuthor, CommitDetail } from './api';
import { BotIcon, UserIcon, BroadcastIcon, ChevronLeftIcon, ChevronDownIcon } from './icons';

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
  onScrub: (commit: string) => void;
  onOpenFileAt: (path: string, commit: string) => void;
  onReturnToLive?: () => void;
  /**
   * Back out of this history excursion, to wherever you came from.
   *
   * The timeline REPLACES the library in this column, so without a back control
   * here there is none at all while time-travelling — and every edge hop now
   * lands in history, because a reference resolves at the commit it was added
   * at. Arriving somewhere with no way back was the practical cost of that.
   */
  canBack?: boolean;
  onBack?: () => void;
}

const AMBER = '#e5a23c';

export function TimelineNav({ repo, branch, factPath, activeCommit, onScrub, onOpenFileAt, onReturnToLive, canBack = false, onBack }: Props) {
  const [entries, setEntries] = useState<FactEntry[]>([]);
  const [detail, setDetail] = useState<CommitDetail | null>(null);
  // THE DETAIL CARD STARTS CLOSED. This column is a list of versions first, and
  // a merge commit's files-affected list runs to a hundred rows — opening it by
  // default buried the very list the reader came here to scan, so the other
  // versions sat below a card nobody asked for. Clicking the active row opens
  // it; changing version closes it again, so scrubbing down the timeline never
  // re-buries the list.
  const [detailOpen, setDetailOpen] = useState(false);
  useEffect(() => { setDetailOpen(false); }, [activeCommit]);

  // Click a row: toggle the detail when it's already the active version, else
  // select that version (scrub). Selecting the newest version is still a
  // history scrub — it does not exit to live.
  const handleRowClick = (commit: string) => {
    if (sameCommit(commit, activeCommit)) {
      setDetailOpen(o => !o);
    } else {
      setDetailOpen(false);
      onScrub(commit);
    }
  };

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

  // Determine if this fact is retracted: the newest entry (entries are
  // newest-first) carries a 'retract' operation. This is the only signal for
  // "no HEAD version" — scrubbing to an older commit must not trigger it.
  const newestCommit = entries[0]?.commit ?? null;
  const isRetracted = entries.length > 0 && entries[0].operation === 'retract';

  return (
    <div
      data-testid="timeline-nav"
      style={{
        background: '#0f0f0f',
        display: 'flex', flexDirection: 'column', overflow: 'hidden',
        fontFamily: 'var(--k-font-body)', color: '#ddd',
        width: '100%', height: '100%',
      }}
    >
      {/* Header */}
      <div style={{
        flexShrink: 0, borderBottom: '1px solid #1a1a1a',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '9px 12px' }}>
          {/* Back leads the header, exactly as it does in the live library
              header — same action (NAV_BACK), same position, so the control
              does not move when the column swaps between modes. Amber here
              because everything in this rail is. */}
          <button
            data-testid="timeline-back"
            onClick={() => { if (canBack) onBack?.(); }}
            disabled={!canBack}
            aria-label="Back"
            title="Back (⌘[ or Backspace)"
            style={{
              background: 'none', border: 'none', outline: 'none', borderRadius: 0,
              width: 16, height: 20, padding: 0, flexShrink: 0,
              display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
              cursor: canBack ? 'pointer' : 'default',
            }}
          >
            <ChevronLeftIcon color={canBack ? AMBER : '#4a3a20'} size={13} />
          </button>
          {/* Title mirrors the live rail's "LIBRARY · N facts": accent title +
              muted count. */}
          <span style={{ fontSize: 10, fontFamily: 'var(--k-font-mono)', letterSpacing: 1, textTransform: 'uppercase' }}>
            <span style={{ color: AMBER }}>Timeline</span>
            <span style={{ color: '#666' }}> · {entries.length} {entries.length === 1 ? 'version' : 'versions'}</span>
          </span>
          <span style={{ flex: 1 }} />
          {/* Exit the history excursion → return to live (also bound to 'h').
              Icon-only broadcast glyph, borderless, in the amber history color
              so it reads as an action — distinct from the passive status dot in
              the footer. Hover lifts a faint amber wash; label lives in the
              tooltip + accessible name. */}
          <button
            data-testid="timeline-return-live"
            onClick={onReturnToLive}
            title="Exit history — return to live (h)"
            aria-label="Return to live"
            style={{
              display: 'inline-flex', alignItems: 'center',
              background: 'none', border: 'none', borderRadius: 4,
              cursor: 'pointer', padding: '3px 6px',
              color: AMBER,
            }}
            onMouseEnter={e => { (e.currentTarget as HTMLElement).style.background = 'rgba(229,162,60,0.12)'; }}
            onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = 'none'; }}
          >
            <BroadcastIcon color={AMBER} size={13} />
          </button>
        </div>
        {/* Retracted note — shown only when the fact is genuinely retracted */}
        {isRetracted && (
          <div style={{
            margin: '0 12px 8px', padding: '6px 8px', borderRadius: 4,
            background: 'rgba(255,80,80,0.07)', border: '1px solid rgba(255,80,80,0.25)',
            fontSize: 9.5, color: '#f88', fontFamily: 'var(--k-font-mono)', lineHeight: 1.5,
          }}>
            retracted at {newestCommit?.slice(0, 7)} · no HEAD version
          </div>
        )}
      </div>

      <div style={{ flex: 1, overflowY: 'auto', padding: '4px 0' }}>
        {entries.map((entry, index) => {
          const isNewest = index === 0;
          const isActive = sameCommit(entry.commit, activeCommit);

          // Badge logic: newest row always gets HEAD (live) or LAST (retracted).
          // Active row always gets AT. These are independent and can both appear.
          const newestBadge: { label: string; color: string; borderColor: string } | null =
            isNewest
              ? isRetracted
                ? { label: 'LAST', color: '#f88', borderColor: '#f88555' }
                : { label: 'HEAD', color: '#6a9', borderColor: '#6a9555' }
              : null;
          const atBadge = isActive
            ? { label: 'AT', color: AMBER, borderColor: AMBER + '55' }
            : null;

          return (
            <div key={entry.commit}>
              <button
                data-testid="timeline-row"
                onClick={() => handleRowClick(entry.commit)}
                title={isActive ? (detailOpen ? 'Collapse commit details' : 'Expand commit details') : 'View this version'}
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
                    flexShrink: 0, fontFamily: 'var(--k-font-mono)', fontSize: 9, padding: '1px 6px',
                    borderRadius: 8, background: entry.operation ? '#1a1a2a' : 'transparent',
                    color: entry.operation ? '#aaf' : 'transparent',
                    letterSpacing: 0.4,
                  }}>
                    {entry.operation || '·'}
                  </span>
                  {/* 7-char hash */}
                  <span style={{ flexShrink: 0, fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#8af' }}>
                    {entry.commit.slice(0, 7)}
                  </span>
                  {/* Badges: newest (HEAD/LAST) and active (AT) are independent */}
                  {newestBadge && (
                    <span style={{
                      fontSize: 8, fontFamily: 'var(--k-font-mono)', letterSpacing: 0.6,
                      padding: '0 4px', border: `1px solid ${newestBadge.borderColor}`,
                      borderRadius: 2, color: newestBadge.color, flexShrink: 0,
                    }}>
                      {newestBadge.label}
                    </span>
                  )}
                  {atBadge && (
                    <span style={{
                      fontSize: 8, fontFamily: 'var(--k-font-mono)', letterSpacing: 0.6,
                      padding: '0 4px', border: `1px solid ${atBadge.borderColor}`,
                      borderRadius: 2, color: atBadge.color, flexShrink: 0,
                    }}>
                      {atBadge.label}
                    </span>
                  )}
                  {/* Relative time */}
                  {entry.date && (
                    <span style={{ marginLeft: 'auto', fontSize: 9.5, color: '#555', flexShrink: 0 }}>
                      {entry.date}
                    </span>
                  )}
                  {/* Disclosure caret — only on the active row, the only one
                      with a detail to disclose. A 9px text triangle (▸/▾) was
                      the smallest mark on the row and read as punctuation; this
                      is the stroked chevron used everywhere else in the app, at
                      a legible size, turned a quarter turn when closed so open
                      and closed are the same shape pointing two ways rather
                      than two glyphs to tell apart. */}
                  {isActive && (
                    <span
                      aria-hidden="true"
                      data-testid="timeline-detail-caret"
                      data-open={detailOpen ? 'true' : 'false'}
                      style={{
                        marginLeft: entry.date ? 6 : 'auto', flexShrink: 0,
                        display: 'inline-flex', alignItems: 'center',
                        transform: detailOpen ? 'none' : 'rotate(-90deg)',
                      }}
                    >
                      <ChevronDownIcon color={AMBER} size={13} />
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

              {/* Inline commit detail card for the active row (collapsed until asked for) */}
              {isActive && detail && detailOpen && (
                <div style={{
                  margin: '0 12px 10px 30px', padding: '10px 12px',
                  background: '#111', border: '1px solid #1a1a1a', borderRadius: 6,
                }}>
                  <CommitAuthorLine author={detail.author} />
                  {detail.message && (
                    <div style={{
                      fontSize: 11.5, color: '#ddd', lineHeight: 1.5, marginBottom: 10,
                      fontStyle: 'italic',
                    }}>
                      {detail.message}
                    </div>
                  )}
                  <div style={{ fontSize: 9, color: '#555', fontFamily: 'var(--k-font-mono)', letterSpacing: 1, textTransform: 'uppercase', marginBottom: 6 }}>
                    Files affected · {detail.files.length}
                  </div>
                  {detail.files.map(f => {
                    const glyph = f.action === 'added' ? '+' : f.action === 'deleted' ? '−' : '~';
                    const color = f.action === 'added' ? '#7c9' : f.action === 'deleted' ? '#f66' : '#aaf';
                    const isDeleted = f.action === 'deleted';
                    const isSelf = f.path === factPath;
                    const isDisabled = isDeleted || isSelf;
                    // The current fact is "you are here" — mark it with an amber
                    // arrow + band matching the active timeline row, not dimmed.
                    return (
                      <button
                        key={f.path}
                        data-testid="timeline-file-row"
                        data-self={isSelf ? 'true' : undefined}
                        disabled={isDisabled}
                        onClick={() => { if (!isDisabled) onOpenFileAt(f.path, activeCommit); }}
                        style={{
                          width: '100%', display: 'flex', alignItems: 'flex-start', gap: 8, padding: '5px 4px',
                          background: isSelf ? 'rgba(229,162,60,0.10)' : 'none',
                          border: 'none', borderLeft: `2px solid ${isSelf ? AMBER : 'transparent'}`,
                          outline: 'none', borderRadius: 3, textAlign: 'left',
                          cursor: isDisabled ? 'default' : 'pointer', color: 'inherit',
                        }}
                      >
                        <span style={{ color: isSelf ? AMBER : color, fontFamily: 'var(--k-font-mono)', width: 12, fontSize: 12, flexShrink: 0, lineHeight: 1.3 }}>
                          {isSelf ? '▸' : glyph}
                        </span>
                        <span style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 1 }}>
                          <span style={{
                            fontSize: 11, fontWeight: isSelf ? 600 : 400,
                            color: isSelf ? AMBER : isDeleted ? '#666' : '#ddd',
                            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                            textDecoration: isDeleted ? 'line-through' : 'none',
                          }}>
                            {f.title || f.path.split('/').pop()}
                          </span>
                          <span style={{ fontSize: 9, color: '#555', fontFamily: 'var(--k-font-mono)', lineHeight: 1.3, wordBreak: 'break-all' }}>
                            {f.path}
                          </span>
                        </span>
                        {isSelf && (
                          <span data-testid="timeline-here-marker" style={{ fontSize: 8, color: AMBER, fontFamily: 'var(--k-font-mono)', flexShrink: 0, letterSpacing: 0.6, textTransform: 'uppercase' }}>here</span>
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
