import { useEffect, useLayoutEffect, useRef, useState, useCallback, useMemo } from 'react';
import { useAsync } from './hooks';
import { EmptyState, LoadingSpinner } from './ui';
import type { Dispatch } from 'react';
import { api } from './api';
import type { HistoryEntryWithTags } from './api';
import type { AppState, Action, AsOf } from './state';
import { currentPath, selectAnchorCommit } from './state';
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

function FileCountChip({ files }: { files?: { added?: number; modified?: number; deleted?: number } }) {
  const a = files?.added ?? 0;
  const m = files?.modified ?? 0;
  const d = files?.deleted ?? 0;
  if (a + m + d === 0) return null;
  return (
    <span style={{
      fontSize: 9, background: '#16161b', padding: '1px 5px', borderRadius: 8,
      fontFamily: 'monospace', marginLeft: 'auto', display: 'flex', gap: 4,
    }}>
      {a > 0 && <span style={{ color: '#7c9' }}>+{a}</span>}
      {m > 0 && <span style={{ color: '#8af' }}>~{m}</span>}
      {d > 0 && <span style={{ color: '#f88' }}>−{d}</span>}
    </span>
  );
}

// Compute the next AsOf for ⌥+click on a row. Returns null when the alt-click
// is degenerate (anchored row, no anchor available) so the caller falls back
// to plain-click semantics.
export function computeAltClick(currentAsOf: AsOf, headCommit: string, rowCommit: string): AsOf | null {
  if (currentAsOf.mode === 'live') {
    if (!headCommit || headCommit === rowCommit) return null;
    return { mode: 'diff', from: headCommit, to: rowCommit };
  }
  if (currentAsOf.mode === 'scrubbed') {
    if (currentAsOf.commit === rowCommit) return null;
    return { mode: 'diff', from: currentAsOf.commit, to: rowCommit };
  }
  // diff mode → re-anchor 'to'
  if (currentAsOf.to === rowCommit) return null;
  return { mode: 'diff', from: currentAsOf.from, to: rowCommit };
}

// Returns true when rowCommit lies between (and including) the diff
// endpoints in the visible entry list. False outside diff mode or when
// either endpoint is not visible.
export function isInRange(asOf: AsOf, rowCommit: string, allEntries: { commit: string }[]): boolean {
  if (asOf.mode !== 'diff') return false;
  const fromIdx = allEntries.findIndex(e => e.commit === asOf.from);
  const toIdx = allEntries.findIndex(e => e.commit === asOf.to);
  if (fromIdx < 0 || toIdx < 0) return false;
  const idx = allEntries.findIndex(e => e.commit === rowCommit);
  return idx >= Math.min(fromIdx, toIdx) && idx <= Math.max(fromIdx, toIdx);
}

