import { useEffect, useRef, useState, useCallback } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { HistoryEntryWithTags } from './api';
import type { AppState, Action } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

const opStyles: Record<string, { color: string; bg: string; label: string }> = {
  learn:   { color: '#7c9', bg: '#1a2e1a', label: 'learn' },
  update:  { color: '#8af', bg: '#1a1a2e', label: 'update' },
  retract: { color: '#f88', bg: '#2e1a1a', label: 'retract' },
  subsume: { color: '#fa0', bg: '#2e2a1a', label: 'subsume' },
  sync:    { color: '#888', bg: '#222',    label: 'sync' },
};
const defaultStyle = { color: '#555', bg: '#222', label: '' };

function commitStyle(entry: HistoryEntryWithTags): { color: string; bg: string; label: string } {
  // Prefer explicit operation field (new commits).
  if (entry.operation && opStyles[entry.operation]) return opStyles[entry.operation];
  // Fall back to tag prefix detection (legacy commits).
  for (const tag of entry.tags || []) {
    if (tag.startsWith('learn/')) return opStyles.learn;
    if (tag.startsWith('update/')) return opStyles.update;
    if (tag.startsWith('retract/')) return opStyles.retract;
    if (tag.startsWith('synthesize/') || tag.startsWith('subsume/')) return opStyles.subsume;
  }
  return defaultStyle;
}

function relativeTime(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(dateStr).toLocaleDateString();
}

export function HistoryTimeline({ state, dispatch }: Props) {
  const [entries, setEntries] = useState<HistoryEntryWithTags[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const sentinelRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const containerRef = useRef<HTMLDivElement>(null);

  // Fetch first page on mount and when currentPath changes
  useEffect(() => {
    setLoading(true);
    setEntries([]);
    setNextCursor(undefined);
    setSelectedIdx(0);
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
    const idx = entries.findIndex(e => e.commit === state.historyCommit);
    if (idx === -1) return; // not yet loaded
    setSelectedIdx(idx);
    itemRefs.current[idx]?.scrollIntoView({ block: 'nearest' });
  }, [state.historyCommit, entries]);

  // Keyboard navigation — j/k moves selection and loads commit in right panel
  const navigate = useCallback((delta: 1 | -1) => {
    const next = Math.max(0, Math.min(selectedIdx + delta, entries.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const entry = entries[next];
    if (entry) dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
  }, [selectedIdx, entries, dispatch]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (state.rightPanelFocused) return; // right panel owns these keys when focused
      if (entries.length === 0) return;
      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); navigate(1); }
      if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); navigate(-1); }
      if (e.key === 'Enter') {
        e.preventDefault();
        const entry = entries[selectedIdx];
        if (entry) dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
      }
      if (e.key === 'ArrowRight') {
        e.preventDefault();
        dispatch({ type: 'FOCUS_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, entries, selectedIdx, navigate, dispatch]);

  return (
    <div ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ padding: '8px 12px', borderBottom: '1px solid #333', fontSize: 12, color: '#888' }}>
        History: {state.currentPath}
      </div>
      <div style={{ flex: 1, overflowY: 'auto', padding: '8px 0' }}>
        {entries.length === 0 && !loading && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>No history for this path.</div>
        )}
        {entries.map((entry, i) => {
          const isSelected = i === selectedIdx;
          const isHighlighted = state.historyCommit === entry.commit;
          const cs = commitStyle(entry);
          const hasLabel = cs.label !== '';
          const dotSize = hasLabel ? 10 : 6;

          return (
            <div
              key={entry.commit}
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
                <div style={{ width: 2, background: i === entries.length - 1 ? 'transparent' : '#333', flex: 1 }} />
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
                {/* Hash + time */}
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span
                    onClick={e => {
                      e.stopPropagation();
                      dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
                    }}
                    style={{ fontFamily: 'monospace', fontSize: 12, color: '#8af', cursor: 'pointer' }}
                  >
                    {entry.commit.slice(0, 7)}
                  </span>
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
