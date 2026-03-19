import { useEffect, useRef, useState, useCallback } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { RecentFactEntry } from './api';
import type { AppState, Action } from './state';
import { relativeTimeEpoch, opStyles } from './utils';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function RecentFacts({ state, dispatch }: Props) {
  const [facts, setFacts] = useState<RecentFactEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const [query, setQuery] = useState('');
  const [activeQuery, setActiveQuery] = useState(''); // debounced query sent to backend
  const sentinelRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const searchRef = useRef<HTMLInputElement>(null);

  // Debounce query input
  useEffect(() => {
    if (query === activeQuery) return;
    const t = setTimeout(() => setActiveQuery(query), 300);
    return () => clearTimeout(t);
  }, [query]);

  // Fetch when path or activeQuery changes
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setFacts([]);
    setTotal(0);
    setSelectedIdx(0);
    api.recent(state.repo, state.currentPath, activeQuery).then(r => {
      if (cancelled) return;
      setFacts(r.facts || []);
      setTotal(r.total);
      setLoading(false);
      if (r.facts?.length > 0) dispatch({ type: 'SELECT_FACT', path: r.facts[0].path });
    }).catch(() => { if (!cancelled) { setFacts([]); setLoading(false); } });
    return () => { cancelled = true; };
  }, [state.currentPath, state.headCommit, activeQuery]);

  // Infinite scroll
  const loadMore = useCallback(() => {
    if (loading || facts.length >= total) return;
    setLoading(true);
    api.recent(state.repo, state.currentPath, activeQuery, 50, facts.length).then(r => {
      setFacts(prev => [...prev, ...(r.facts || [])]);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [loading, facts.length, total, state.repo, state.currentPath, activeQuery]);

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
    const next = Math.max(0, Math.min(selectedIdx + delta, facts.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const f = facts[next];
    if (f) dispatch({ type: 'SELECT_FACT', path: f.path });
  }, [selectedIdx, facts, dispatch]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (state.rightPanelFocused) return;
      if (document.activeElement === searchRef.current) {
        if (e.key === 'Escape') { searchRef.current?.blur(); setQuery(''); e.preventDefault(); return; }
        if (e.key === 'ArrowDown') { e.preventDefault(); navigate(1); return; }
        if (e.key === 'ArrowUp') { e.preventDefault(); navigate(-1); return; }
        if (e.key === 'Enter' && facts.length > 0) {
          e.preventDefault();
          dispatch({ type: 'SELECT_FACT', path: facts[selectedIdx]?.path || facts[0].path });
          return;
        }
        return;
      }
      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); navigate(1); }
      else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); navigate(-1); }
      else if (e.key === 'ArrowRight') { e.preventDefault(); dispatch({ type: 'FOCUS_RIGHT_PANEL' }); }
      else if (e.key === '/') { e.preventDefault(); searchRef.current?.focus(); }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, facts, selectedIdx, navigate, dispatch]);

  return (
    <div data-testid="recent-list" ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ padding: '4px 8px', borderBottom: '1px solid #333', flexShrink: 0, position: 'relative' }}>
        <input
          data-testid="recent-search-input"
          ref={searchRef}
          value={query}
          onChange={e => { setQuery(e.target.value); setSelectedIdx(0); }}
          placeholder="Search facts…"
          style={{
            width: '100%', background: '#1a1a1a', border: '1px solid #333', borderRadius: 3,
            color: '#ccc', fontSize: 12, padding: '4px 24px 4px 8px', outline: 'none', fontFamily: 'monospace',
            boxSizing: 'border-box',
          }}
        />
        {query && (
          <button data-testid="recent-search-clear" onClick={() => { setQuery(''); searchRef.current?.focus(); }}
            style={{ position: 'absolute', right: 12, top: '50%', transform: 'translateY(-50%)', background: 'none', border: 'none', color: '#666', cursor: 'pointer', fontSize: 12, padding: '2px 4px', lineHeight: 1 }}
            onMouseEnter={e => { e.currentTarget.style.color = '#aaa'; }}
            onMouseLeave={e => { e.currentTarget.style.color = '#666'; }}
          >✕</button>
        )}
      </div>
      <div style={{ flex: 1, overflowY: 'auto', padding: '4px 0' }}>
        {facts.length === 0 && !loading && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>
            {activeQuery ? 'No facts match the search.' : 'No facts in this path.'}
          </div>
        )}
        {facts.map((f, i) => (
          <div
            key={f.path}
            data-testid="recent-item"
            data-path={f.path}
            ref={el => { itemRefs.current[i] = el; }}
            onClick={() => {
              setSelectedIdx(i);
              dispatch({ type: 'SELECT_FACT', path: f.path });
            }}
            style={{
              padding: '6px 12px', cursor: 'pointer',
              background: i === selectedIdx ? '#2a2a3a' : 'transparent',
              borderBottom: '1px solid #1a1a1a',
            }}
          >
            <div style={{ fontSize: 12, color: (f.operation && opStyles[f.operation]?.color) || '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {f.title}
            </div>
            <div style={{ fontSize: 10, color: '#666', marginTop: 1, display: 'flex', gap: 8 }}>
              <span style={{ fontFamily: 'monospace' }}>{f.path.split('/').pop()}</span>
              <span>{relativeTimeEpoch(f.committed_at)}</span>
              {f.score != null && f.score > 0 && <span style={{ color: '#7c9' }}>{Math.round(f.score)}%</span>}
            </div>
          </div>
        ))}
        <div ref={sentinelRef} style={{ height: 1 }} />
        {loading && (
          <div style={{ padding: 12, color: '#666', fontSize: 12, textAlign: 'center' }}>Loading...</div>
        )}
      </div>
    </div>
  );
}