export function HistoryTimeline({ state, dispatch, navigate }: Props) {
  const [entries, setEntries] = useState<HistoryEntryWithTags[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [prevCursor, setPrevCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const sentinelRef = useRef<HTMLDivElement>(null);
  const topSentinelRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const containerRef = useRef<HTMLDivElement>(null);
  const scrollElRef = useRef<HTMLDivElement>(null);
  const scrollHeightBeforeRef = useRef(0);

  const path = currentPath(state);

  const staleStateRef = useRef(state);
  staleStateRef.current = state;

  useAsync((stale) => {
    setLoading(true);
    setEntries([]);
    setNextCursor(undefined);
    setPrevCursor(undefined);
    setSelectedIdx(0);
    api.history(state.repo, state.branch, path, undefined, selectAnchorCommit(state) || undefined).then(r => {
      if (stale()) return;
      const e = r.entries || [];
      setEntries(e);
      setNextCursor(r.next);
      setPrevCursor(r.prev);
      setLoading(false);
      // If anchor is already set (e.g. NAV_BACK restored it), just sync visual index.
      const anchor = selectAnchorCommit(staleStateRef.current);
      if (anchor) {
        const idx = e.findIndex(c => c.commit === anchor);
        if (idx >= 0) setSelectedIdx(idx);
        return;
      }
      // No explicit selection — amend current nav entry in-place (no navStack push).
      if (e.length > 0) {
        dispatch({
          type: 'AMEND_NAV',
          factPath: null,
          asOf: { mode: 'scrubbed', commit: e[0].commit },
        });
      }
    }).catch(() => {
      if (!stale()) { setEntries([]); setLoading(false); }
    });
  }, [path, state.repo, state.branch]);

  const loadingRef = useRef(loading);
  loadingRef.current = loading;

  const loadMore = useCallback(() => {
    if (loadingRef.current || !nextCursor) return;
    setLoading(true);
    api.history(state.repo, state.branch, path, nextCursor).then(r => {
      setEntries(prev => [...prev, ...(r.entries || [])]);
      setNextCursor(r.next);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [nextCursor, path, state.repo, state.branch]);

  const loadPrev = useCallback(() => {
    if (loadingRef.current || !prevCursor) return;
    setLoading(true);
    scrollHeightBeforeRef.current = scrollElRef.current?.scrollHeight ?? 0;
    api.history(state.repo, state.branch, path, undefined, undefined, prevCursor).then(r => {
      const newEntries = r.entries || [];
      if (newEntries.length === 0) {
        scrollHeightBeforeRef.current = 0;
        setPrevCursor(undefined);
        setLoading(false);
        return;
      }
      setEntries(prev => [...newEntries, ...prev]);
      setSelectedIdx(idx => idx + newEntries.length);
      setPrevCursor(r.prev);
      setLoading(false);
    }).catch(() => { scrollHeightBeforeRef.current = 0; setLoading(false); });
  }, [prevCursor, path, state.repo, state.branch]);

  // After prepending entries, restore scroll position so the viewport doesn't jump.
  useLayoutEffect(() => {
    if (scrollHeightBeforeRef.current > 0 && scrollElRef.current) {
      scrollElRef.current.scrollTop += scrollElRef.current.scrollHeight - scrollHeightBeforeRef.current;
      scrollHeightBeforeRef.current = 0;
    }
  }, [entries]);

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

  useEffect(() => {
    const sentinel = topSentinelRef.current;
    if (!sentinel || !prevCursor) return;
    const observer = new IntersectionObserver(
      ([entry]) => { if (entry.isIntersecting) loadPrev(); },
      { root: containerRef.current, threshold: 0.1 }
    );
    observer.observe(sentinel);
    return () => observer.disconnect();
  }, [loadPrev]);

  const epFiltersKey = state.filters.filter(f => f.category === 'ep').map(f => f.value).join('\0');
  const epFilters = useMemo(
    () => state.filters.filter(f => f.category === 'ep').map(f => f.value),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [epFiltersKey],
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
    const anchor = selectAnchorCommit(state);
    if (!anchor) return;
    const idx = filteredEntries.findIndex(e => e.commit === anchor);
    if (idx >= 0 && idx !== selectedIdx) {
      setSelectedIdx(idx);
      itemRefs.current[idx]?.scrollIntoView({ block: 'nearest' });
    }
  }, [state.asOf, filteredEntries]);

  const moveSelection = useCallback((delta: 1 | -1) => {
    const next = Math.max(0, Math.min(selectedIdx + delta, filteredEntries.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const entry = filteredEntries[next];
    // Dispatch synchronously so the anchor tracks local selectedIdx without lag.
    // Keep the current factPath so the right panel doesn't flash through the stats view while
    // CommitPanel fetches the new commit detail. CommitPanel will switch to the first file if
    // the current fact doesn't exist in the new commit.
    if (entry) {
      dispatch({
        type: 'APPLY_NAV',
        view: 'history',
        factPath: staleStateRef.current.factPath,
        asOf: { mode: 'scrubbed', commit: entry.commit },
      });
    }
  }, [selectedIdx, filteredEntries, dispatch]);

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
  }, [state.rightPanelFocused, state.view, filteredEntries, moveSelection, dispatch]);

  const handleRowClick = (entry: HistoryEntryWithTags, i: number, alt: boolean) => {
    setSelectedIdx(i);
    if (alt) {
      const next = computeAltClick(state.asOf, state.headCommit, entry.commit);
      if (next) {
        dispatch({ type: 'SET_AS_OF', asOf: next });
        return;
      }
      // fallthrough to plain click
    }
    navigate({
      view: 'history',
      factPath: null,
      asOf: { mode: 'scrubbed', commit: entry.commit },
    });
  };

  return (
    <div data-testid="history-timeline" ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{
        padding: '6px 14px 8px', fontSize: 9, fontFamily: 'monospace',
        color: '#5a5a65', display: 'flex', gap: 8, alignItems: 'center', flexShrink: 0,
      }}>
        <span style={{ color: '#e5a23c' }}>● scrub</span>
        <span style={{ color: '#3a3a42' }}>·</span>
        <span style={{ color: '#7c9' }}>┃ HEAD</span>
        <span style={{ color: '#3a3a42' }}>·</span>
        <span>⌥+click = range</span>
        {state.asOf.mode === 'diff' && <>
          <span style={{ color: '#3a3a42' }}>·</span>
          <span>click = exit range</span>
        </>}
      </div>
      <div ref={scrollElRef} style={{ flex: 1, overflowY: 'auto', padding: '8px 0' }}>
        <div ref={topSentinelRef} style={{ height: 1 }} />
        {filteredEntries.length === 0 && !loading && (
          <EmptyState message={entries.length === 0 ? 'No history for this path.' : 'No commits match the current filters.'} />
        )}
        {filteredEntries.map((entry, i) => {
          const isAnchor = entry.commit === selectAnchorCommit(state);
          const isHead = i === 0;
          const inRange = isInRange(state.asOf, entry.commit, filteredEntries);
          const cs = commitStyle(entry);
          const hasLabel = cs.label !== '';
          const anchorActive = isAnchor && state.asOf.mode !== 'live';
          const dotSize = anchorActive ? 12 : (hasLabel ? 10 : 6);

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
                background: anchorActive
                  ? 'rgba(229,162,60,0.08)'
                  : isAnchor ? '#2a2a3a'
                  : inRange ? 'rgba(229,162,60,0.04)'
                  : 'transparent',
                borderLeft: anchorActive
                  ? '3px solid #e5a23c'
                  : isAnchor ? `3px solid ${cs.color}` : '3px solid transparent',
                cursor: 'pointer',
              }}
              onClick={(e) => handleRowClick(entry, i, e.altKey)}
            >
              {/* Timeline column: continuous line + dot */}
              <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', width: 20, flexShrink: 0 }}>
                <div style={{ width: 2, background: i === 0 ? 'transparent' : (inRange ? '#e5a23c' : '#333'), flex: 1 }} />
                <div
                  style={{
                    width: dotSize,
                    height: dotSize,
                    borderRadius: '50%',
                    background: anchorActive ? '#e5a23c' : cs.color,
                    boxShadow: anchorActive ? '0 0 0 3px #0b0b0d, 0 0 8px #e5a23c' : 'none',
                    flexShrink: 0,
                    margin: '2px 0',
                  }}
                />
                <div style={{ width: 2, background: i === filteredEntries.length - 1 ? 'transparent' : (inRange ? '#e5a23c' : '#333'), flex: 1 }} />
              </div>

              {/* Commit info */}
              <div style={{ flex: 1, minWidth: 0, paddingLeft: 8, paddingTop: 4, paddingBottom: 4 }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 3 }}>
                  {hasLabel && (
                    <span style={{
                      fontSize: 10, padding: '1px 6px', borderRadius: 8,
                      color: cs.color, background: cs.bg, whiteSpace: 'nowrap',
                    }}>{cs.label}</span>
                  )}
                  <span style={{ fontFamily: 'monospace', fontSize: 12, color: '#8af' }}>
                    {entry.commit.slice(0, 7)}
                  </span>
                  {isHead && (
                    <span style={{
                      fontSize: 8, color: '#7c9', fontFamily: 'monospace',
                      letterSpacing: 0.6, padding: '0 4px', border: '1px solid #77cc9955',
                      borderRadius: 2,
                    }}>HEAD</span>
                  )}
                  {anchorActive && (
                    <span style={{
                      fontSize: 8, color: '#e5a23c', fontFamily: 'monospace',
                      letterSpacing: 0.6, padding: '0 4px', border: '1px solid #e5a23c55',
                      borderRadius: 2,
                    }}>AT</span>
                  )}
                  <span style={{ fontSize: 11, color: '#666' }}>{relativeTime(entry.date)}</span>
                  <FileCountChip files={entry.files} />
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
        {loading && <LoadingSpinner />}
      </div>
    </div>
  );
}
