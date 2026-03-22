import { useEffect, useRef, useState, useCallback } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { HistoryEntryWithTags } from './api';
import type { AppState, Action } from './state';
import { relativeTime, opStyles, defaultOpStyle } from './utils';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

function commitStyle(entry: HistoryEntryWithTags): { color: string; bg: string; label: string } {
  if (entry.operation && opStyles[entry.operation]) return opStyles[entry.operation];
  return defaultOpStyle;
}

export function HistoryTimeline({ state, dispatch }: Props) {
  const [entries, setEntries] = useState<HistoryEntryWithTags[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const [activeOps, setActiveOps] = useState<Set<string>>(new Set());
  const sentinelRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const containerRef = useRef<HTMLDivElement>(null);

  // Fetch first page on mount and when currentPath changes
  useEffect(() => {
    setLoading(true);
    setEntries([]);
    setNextCursor(undefined);
    setSelectedIdx(0);
    setActiveOps(new Set());
    api.history(state.repo, state.currentPath).then(r => {
      const e = r.entries || [];
      setEntries(e);
      setNextCursor(r.next);
      setLoading(false);
      if (e.length > 0) dispatch({ type: 'SELECT_COMMIT', commit: e[0].commit });
    }).catch(() => {
      setEntries([]);
      setLoading(false);
    });
  }, [state.currentPath]);

  // Collect distinct operations from loaded entries; add "other" if any have no operation
  const availableOps = Array.from(new Set(entries.map(e => e.operation || '').filter(Boolean)));
  const hasOther = entries.some(e => !e.operation);
  if (hasOther) availableOps.push('other');

  // Filter entries by active operations (empty set = show all)
  const filtered = activeOps.size === 0 ? entries : entries.filter(e => {
    if (!e.operation) return activeOps.has('other');
    return activeOps.has(e.operation);
  });

  const toggleOp = (op: string) => {
    setActiveOps(prev => {
      const next = new Set(prev);
      if (next.has(op)) next.delete(op); else next.add(op);
      return next;
    });
    setSelectedIdx(0);
  };

  // Infinite scroll via IntersectionObserver
  const loadMore = useCallback(() => {
    if (loading || !nextCursor) return;
    setLoading(true);
    api.history(state.repo, state.currentPath, nextCursor).then(r => {
      setEntries(prev => [...prev, ...(r.entries || [])]);
      setNextCursor(r.next);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [loading, nextCursor, state.currentPath]);

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

  // Sync selection + scroll when historyCommit is set externally (e.g. from_commit badge click)
  useEffect(() => {
    if (!state.historyCommit) return;
    const idx = filtered.findIndex(e => e.commit === state.historyCommit);
    if (idx === -1) return; // not yet loaded or filtered out
    setSelectedIdx(idx);
    itemRefs.current[idx]?.scrollIntoView({ block: 'nearest' });
  }, [state.historyCommit, filtered]);

  // Keyboard navigation — j/k moves selection and loads commit in right panel
  const navigate = useCallback((delta: 1 | -1) => {
    const next = Math.max(0, Math.min(selectedIdx + delta, filtered.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const entry = filtered[next];
    if (entry) dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
  }, [selectedIdx, filtered, dispatch]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (state.rightPanelFocused) return; // right panel owns these keys when focused
      if (filtered.length === 0) return;
      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); navigate(1); }
      if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); navigate(-1); }
      if (e.key === 'Enter') {
        e.preventDefault();
        const entry = filtered[selectedIdx];
        if (entry) dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
      }
      if (e.key === 'ArrowRight') {
        e.preventDefault();
        dispatch({ type: 'FOCUS_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, filtered, selectedIdx, navigate, dispatch]);

  return (
    <div data-testid="history-timeline" ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ padding: '6px 12px', borderBottom: '1px solid #333', display: 'flex', flexWrap: 'wrap', gap: 4, alignItems: 'center', minHeight: 30 }}>
        {availableOps.map(op => {
          const s = opStyles[op] || defaultOpStyle;
          const active = activeOps.has(op);
          return (
            <span
              key={op}
              onClick={() => toggleOp(op)}
              style={{
                fontSize: 10,
                padding: '2px 8px',
                borderRadius: 8,
                cursor: 'pointer',
                color: active ? '#fff' : s.color,
                background: active ? s.color : s.bg,
                border: `1px solid ${s.color}`,
                opacity: active ? 1 : 0.6,
                userSelect: 'none',
              }}
            >{op}</span>
          );
        })}
        {availableOps.length === 0 && !loading && (
          <span style={{ fontSize: 11, color: '#555' }}>No operations</span>
        )}
      </div>
      <div style={{ flex: 1, overflowY: 'auto', padding: '8px 0' }}>
        {filtered.length === 0 && !loading && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>
            {activeOps.size > 0 ? 'No entries match the selected operations.' : 'No history for this path.'}
          </div>
        )}
        {filtered.map((entry, i) => {
          const isSelected = i === selectedIdx;
          const isHighlighted = state.historyCommit === entry.commit;
          const cs = commitStyle(entry);
          const hasLabel = cs.label !== '';
          const dotSize = hasLabel ? 10 : 6;

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
                background: isSelected || isHighlighted ? '#2a2a3a' : 'transparent',
                cursor: 'pointer',
              }}
              onClick={() => {
                setSelectedIdx(i);
                dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
              }}
            >
              {/* Timeline column: continuous line + dot */}
              <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', width: 20, flexShrink: 0 }}>
                {/* Top connector — hide for first entry */}
                <div style={{ width: 2, background: i === 0 ? 'transparent' : '#333', flex: 1 }} />
                {/* Dot */}
                <div style={{
                  width: dotSize,
                  height: dotSize,
                  borderRadius: '50%',
                  background: cs.color,
                  flexShrink: 0,
                  margin: '2px 0',
                }} />
                {/* Bottom connector — hide for last entry */}
                <div style={{ width: 2, background: i === filtered.length - 1 ? 'transparent' : '#333', flex: 1 }} />
              </div>

              {/* Commit info */}
              <div style={{ flex: 1, minWidth: 0, paddingLeft: 8, paddingTop: 4, paddingBottom: 4 }}>
                {/* Operation badge */}
                {hasLabel && (
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginBottom: 2 }}>
                    <span style={{
                      fontSize: 10,
                      padding: '1px 6px',
                      borderRadius: 8,
                      color: cs.color,
                      background: cs.bg,
                      whiteSpace: 'nowrap',
                    }}>{cs.label}</span>
                  </div>
                )}
                {/* Hash + time + file counts */}
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span
                    onClick={e => {
                      e.stopPropagation();
                      dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
                    }}
                    style={{ fontFamily: 'monospace', fontSize: 12, color: '#8af', cursor: 'pointer' }}
                  >
                    {entry.commit.slice(0, 7)}
                  </span>
                  {entry.files?.added ? <span style={{ fontSize: 9, color: '#7c9', fontFamily: 'monospace' }}>{entry.files.added}A</span> : null}
                  {entry.files?.modified ? <span style={{ fontSize: 9, color: '#8af', fontFamily: 'monospace' }}>{entry.files.modified}M</span> : null}
                  {entry.files?.deleted ? <span style={{ fontSize: 9, color: '#f88', fontFamily: 'monospace' }}>{entry.files.deleted}D</span> : null}
                  <span style={{ fontSize: 11, color: '#666' }}>{relativeTime(entry.date)}</span>
                </div>
                {/* Commit message */}
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
