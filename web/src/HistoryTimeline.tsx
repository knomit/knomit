import { useEffect, useRef, useState, useCallback } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { HistoryEntryWithTags, CommitDetail } from './api';
import type { AppState, Action } from './state';
import { currentPath } from './state';
import { relativeTime, opStyles, defaultOpStyle } from './utils';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

function commitStyle(entry: HistoryEntryWithTags): { color: string; bg: string; label: string } {
  if (entry.operation && opStyles[entry.operation]) return opStyles[entry.operation];
  return defaultOpStyle;
}

interface ExpandedState {
  detail: CommitDetail | null;
  loading: boolean;
}

export function HistoryTimeline({ state, dispatch }: Props) {
  const [entries, setEntries] = useState<HistoryEntryWithTags[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const [expanded, setExpanded] = useState<Record<string, ExpandedState>>({});
  const sentinelRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const containerRef = useRef<HTMLDivElement>(null);

  const path = currentPath(state);

  // Fetch first page on mount and when path changes
  useEffect(() => {
    setLoading(true);
    setEntries([]);
    setNextCursor(undefined);
    setSelectedIdx(0);
    setExpanded({});
    api.history(state.repo, path).then(r => {
      const e = r.entries || [];
      setEntries(e);
      setNextCursor(r.next);
      setLoading(false);
      if (e.length > 0) dispatch({ type: 'SELECT_COMMIT', commit: e[0].commit });
    }).catch(() => {
      setEntries([]);
      setLoading(false);
    });
  }, [path, state.repo]);

  // Filter visible children by active filter chips
  const filterChildren = (files: { path: string; action: string }[]) => {
    if (state.filters.length === 0) return files;
    return files.filter(f => {
      const typeChips = state.filters.filter(c => c.category === 'type');
      if (typeChips.length > 0) {
        // Can't determine type from commit detail, so show all
      }
      const pathChips = state.filters.filter(c => c.category === 'path');
      if (pathChips.length > 0) {
        if (!pathChips.some(c => f.path.startsWith(c.value))) return false;
      }
      return true;
    });
  };

  const toggleExpand = async (commit: string) => {
    const current = expanded[commit];
    if (current) {
      // Collapse
      setExpanded(prev => {
        const next = { ...prev };
        delete next[commit];
        return next;
      });
      return;
    }
    // Expand: fetch detail
    setExpanded(prev => ({ ...prev, [commit]: { detail: null, loading: true } }));
    try {
      const detail = await api.commitDetail(state.repo, commit);
      setExpanded(prev => ({ ...prev, [commit]: { detail, loading: false } }));
    } catch {
      setExpanded(prev => ({ ...prev, [commit]: { detail: null, loading: false } }));
    }
  };

  // Infinite scroll via IntersectionObserver
  const loadMore = useCallback(() => {
    if (loading || !nextCursor) return;
    setLoading(true);
    api.history(state.repo, path, nextCursor).then(r => {
      setEntries(prev => [...prev, ...(r.entries || [])]);
      setNextCursor(r.next);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [loading, nextCursor, path, state.repo]);

  useEffect(() => {
    const sentinel = sentinelRef.current;
    if (!sentinel) return;
    const observer = new IntersectionObserver(
      ([entry]) => { if (entry.isIntersecting) loadMore(); },
      { root: containerRef.current, threshold: 0.1 }
    );
    observer.observe(sentinel);
    return () => observer.disconnect();
  }, [loadMore]);

  // Keyboard navigation
  const navigate = useCallback((delta: 1 | -1) => {
    const next = Math.max(0, Math.min(selectedIdx + delta, entries.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const entry = entries[next];
    if (entry) dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
  }, [selectedIdx, entries, dispatch]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if (state.rightPanelFocused) return;
      if (state.view !== 'history') return;
      if (entries.length === 0) return;

      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); navigate(1); }
      if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); navigate(-1); }
      if (e.key === 'Enter') {
        e.preventDefault();
        const entry = entries[selectedIdx];
        if (entry) toggleExpand(entry.commit);
      }
      if (e.key === 'ArrowRight') {
        e.preventDefault();
        dispatch({ type: 'FOCUS_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, state.view, entries, selectedIdx, navigate, dispatch]);

  // Compute fact count badge
  const factCountBadge = (entry: HistoryEntryWithTags) => {
    const total = (entry.files?.added || 0) + (entry.files?.modified || 0) + (entry.files?.deleted || 0);
    return total > 0 ? total : null;
  };

  return (
    <div data-testid="history-timeline" ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ flex: 1, overflowY: 'auto', padding: '8px 0' }}>
        {entries.length === 0 && !loading && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>No history for this path.</div>
        )}
        {entries.map((entry, i) => {
          const isSelected = i === selectedIdx;
          const cs = commitStyle(entry);
          const hasLabel = cs.label !== '';
          const dotSize = hasLabel ? 10 : 6;
          const exp = expanded[entry.commit];
          const isExpanded = !!exp;
          const count = factCountBadge(entry);

          return (
            <div key={entry.commit}>
              <div
                data-testid="history-commit"
                data-hash={entry.commit}
                ref={el => { itemRefs.current[i] = el; }}
                style={{
                  display: 'flex',
                  alignItems: 'stretch',
                  paddingLeft: 12,
                  paddingRight: 12,
                  background: isSelected ? '#2a2a3a' : 'transparent',
                  cursor: 'pointer',
                }}
                onClick={() => {
                  setSelectedIdx(i);
                  dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
                }}
              >
                {/* Timeline column: continuous line + dot */}
                <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', width: 20, flexShrink: 0 }}>
                  <div style={{ width: 2, background: i === 0 ? 'transparent' : '#333', flex: 1 }} />
                  <div
                    onClick={e => { e.stopPropagation(); toggleExpand(entry.commit); }}
                    style={{
                      width: dotSize,
                      height: dotSize,
                      borderRadius: '50%',
                      background: cs.color,
                      flexShrink: 0,
                      margin: '2px 0',
                      cursor: 'pointer',
                    }}
                  />
                  <div style={{ width: 2, background: i === entries.length - 1 && !isExpanded ? 'transparent' : '#333', flex: 1 }} />
                </div>

                {/* Commit info */}
                <div style={{ flex: 1, minWidth: 0, paddingLeft: 8, paddingTop: 4, paddingBottom: 4 }}>
                  {hasLabel && (
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginBottom: 2 }}>
                      <span style={{
                        fontSize: 10, padding: '1px 6px', borderRadius: 8,
                        color: cs.color, background: cs.bg, whiteSpace: 'nowrap',
                      }}>{cs.label}</span>
                    </div>
                  )}
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ fontFamily: 'monospace', fontSize: 12, color: '#8af' }}>
                      {entry.commit.slice(0, 7)}
                    </span>
                    <span style={{ fontSize: 11, color: '#666' }}>{relativeTime(entry.date)}</span>
                    {count != null && (
                      <span style={{ fontSize: 9, color: '#aaa', background: '#2a2a2a', padding: '1px 5px', borderRadius: 8, fontFamily: 'monospace' }}>
                        {count} facts
                      </span>
                    )}
                  </div>
                  <div style={{ fontSize: 10, color: '#888', marginTop: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {entry.message}
                  </div>
                </div>
              </div>

              {/* Expanded: child files */}
              {isExpanded && (
                <div style={{ paddingLeft: 40, background: '#1a1a1e', borderBottom: '1px solid #222' }}>
                  {exp.loading && (
                    <div style={{ padding: '6px 12px', color: '#666', fontSize: 11 }}>Loading...</div>
                  )}
                  {exp.detail && filterChildren(exp.detail.files).map(file => {
                    const opIndicator = file.action === 'added' ? '+' : file.action === 'deleted' ? '\u2212' : '~';
                    const opColor = file.action === 'added' ? '#7c9' : file.action === 'deleted' ? '#f88' : '#8af';
                    const basename = file.path.split('/').pop()?.replace(/\.md$/, '') || file.path;
                    return (
                      <div
                        key={file.path}
                        onClick={() => {
                          dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
                          dispatch({ type: 'SELECT_FACT', path: file.path });
                        }}
                        style={{
                          padding: '4px 12px', cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 6,
                          fontSize: 11, color: '#ccc',
                        }}
                        onMouseEnter={e => { e.currentTarget.style.background = '#222'; }}
                        onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}
                      >
                        <span style={{ color: opColor, fontWeight: 'bold', fontFamily: 'monospace', width: 12, textAlign: 'center' }}>{opIndicator}</span>
                        <span>{basename}</span>
                      </div>
                    );
                  })}
                  {exp.detail && exp.detail.files.length === 0 && (
                    <div style={{ padding: '6px 12px', color: '#555', fontSize: 11 }}>No files in this commit.</div>
                  )}
                </div>
              )}
            </div>
          );
        })}
        {/* Sentinel for infinite scroll */}
        <div ref={sentinelRef} style={{ height: 1 }} />
        {loading && (
          <div style={{ padding: 12, color: '#666', fontSize: 12, textAlign: 'center' }}>Loading...</div>
        )}
      </div>
    </div>
  );
}
