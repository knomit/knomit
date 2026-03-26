import { useEffect, useRef, useState, useCallback, useMemo } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { HistoryEntryWithTags } from './api';
import type { AppState, Action } from './state';
import { currentPath } from './state';
import { relativeTime, opStyles, defaultOpStyle } from './utils';
import type { NavRequest } from './useNavigationManager';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
}

function commitStyle(entry: HistoryEntryWithTags): { color: string; bg: string; label: string } {
  if (entry.operation && opStyles[entry.operation]) return opStyles[entry.operation];
  return defaultOpStyle;
}

export function HistoryTimeline({ state, dispatch, navigate }: Props) {
  const [entries, setEntries] = useState<HistoryEntryWithTags[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const sentinelRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const containerRef = useRef<HTMLDivElement>(null);

  const path = currentPath(state);

  const staleStateRef = useRef(state);
  staleStateRef.current = state;

  useEffect(() => {
    let stale = false;
    setLoading(true);
    setEntries([]);
    setNextCursor(undefined);
    setSelectedIdx(0);
    api.history(state.repo, path).then(r => {
      if (stale) return;
      const e = r.entries || [];
      setEntries(e);
      setNextCursor(r.next);
      setLoading(false);
      // If historyCommit is already set (e.g. NAV_BACK restored it), just sync visual index.
      if (staleStateRef.current.historyCommit) {
        const idx = e.findIndex(c => c.commit === staleStateRef.current.historyCommit);
        if (idx >= 0) setSelectedIdx(idx);
        return;
      }
      // No explicit selection — amend current nav entry in-place (no navStack push).
      if (e.length > 0) {
        dispatch({ type: 'AMEND_NAV', historyCommit: e[0].commit, factPath: null, factCommit: e[0].commit });
      }
    }).catch(() => {
      if (!stale) { setEntries([]); setLoading(false); }
    });
    return () => { stale = true; };
  }, [path, state.repo]);

  const loadingRef = useRef(loading);
  loadingRef.current = loading;

  const loadMore = useCallback(() => {
    if (loadingRef.current || !nextCursor) return;
    setLoading(true);
    api.history(state.repo, path, nextCursor).then(r => {
      setEntries(prev => [...prev, ...(r.entries || [])]);
      setNextCursor(r.next);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [nextCursor, path, state.repo]);

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

  const epFilters = useMemo(
    () => state.filters.filter(f => f.category === 'ep').map(f => f.value),
    [state.filters],
  );
  const freeText = state.freeText.toLowerCase();

  const filteredEntries = useMemo(() =>
    entries.filter(entry => {
      if (epFilters.length > 0 && (!entry.operation || !epFilters.includes(entry.operation))) return false;
      if (freeText && !entry.message.toLowerCase().includes(freeText)) return false;
      return true;
    }),
    [entries, epFilters, freeText],
  );

  useEffect(() => {
    if (!state.historyCommit) return;
    const idx = filteredEntries.findIndex(e => e.commit === state.historyCommit);
    if (idx >= 0 && idx !== selectedIdx) {
      setSelectedIdx(idx);
      itemRefs.current[idx]?.scrollIntoView({ block: 'nearest' });
    }
  }, [state.historyCommit, filteredEntries]);

  const moveSelection = useCallback((delta: 1 | -1) => {
    const next = Math.max(0, Math.min(selectedIdx + delta, filteredEntries.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const entry = filteredEntries[next];
    if (entry) navigate({ view: 'history', historyCommit: entry.commit, factPath: null });
  }, [selectedIdx, filteredEntries, navigate]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if (state.rightPanelFocused) return;
      if (state.view !== 'history') return;
      if (filteredEntries.length === 0) return;

      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); moveSelection(1); }
      if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); moveSelection(-1); }
      if (e.key === 'ArrowRight') {
        e.preventDefault();
        dispatch({ type: 'FOCUS_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, state.view, filteredEntries, selectedIdx, moveSelection, dispatch]);

  const factCountBadge = (entry: HistoryEntryWithTags) => {
    const total = (entry.files?.added || 0) + (entry.files?.modified || 0) + (entry.files?.deleted || 0);
    return total > 0 ? total : null;
  };

  return (
    <div data-testid="history-timeline" ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ flex: 1, overflowY: 'auto', padding: '8px 0' }}>
        {filteredEntries.length === 0 && !loading && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>
            {entries.length === 0 ? 'No history for this path.' : 'No commits match the current filters.'}
          </div>
        )}
        {filteredEntries.map((entry, i) => {
          const isSelected = entry.commit === state.historyCommit;
          const cs = commitStyle(entry);
          const hasLabel = cs.label !== '';
          const dotSize = hasLabel ? 10 : 6;
          const count = factCountBadge(entry);

          return (
            <div
              key={entry.commit}
              data-testid="history-commit"
              data-hash={entry.commit}
              ref={el => { itemRefs.current[i] = el; }}
              style={{
                display: 'flex',
                alignItems: 'stretch',
                paddingLeft: 12,
                paddingRight: 12,
                background: isSelected ? '#2a2a3a' : 'transparent',
                borderLeft: isSelected ? `3px solid ${cs.color}` : '3px solid transparent',
                cursor: 'pointer',
              }}
              onClick={() => {
                setSelectedIdx(i);
                navigate({ view: 'history', historyCommit: entry.commit, factPath: null });
              }}
            >
              {/* Timeline column: continuous line + dot */}
              <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', width: 20, flexShrink: 0 }}>
                <div style={{ width: 2, background: i === 0 ? 'transparent' : '#333', flex: 1 }} />
                <div
                  style={{
                    width: dotSize,
                    height: dotSize,
                    borderRadius: '50%',
                    background: cs.color,
                    flexShrink: 0,
                    margin: '2px 0',
                  }}
                />
                <div style={{ width: 2, background: i === entries.length - 1 ? 'transparent' : '#333', flex: 1 }} />
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
                      {count} {count === 1 ? 'fact' : 'facts'}
                    </span>
                  )}
                </div>
                <div style={{ fontSize: 10, color: '#888', marginTop: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {entry.message}
                </div>
              </div>
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
