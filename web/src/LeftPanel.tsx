import { useEffect, useRef, useState } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { DirChild, SearchResult } from './api';
import type { AppState, Action } from './state';
import { HistoryTimeline } from './HistoryTimeline';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function LeftPanel({ state, dispatch }: Props) {
  const [children, setChildren] = useState<DirChild[]>([]);
  const [searchResults, setSearchResults] = useState<SearchResult[]>([]);
  const [searchReady, setSearchReady] = useState(false); // true once results loaded for current query
  const [selectedIdx, setSelectedIdx] = useState(0);
  const searchRef = useRef<HTMLInputElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const prevPathRef = useRef(state.currentPath);
  const prevSearchRef = useRef(state.searchQuery);

  // Load directory listing; auto-preview first item so right panel is always in sync
  // Re-fetches when headCommit changes (e.g. after sync) but preserves selection
  useEffect(() => {
    if (state.searchQuery || state.similarTo) return;
    const isHeadChangeOnly = state.currentPath === prevPathRef.current && state.searchQuery === prevSearchRef.current;
    prevPathRef.current = state.currentPath;
    prevSearchRef.current = state.searchQuery;
    api.browse(state.repo, state.currentPath).then(r => {
      const c = r.children || [];
      setChildren(c);
      if (!isHeadChangeOnly) {
        setSelectedIdx(-1);
      }
    }).catch(() => setChildren([]));
  }, [state.currentPath, state.searchQuery, state.headCommit]);

  // Similarity search — sends fact text through the regular search endpoint
  useEffect(() => {
    if (!state.similarTo) return;
    setSearchReady(false);
    setSelectedIdx(0);
    const p = new URLSearchParams({ q: state.similarTo.text, limit: '50' });
    fetch(`/api/v1/${state.repo}/search?${p}`).then(r => r.json()).then(r => {
      const results = (r.results || []).filter((sr: { path: string }) => sr.path !== state.similarTo!.path);
      setSearchResults(results);
      setSearchReady(true);
      if (results.length > 0) dispatch({ type: 'SELECT_FACT', path: results[0].path });
    }).catch(() => { setSearchResults([]); setSearchReady(true); });
  }, [state.similarTo, state.headCommit]);

  // Search — re-runs when headCommit changes to refresh results
  useEffect(() => {
    if (!state.searchQuery) { if (!state.similarTo) { setSearchResults([]); setSearchReady(false); } return; }
    const isHeadChangeOnly = state.searchQuery === prevSearchRef.current;
    if (!isHeadChangeOnly) setSearchReady(false);
    prevSearchRef.current = state.searchQuery;
    const savedIdx = selectedIdx;
    setSelectedIdx(0);
    const t = setTimeout(() => {
      api.search(state.repo, state.searchQuery).then(r => {
        const results = r.results || [];
        setSearchResults(results);
        setSearchReady(true);
        if (isHeadChangeOnly) {
          // Preserve selection position, clamped to new results length
          setSelectedIdx(Math.min(savedIdx, results.length - 1));
        } else {
          setSelectedIdx(0);
          if (results.length > 0) dispatch({ type: 'SELECT_FACT', path: results[0].path });
        }
      }).catch(() => { setSearchResults([]); setSearchReady(true); });
    }, 300);
    return () => clearTimeout(t);
  }, [state.searchQuery, state.headCommit]);

  const isSearchMode = !!(state.searchQuery || state.similarTo) && searchReady;
  const listLen = () => isSearchMode ? searchResults.length : children.length;

  const previewItem = (idx: number) => {
    if (isSearchMode) {
      const r = searchResults[idx];
      if (r) dispatch({ type: 'SELECT_FACT', path: r.path });
    } else {
      const c = children[idx];
      if (!c) return;
      if (c.is_dir) dispatch({ type: 'PREVIEW_DIR', path: `${state.currentPath}/${c.name}` });
      else dispatch({ type: 'SELECT_FACT', path: `${state.currentPath}/${c.name}` });
    }
  };

  const moveSelection = (delta: 1 | -1) => {
    // ArrowUp at top → go one level up (browse mode only)
    if (delta === -1 && selectedIdx === 0 && !isSearchMode) {
      dispatch({ type: 'GO_UP' });
      return;
    }
    const next = Math.max(0, Math.min(selectedIdx + delta, listLen() - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    previewItem(next);
  };

  // Enter or Right: open/navigate into selected item
  const activateSelected = () => {
    if (isSearchMode) {
      const r = searchResults[selectedIdx];
      if (r) dispatch({ type: 'SELECT_FACT', path: r.path });
      return;
    }
    const child = children[selectedIdx];
    if (!child) return;
    if (child.is_dir) dispatch({ type: 'NAVIGATE', path: `${state.currentPath}/${child.name}` });
    else dispatch({ type: 'SELECT_FACT', path: `${state.currentPath}/${child.name}` });
  };

  // Keyboard: global shortcuts when search input is NOT focused
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if (e.key === '/') { e.preventDefault(); searchRef.current?.focus(); }
      if (e.key === 'Escape') {
        if (state.leftMode === 'history') {
          dispatch({ type: 'EXIT_HISTORY' });
        } else {
          dispatch({ type: 'CLEAR_SEARCH' });
          searchRef.current?.blur();
        }
      }
      if (e.key === 'h' && state.leftMode !== 'history') { e.preventDefault(); dispatch({ type: 'ENTER_HISTORY' }); }
      if (e.key === 'Backspace' || e.key === 'Delete') { e.preventDefault(); dispatch({ type: 'NAV_BACK' }); }
      // Browse-mode only shortcuts — skip when in history mode (HistoryTimeline handles its own keys)
      if (state.leftMode === 'history') return;
      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); moveSelection(1); }
      if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); moveSelection(-1); }
      if (e.key === 'ArrowRight' || e.key === 'Enter') { e.preventDefault(); activateSelected(); }
      if (e.key === 'ArrowLeft' && !isSearchMode) { e.preventDefault(); dispatch({ type: 'GO_UP' }); }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  });

  const pathLabel = (name: string) => name.replace(/\.md$/, '');

  if (state.leftMode === 'history') {
    return <HistoryTimeline state={state} dispatch={dispatch} />;
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      {/* Search row */}
      <div style={{ padding: '6px 8px', borderBottom: '1px solid #333' }}>
        <div style={{ position: 'relative' }}>
          <input
            ref={searchRef}
            type="text"
            placeholder={state.similarTo ? `Similar to: ${state.similarTo.path.replace(/\.md$/, '')}` : 'Search… (/)'}
            value={state.searchQuery}
            onChange={e => dispatch({ type: 'SEARCH', query: e.target.value })}
            onKeyDown={e => {
              if (e.key === 'Escape') { dispatch({ type: 'CLEAR_SEARCH' }); e.currentTarget.blur(); }
              if (e.key === 'ArrowDown') { e.preventDefault(); moveSelection(1); }
              if (e.key === 'ArrowUp') { e.preventDefault(); moveSelection(-1); }
              if (e.key === 'Enter') { e.preventDefault(); activateSelected(); }
            }}
            style={{ width: '100%', boxSizing: 'border-box', background: '#1a1a1a', border: '1px solid #333', color: '#eee', padding: '5px 24px 5px 8px', borderRadius: 4, fontSize: 12 }}
          />
          {(state.searchQuery || state.similarTo) && (
            <button onClick={() => { dispatch({ type: 'CLEAR_SEARCH' }); searchRef.current?.focus(); }}
              style={{ position: 'absolute', right: 4, top: '50%', transform: 'translateY(-50%)', background: 'none', border: 'none', color: '#666', cursor: 'pointer', fontSize: 12, padding: '2px 4px', lineHeight: 1 }}
              onMouseEnter={e => { e.currentTarget.style.color = '#aaa'; }}
              onMouseLeave={e => { e.currentTarget.style.color = '#666'; }}
            >✕</button>
          )}
        </div>
      </div>

      <div style={{ flex: 1, overflowY: 'auto' }}>
        {isSearchMode ? (
          searchResults.length === 0 ? (
            <div style={{ padding: 16, color: '#666', fontSize: 13 }}>
              {state.similarTo ? 'No similar facts found' : 'No results'}
            </div>
          ) : searchResults.map((r, i) => (
            <div key={r.path} ref={el => { itemRefs.current[i] = el; }} onClick={() => { setSelectedIdx(i); dispatch({ type: 'SELECT_FACT', path: r.path }); }}
              style={{ padding: '8px 12px', cursor: 'pointer', background: i === selectedIdx ? '#2a2a3a' : 'transparent', borderBottom: '1px solid #222' }}>
              <div style={{ fontSize: 13, color: '#ddd' }}>{r.title || pathLabel(r.path)}</div>
              <div style={{ fontSize: 11, color: '#666', fontFamily: 'monospace' }}>{r.path}</div>
              <div style={{ fontSize: 11, color: r.score > 75 ? '#4caf50' : r.score > 50 ? '#ff9800' : '#888' }}>
                score: {Math.round(r.score)}
              </div>
            </div>
          ))
        ) : (
          <>
            {children.map((c, i) => (
                <div key={c.name} ref={el => { itemRefs.current[i] = el; }}
                  onClick={() => {
                    setSelectedIdx(i);
                    if (c.is_dir) dispatch({ type: 'NAVIGATE', path: `${state.currentPath}/${c.name}` });
                    else dispatch({ type: 'SELECT_FACT', path: `${state.currentPath}/${c.name}` });
                  }}
                  style={{ padding: '8px 12px', cursor: 'pointer', background: i === selectedIdx ? '#2a2a3a' : 'transparent', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{ width: 6, height: 6, borderRadius: '50%', background: c.is_dir ? '#7c9' : '#8af', flexShrink: 0, opacity: 0.7 }} />
                  <span style={{ fontSize: 13, color: '#ddd' }}>{c.is_dir ? c.name : pathLabel(c.name)}</span>
                </div>
            ))}
            {children.length === 0 && (
              <div style={{ padding: 16, color: '#666', fontSize: 13 }}>No items in this path.</div>
            )}
          </>
        )}
      </div>
    </div>
  );
}
