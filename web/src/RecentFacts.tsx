import { useEffect, useRef, useState, useCallback } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { RecentFactEntry } from './api';
import type { AppState, Action } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

function relativeTime(epoch: number): string {
  if (!epoch) return '';
  const diff = Date.now() - epoch * 1000;
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(epoch * 1000).toLocaleDateString();
}

export function RecentFacts({ state, dispatch }: Props) {
  const [facts, setFacts] = useState<RecentFactEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const [filter, setFilter] = useState('');
  const sentinelRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const searchRef = useRef<HTMLInputElement>(null);

  // Fetch first page when path changes
  useEffect(() => {
    setLoading(true);
    setFacts([]);
    setTotal(0);
    setSelectedIdx(0);
    setFilter('');
    api.recent(state.repo, state.currentPath).then(r => {
      setFacts(r.facts || []);
      setTotal(r.total);
      setLoading(false);
      if (r.facts?.length > 0) dispatch({ type: 'SELECT_FACT', path: r.facts[0].path });
    }).catch(() => { setFacts([]); setLoading(false); });
  }, [state.currentPath, state.headCommit]);

  // Infinite scroll
  const loadMore = useCallback(() => {
    if (loading || facts.length >= total) return;
    setLoading(true);
    api.recent(state.repo, state.currentPath, 50, facts.length).then(r => {
      setFacts(prev => [...prev, ...(r.facts || [])]);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [loading, facts.length, total, state.repo, state.currentPath]);

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

  // Client-side filter on titles
  const filtered = filter
    ? facts.filter(f => f.title.toLowerCase().includes(filter.toLowerCase()))
    : facts;

  // Keyboard navigation
  const navigate = useCallback((delta: 1 | -1) => {
    const next = Math.max(0, Math.min(selectedIdx + delta, filtered.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const f = filtered[next];
    if (f) dispatch({ type: 'SELECT_FACT', path: f.path });
  }, [selectedIdx, filtered, dispatch]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (state.rightPanelFocused) return;
      if (document.activeElement === searchRef.current) {
        if (e.key === 'Escape') { searchRef.current?.blur(); setFilter(''); e.preventDefault(); return; }
        if (e.key === 'ArrowDown') { e.preventDefault(); navigate(1); return; }
        if (e.key === 'ArrowUp') { e.preventDefault(); navigate(-1); return; }
        if (e.key === 'Enter' && filtered.length > 0) {
          e.preventDefault();
          dispatch({ type: 'SELECT_FACT', path: filtered[selectedIdx]?.path || filtered[0].path });
          return;
        }
        return; // let typing happen
      }
      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); navigate(1); }
      else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); navigate(-1); }
      else if (e.key === 'ArrowRight') { e.preventDefault(); dispatch({ type: 'FOCUS_RIGHT_PANEL' }); }
      else if (e.key === '/') { e.preventDefault(); searchRef.current?.focus(); }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, filtered, selectedIdx, navigate, dispatch]);

  return (
    <div ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ padding: '4px 8px', borderBottom: '1px solid #333', flexShrink: 0 }}>
        <input
          ref={searchRef}
          value={filter}
          onChange={e => { setFilter(e.target.value); setSelectedIdx(0); }}
          placeholder="Filter by title…"
          style={{
            width: '100%', background: '#1a1a1a', border: '1px solid #333', borderRadius: 3,
            color: '#ccc', fontSize: 12, padding: '4px 8px', outline: 'none', fontFamily: 'monospace',
            boxSizing: 'border-box',
          }}
        />
      </div>
      <div style={{ flex: 1, overflowY: 'auto', padding: '4px 0' }}>
        {filtered.length === 0 && !loading && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>
            {filter ? 'No facts match the filter.' : 'No facts in this path.'}
          </div>
        )}
        {filtered.map((f, i) => (
          <div
            key={f.path}
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
            <div style={{ fontSize: 12, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {f.title}
            </div>
            <div style={{ fontSize: 10, color: '#666', marginTop: 1, display: 'flex', gap: 8 }}>
              <span style={{ fontFamily: 'monospace' }}>{f.path.split('/').pop()}</span>
              <span>{relativeTime(f.committed_at)}</span>
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
