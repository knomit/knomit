import { useEffect, useRef, useState, useCallback } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { DirChild, RecentFactEntry } from './api';
import type { AppState, Action } from './state';
import { currentPath } from './state';
import { HistoryTimeline } from './HistoryTimeline';
import { typeStyles, defaultTypeStyle, relativeTimeEpoch } from './utils';
import { TypeIcon, FolderIcon } from './icons';
import type { NavRequest } from './useNavigationManager';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
}

// ---------- TreeView ----------

function TreeView({ state, dispatch, navigate }: Props) {
  const [children, setChildren] = useState<DirChild[]>([]);
  const [selectedIdx, setSelectedIdx] = useState(-1);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const path = currentPath(state);

  // Determine if we should search instead of browse
  const hasNonPathFilters = state.filters.some(f => f.category !== 'path');
  const shouldSearch = hasNonPathFilters || !!state.freeText;

  // Search: fetch results when filters/freeText change (NOT when selectedFact changes)
  useEffect(() => {
    if (!shouldSearch) return;
    let stale = false;
    const domains = state.filters.filter(f => f.category === 'domain').map(f => f.value);
    const entities = state.filters.filter(f => f.category === 'entity').map(f => f.value);
    const types = state.filters.filter(f => f.category === 'type').map(f => f.value);
    const eps = state.filters.filter(f => f.category === 'ep').map(f => f.value);
    api.search(state.repo, state.freeText, path, 0, { types, eps, domains, entities }).then(r => {
      if (stale) return; // Don't overwrite if filters changed while fetching
      const items: DirChild[] = (r.results || []).map(sr => ({
        name: sr.path.split('/').pop() || sr.path,
        is_dir: false,
        title: sr.title,
        type: sr.type,
        fullPath: sr.path,
      }));
      setChildren(items);
      setSelectedIdx(items.length > 0 ? 0 : -1);
      if (items.length > 0 && items[0].fullPath) {
        dispatch({ type: 'AMEND_NAV', historyCommit: null, factPath: items[0].fullPath, factCommit: null });
      }
    }).catch(() => { if (!stale) setChildren([]); });
    return () => { stale = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, state.headCommit, state.freeText, shouldSearch, state.repo, state.filters]);

  // Browse: fetch directory when path/headCommit/selectedFact changes
  useEffect(() => {
    if (shouldSearch) return;
    let stale = false;
    api.browse(state.repo, path).then(r => {
      if (stale) return; // Don't overwrite if filters were added while fetching
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
    }).catch(() => { if (!stale) setChildren([]); });
    return () => { stale = true; };
  }, [path, state.headCommit, shouldSearch, state.repo, state.factPath]);

  const moveSelection = useCallback((delta: 1 | -1) => {
    const len = children.length;
    if (len === 0) return;
    const next = Math.max(0, Math.min(selectedIdx + delta, len - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const c = children[next];
    if (c && !c.is_dir) {
      navigate({ view: 'tree', factPath: c.fullPath || `${path}/${c.name}` });
    }
  }, [children, selectedIdx, path, navigate]);

  const activateSelected = useCallback(() => {
    const child = children[selectedIdx];
    if (!child) return;
    if (child.is_dir) {
      dispatch({ type: 'NAVIGATE', path: `${path}/${child.name}` });
    } else {
      navigate({ view: 'tree', factPath: child.fullPath || `${path}/${child.name}` });
    }
  }, [children, selectedIdx, path, dispatch, navigate]);

  // Keyboard: j/k navigation, Enter to open, ArrowLeft to go up
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if (state.rightPanelFocused) return;
      if (state.view !== 'tree') return;

      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); moveSelection(1); }
      else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); moveSelection(-1); }
      else if (e.key === 'Enter') { e.preventDefault(); activateSelected(); }
      else if (e.key === 'ArrowLeft') {
        e.preventDefault();
        // Go up directory: remove last path segment from path chip
        const parts = path.split('/');
        if (parts.length > 1) {
          dispatch({ type: 'GO_UP' });
        }
      }
      else if (e.key === 'ArrowRight') {
        e.preventDefault();
        const child = children[selectedIdx];
        if (!child) return;
        if (child.is_dir) {
          activateSelected();
        } else {
          dispatch({ type: 'FOCUS_RIGHT_PANEL' });
        }
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, state.view, moveSelection, activateSelected, children, selectedIdx, path, dispatch]);

  return (
    <div data-testid="left-panel" style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ flex: 1, overflowY: 'auto' }}>
        {children.map((c, i) => {
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
                  navigate({ view: 'tree', factPath: c.fullPath || `${path}/${c.name}` });
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
        {children.length === 0 && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>No items in this path.</div>
        )}
      </div>
    </div>
  );
}

// ---------- ChronoView ----------

