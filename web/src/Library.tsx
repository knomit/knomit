import { useEffect, useRef, useState, useCallback, useMemo } from 'react';
import { useAsync } from './hooks';
import { EmptyState, LoadingSpinner } from './ui';
import type { Dispatch } from 'react';
import { api } from './api';
import type { DirChild, RecentFactEntry } from './api';
import type { AppState, Action } from './state';
import { currentPath, isLive } from './state';
import { typeStyles, defaultTypeStyle, relativeTimeEpoch } from './utils';
import { TypeIcon, FolderIcon } from './icons';
import { LibraryHeader } from './LibraryHeader';
import type { NavRequest } from './useNavigationManager';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
}

function ReadOnlyBanner({ message }: { message: string }) {
  return (
    <div
      data-testid="library-readonly-banner"
      style={{
        display: 'flex', justifyContent: 'flex-end',
        padding: '4px 12px',
        borderBottom: '1px solid #1a1a1a',
        background: '#0f0f0f',
      }}
    >
      <span style={{ color: '#e5a23c', fontSize: 10, fontFamily: 'monospace' }}>
        {message}
      </span>
    </div>
  );
}

export function Library({ state, dispatch, navigate }: Props) {
  const path = currentPath(state);
  const hasNonPathFilters = state.filters.some(f => f.category !== 'path');
  const searchActive = hasNonPathFilters || !!state.freeText;
  const effectiveSort = searchActive ? 'relevance' : state.librarySort;

  const [children, setChildren] = useState<DirChild[]>([]);
  const [selectedIdx, setSelectedIdx] = useState(-1);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);

  // ── Path sort: api.browse for directory entries ──
  useAsync((stale) => {
    if (effectiveSort !== 'path') return;
    api.browse(state.repo, state.branch, path, state.ontologyRoot).then(r => {
      if (stale()) return;
      const c = (r.children || []).slice().sort((a, b) => {
        if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
        return a.name.localeCompare(b.name);
      });
      setChildren(c);
      if (state.factPath) {
        const factName = state.factPath.split('/').pop();
        const idx = c.findIndex(ch => !ch.is_dir && ch.name === factName);
        setSelectedIdx(idx >= 0 ? idx : -1);
      } else {
        setSelectedIdx(-1);
      }
    }).catch(() => { if (!stale()) setChildren([]); });
  }, [path, state.headCommit, effectiveSort, state.repo, state.branch, state.ontologyRoot, state.factPath]);

  // ── Recent sort: api.recent for chrono entries ──
  const [facts, setFacts] = useState<RecentFactEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const sentinelRef = useRef<HTMLDivElement>(null);
  // Stale ref for use inside the async useAsync callback (state updates between
  // dispatch and resolution would otherwise read closed-over stale values).
  const staleStateRef = useRef(state);
  staleStateRef.current = state;

  const { domains, entities, types, kinds, eps } = useMemo(() => {
    const domains: string[] = [], entities: string[] = [], types: string[] = [], kinds: string[] = [], eps: string[] = [];
    for (const f of state.filters) {
      if (f.category === 'domain') domains.push(f.value);
      else if (f.category === 'entity') entities.push(f.value);
      else if (f.category === 'type') types.push(f.value);
      else if (f.category === 'kind') kinds.push(f.value);
      else if (f.category === 'ep') eps.push(f.value);
    }
    return { domains, entities, types, kinds, eps };
  }, [state.filters]);
  const typeFilter = types.length === 1 ? types[0] : undefined;
  const filtersKey = state.filters.map(f => `${f.category}:${f.value}`).join('\0');

  useAsync((stale) => {
    if (effectiveSort !== 'recent') return;
    setLoading(true);
    setFacts([]);
    api.recent(state.repo, state.branch, path, state.freeText, 50, 0, {
      typeFilter,
      kinds: kinds.length ? kinds : undefined,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
      eps: eps.length ? eps : undefined,
    }).then(r => {
      if (stale()) return;
      setFacts(r.facts || []);
      setLoading(false);
      const loaded = r.facts || [];
      const alreadyInList = loaded.some(f => f.path === staleStateRef.current.factPath);
      if (loaded.length > 0 && !alreadyInList) {
        dispatch({ type: 'AMEND_NAV', factPath: loaded[0].path });
      }
    }).catch(() => { if (!stale()) { setFacts([]); setLoading(false); } });
  }, [path, state.headCommit, state.freeText, state.repo, state.branch, typeFilter, filtersKey, effectiveSort]);

  // ── Relevance sort: api.search for free-text results ──
  useAsync((stale) => {
    if (effectiveSort !== 'relevance') return;
    api.search(state.repo, state.branch, state.freeText, path, 0, {
      types: types.length ? types : undefined,
      kinds: kinds.length ? kinds : undefined,
      eps: eps.length ? eps : undefined,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
    }).then(r => {
      if (stale()) return;
      const items: DirChild[] = (r.results || []).map(sr => ({
        name: sr.path.split('/').pop() || sr.path,
        is_dir: false,
        title: sr.title,
        type: sr.type,
        fullPath: sr.path,
      }));
      setChildren(items);
      const currentFactPath = staleStateRef.current.factPath;
      const matchIdx = items.findIndex(it => it.fullPath === currentFactPath);
      setSelectedIdx(matchIdx >= 0 ? matchIdx : items.length > 0 ? 0 : -1);
      if (matchIdx < 0 && items.length > 0 && items[0].fullPath) {
        dispatch({ type: 'AMEND_NAV', factPath: items[0].fullPath });
      }
    }).catch(() => { if (!stale()) setChildren([]); });
  }, [path, state.headCommit, state.freeText, effectiveSort, state.repo, state.branch, filtersKey]);

  const moveSelection = useCallback((delta: 1 | -1) => {
    const len = children.length;
    if (len === 0) return;
    const next = Math.max(0, Math.min(selectedIdx + delta, len - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const c = children[next];
    if (c && !c.is_dir) {
      navigate({ view: 'library', factPath: c.fullPath || `${path}/${c.name}` });
    }
  }, [children, selectedIdx, path, navigate]);

  const activateSelected = useCallback(() => {
    const child = children[selectedIdx];
    if (!child) return;
    if (child.is_dir) {
      dispatch({ type: 'NAVIGATE', path: `${path}/${child.name}` });
    } else {
      navigate({ view: 'library', factPath: child.fullPath || `${path}/${child.name}` });
    }
  }, [children, selectedIdx, path, dispatch, navigate]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if (state.rightPanelFocused) return;

      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); moveSelection(1); }
      else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); moveSelection(-1); }
      else if (e.key === 'Enter') { e.preventDefault(); activateSelected(); }
      else if (e.key === 'ArrowLeft') {
        e.preventDefault();
        const parts = path.split('/');
        if (parts.length > 1) dispatch({ type: 'GO_UP' });
      }
      else if (e.key === 'ArrowRight') {
        e.preventDefault();
        const child = children[selectedIdx];
        if (!child) return;
        if (child.is_dir) activateSelected();
        else dispatch({ type: 'FOCUS_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, moveSelection, activateSelected, children, selectedIdx, path, dispatch]);

  const hasPathChip = state.filters.some(f => f.category === 'path');

  return (
    <div data-testid="left-panel" data-sort={effectiveSort} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <LibraryHeader
        count={effectiveSort === 'recent' ? facts.length : children.length}
        scoped={hasPathChip}
        sort={state.librarySort}
        searchActive={searchActive}
        onSortChange={(sort) => dispatch({ type: 'SET_LIBRARY_SORT', sort })}
      />
      {!isLive(state) && (
        <ReadOnlyBanner message="Showing live library · scrubbed views not yet supported by backend" />
      )}
      <div style={{ flex: 1, overflowY: 'auto' }}>
        {(effectiveSort === 'path' || effectiveSort === 'relevance') && children.map((c, i) => {
          const ts = (c.type && typeStyles[c.type]) || defaultTypeStyle;
          return (
            <div
              key={c.name}
              data-testid="dir-entry"
              data-name={c.name}
              data-isdir={String(c.is_dir)}
              data-path={c.fullPath || ''}
              ref={el => { itemRefs.current[i] = el; }}
              onClick={() => {
                setSelectedIdx(i);
                if (c.is_dir) {
                  dispatch({ type: 'ADD_FILTER', chip: { category: 'path', value: `${path}/${c.name}` } });
                } else {
                  navigate({ view: 'library', factPath: c.fullPath || `${path}/${c.name}` });
                }
              }}
              style={{
                padding: '8px 12px', cursor: 'pointer',
                background: i === selectedIdx ? '#2a2a3a' : 'transparent',
                borderBottom: '1px solid #222',
                display: 'flex', alignItems: 'center', gap: 8,
              }}
            >
              {c.is_dir ? (
                <span style={{ flexShrink: 0, display: 'flex', alignItems: 'center', opacity: 0.7 }}>
                  <FolderIcon color="#7c9" size={12} />
                </span>
              ) : (
                <span data-testid="fact-type-icon" style={{ flexShrink: 0, display: 'flex', alignItems: 'center' }}>
                  <TypeIcon type={c.type || ''} color={ts.color} size={12} />
                </span>
              )}
              <span style={{ fontSize: 13, color: '#ddd' }}>{c.title || c.name}</span>
            </div>
          );
        })}
        {(effectiveSort === 'path' || effectiveSort === 'relevance') && children.length === 0 && <EmptyState message={effectiveSort === 'relevance' ? 'No facts match the search.' : 'No items in this path.'} />}
        {effectiveSort === 'recent' && (
          <>
            {facts.length === 0 && !loading && (
              <EmptyState message={state.freeText ? 'No facts match the search.' : 'No facts in this path.'} />
            )}
            {facts.map((f, i) => {
              const ts = (f.type && typeStyles[f.type]) || defaultTypeStyle;
              return (
                <div
                  key={f.path}
                  data-testid="chrono-item"
                  data-path={f.path}
                  onClick={() => { setSelectedIdx(i); navigate({ view: 'library', factPath: f.path }); }}
                  style={{
                    padding: '6px 12px', cursor: 'pointer',
                    background: i === selectedIdx ? '#2a2a3a' : 'transparent',
                    borderBottom: '1px solid #1a1a1a',
                  }}
                >
                  <div style={{ fontSize: 12, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ flexShrink: 0, display: 'flex', alignItems: 'center' }}><TypeIcon type={f.type || ''} color={ts.color} size={12} /></span>
                    {f.title}
                  </div>
                  <div style={{ fontSize: 10, color: '#666', marginTop: 1, display: 'flex', gap: 8 }}>
                    <span style={{ fontFamily: 'monospace' }}>{f.path.split('/').pop()}</span>
                    <span>{relativeTimeEpoch(f.committed_at)}</span>
                  </div>
                </div>
              );
            })}
            <div ref={sentinelRef} style={{ height: 1 }} />
            {loading && <LoadingSpinner />}
          </>
        )}
      </div>
    </div>
  );
}
