import { useEffect, useRef, useState } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { DirChild, SearchResult } from './api';
import type { AppState, Action } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function LeftPanel({ state, dispatch }: Props) {
  const [children, setChildren] = useState<DirChild[]>([]);
  const [searchResults, setSearchResults] = useState<SearchResult[]>([]);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const searchRef = useRef<HTMLInputElement>(null);

  // Load directory listing
  useEffect(() => {
    if (state.searchQuery) return;
    api.browse(state.currentPath).then(r => setChildren(r.children || [])).catch(() => setChildren([]));
    setSelectedIdx(0);
  }, [state.currentPath, state.searchQuery]);

  // Search
  useEffect(() => {
    if (!state.searchQuery) { setSearchResults([]); return; }
    const t = setTimeout(() => {
      api.search(state.searchQuery).then(r => setSearchResults(r.results || [])).catch(() => setSearchResults([]));
    }, 300);
    return () => clearTimeout(t);
  }, [state.searchQuery]);

  // Keyboard: expose searchRef for '/' shortcut in App
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (document.activeElement === searchRef.current) return;
      if (e.key === '/') { e.preventDefault(); searchRef.current?.focus(); }
      if (e.key === 'Escape') { dispatch({ type: 'CLEAR_SEARCH' }); searchRef.current?.blur(); }
      if (e.key === 'ArrowDown' || e.key === 'j') setSelectedIdx(i => Math.min(i + 1, listLen() - 1));
      if (e.key === 'ArrowUp' || e.key === 'k') setSelectedIdx(i => Math.max(i - 1, 0));
      if (e.key === 'Enter') activateSelected();
      if ((e.key === 'Backspace' || e.key === 'u') && !state.searchQuery) dispatch({ type: 'GO_UP' });
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  });

  const listLen = () => state.searchQuery ? searchResults.length : (state.currentPath !== 'know' ? 1 : 0) + children.length;

  const activateSelected = () => {
    if (state.searchQuery) {
      const r = searchResults[selectedIdx];
      if (r) dispatch({ type: 'SELECT_FACT', path: r.path });
      return;
    }
    const offset = state.currentPath !== 'know' ? 1 : 0;
    if (selectedIdx === 0 && state.currentPath !== 'know') { dispatch({ type: 'GO_UP' }); return; }
    const child = children[selectedIdx - offset];
    if (!child) return;
    if (child.is_dir) dispatch({ type: 'NAVIGATE', path: `${state.currentPath}/${child.name}` });
    else dispatch({ type: 'SELECT_FACT', path: `${state.currentPath}/${child.name}` });
  };

  const pathLabel = (name: string) => name.replace(/\.md$/, '');

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      {/* Search row — action buttons go on the right in the future */}
      <div style={{ padding: '6px 8px', borderBottom: '1px solid #333', display: 'flex', gap: 6, alignItems: 'center' }}>
        <input
          ref={searchRef}
          type="text"
          placeholder="Search… (/)"
          value={state.searchQuery}
          onChange={e => dispatch({ type: 'SEARCH', query: e.target.value })}
          onKeyDown={e => { if (e.key === 'Escape') { dispatch({ type: 'CLEAR_SEARCH' }); e.currentTarget.blur(); } }}
          style={{ flex: 1, background: '#1a1a1a', border: '1px solid #333', color: '#eee', padding: '5px 8px', borderRadius: 4, fontSize: 12 }}
        />
        {/* action buttons slot */}
      </div>

      <div style={{ flex: 1, overflowY: 'auto' }}>
        {state.searchQuery ? (
          searchResults.length === 0 ? (
            <div style={{ padding: 16, color: '#666', fontSize: 13 }}>No results</div>
          ) : searchResults.map((r, i) => (
            <div key={r.path} onClick={() => { setSelectedIdx(i); dispatch({ type: 'SELECT_FACT', path: r.path }); }}
              style={{ padding: '8px 12px', cursor: 'pointer', background: i === selectedIdx ? '#2a2a3a' : 'transparent', borderBottom: '1px solid #222' }}>
              <div style={{ fontSize: 13, color: '#ddd' }}>{r.title || pathLabel(r.path)}</div>
              <div style={{ fontSize: 11, color: '#666', fontFamily: 'monospace' }}>{r.path}</div>
              <div style={{ fontSize: 11, color: r.score > 0.75 ? '#4caf50' : r.score > 0.5 ? '#ff9800' : '#888' }}>
                score: {r.score.toFixed(2)}
              </div>
            </div>
          ))
        ) : (
          <>
            {state.currentPath !== 'know' && (
              <div key=".." onClick={() => dispatch({ type: 'GO_UP' })}
                style={{ padding: '8px 12px', cursor: 'pointer', background: selectedIdx === 0 ? '#2a2a3a' : 'transparent', borderBottom: '1px solid #222', color: '#888', fontSize: 13 }}>
                ↑ ..
              </div>
            )}
            {children.map((c, i) => {
              const idx = i + (state.currentPath !== 'know' ? 1 : 0);
              return (
                <div key={c.name}
                  onClick={() => {
                    setSelectedIdx(idx);
                    if (c.is_dir) dispatch({ type: 'NAVIGATE', path: `${state.currentPath}/${c.name}` });
                    else dispatch({ type: 'SELECT_FACT', path: `${state.currentPath}/${c.name}` });
                  }}
                  style={{ padding: '8px 12px', cursor: 'pointer', background: idx === selectedIdx ? '#2a2a3a' : 'transparent', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{ fontSize: 16 }}>{c.is_dir ? '📁' : '📄'}</span>
                  <span style={{ fontSize: 13, color: '#ddd' }}>{c.is_dir ? c.name : pathLabel(c.name)}</span>
                </div>
              );
            })}
            {children.length === 0 && (
              <div style={{ padding: 16, color: '#666', fontSize: 13 }}>No items in this path.</div>
            )}
          </>
        )}
      </div>
    </div>
  );
}