function ChronoView({ state, dispatch, navigate }: Props) {
  const [facts, setFacts] = useState<RecentFactEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [selectedIdx, setSelectedIdx] = useState(0);
  const sentinelRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);

  const path = currentPath(state);
  const domains = state.filters.filter(f => f.category === 'domain').map(f => f.value);
  const entities = state.filters.filter(f => f.category === 'entity').map(f => f.value);
  const types = state.filters.filter(f => f.category === 'type').map(f => f.value);
  const eps = state.filters.filter(f => f.category === 'ep').map(f => f.value);
  const typeFilter = types.length === 1 ? types[0] : undefined;

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setFacts([]);
    setTotal(0);
    setSelectedIdx(0);
    api.recent(state.repo, path, state.freeText, 50, 0, {
      typeFilter,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
      eps: eps.length ? eps : undefined,
    }).then(r => {
      if (cancelled) return;
      setFacts(r.facts || []);
      setTotal(r.total);
      setLoading(false);
      if (r.facts?.length > 0) dispatch({ type: 'AMEND_NAV', historyCommit: null, factPath: r.facts[0].path, factCommit: null });
    }).catch(() => { if (!cancelled) { setFacts([]); setLoading(false); } });
    return () => { cancelled = true; };
  }, [path, state.headCommit, state.freeText, state.repo, typeFilter,
      JSON.stringify(domains), JSON.stringify(entities), JSON.stringify(eps), navigate]);

  // Infinite scroll
  const loadMore = useCallback(() => {
    if (loading || facts.length >= total) return;
    setLoading(true);
    api.recent(state.repo, path, state.freeText, 50, facts.length, {
      typeFilter,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
      eps: eps.length ? eps : undefined,
    }).then(r => {
      setFacts(prev => [...prev, ...(r.facts || [])]);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [loading, facts.length, total, state.repo, path, state.freeText, typeFilter,
      JSON.stringify(domains), JSON.stringify(entities), JSON.stringify(eps)]);

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
  const moveSelection = useCallback((delta: 1 | -1) => {
    const next = Math.max(0, Math.min(selectedIdx + delta, facts.length - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const f = facts[next];
    if (f) navigate({ view: 'chrono', factPath: f.path });
  }, [selectedIdx, facts, navigate]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if (state.rightPanelFocused) return;
      if (state.view !== 'chrono') return;

      if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); moveSelection(1); }
      else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); moveSelection(-1); }
      else if (e.key === 'ArrowRight') { e.preventDefault(); dispatch({ type: 'FOCUS_RIGHT_PANEL' }); }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, state.view, moveSelection, dispatch]);

  return (
    <div data-testid="chrono-list" ref={containerRef} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <div style={{ flex: 1, overflowY: 'auto', padding: '4px 0' }}>
        {facts.length === 0 && !loading && (
          <div style={{ padding: 16, color: '#666', fontSize: 13 }}>
            {state.freeText ? 'No facts match the search.' : 'No facts in this path.'}
          </div>
        )}
        {facts.map((f, i) => {
          const ts = (f.type && typeStyles[f.type]) || defaultTypeStyle;
          return (
            <div
              key={f.path}
              data-testid="chrono-item"
              data-path={f.path}
              ref={el => { itemRefs.current[i] = el; }}
              onClick={() => { setSelectedIdx(i); navigate({ view: 'chrono', factPath: f.path }); }}
              style={{
                padding: '6px 12px', cursor: 'pointer',
                background: i === selectedIdx ? '#2a2a3a' : 'transparent',
                borderBottom: '1px solid #1a1a1a',
              }}
            >
              <div style={{ fontSize: 12, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', display: 'flex', alignItems: 'center', gap: 6 }}>
                <span style={{ flexShrink: 0, display: 'flex', alignItems: 'center' }}><TypeIcon type={f.type || ''} color={ts.color} size={12} /></span>
                {f.title}
                {f.type && f.type !== 'observation' && (
                  <span data-testid="chrono-type-badge" style={{
                    color: ts.color, background: ts.bg, fontSize: 9, padding: '1px 5px',
                    borderRadius: 3, fontFamily: 'monospace', flexShrink: 0, letterSpacing: 0.3,
                  }}>{ts.label}</span>
                )}
              </div>
              <div style={{ fontSize: 10, color: '#666', marginTop: 1, display: 'flex', gap: 8 }}>
                <span style={{ fontFamily: 'monospace' }}>{f.path.split('/').pop()}</span>
                <span>{relativeTimeEpoch(f.committed_at)}</span>
              </div>
            </div>
          );
        })}
        <div ref={sentinelRef} style={{ height: 1 }} />
        {loading && (
          <div style={{ padding: 12, color: '#666', fontSize: 12, textAlign: 'center' }}>Loading...</div>
        )}
      </div>
    </div>
  );
}

// ---------- HistoryView ----------

function HistoryView({ state, dispatch, navigate }: Props) {
  return <HistoryTimeline state={state} dispatch={dispatch} navigate={navigate} />;
}

// ---------- LeftPanel (dispatcher) ----------

export function LeftPanel({ state, dispatch, navigate }: Props) {
  switch (state.view) {
    case 'tree': return <TreeView state={state} dispatch={dispatch} navigate={navigate} />;
    case 'chrono': return <ChronoView state={state} dispatch={dispatch} navigate={navigate} />;
    case 'history': return <HistoryView state={state} dispatch={dispatch} navigate={navigate} />;
  }
}
