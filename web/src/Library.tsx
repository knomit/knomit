import { useEffect, useRef, useState, useCallback, useMemo } from 'react';
import { useAsync } from './hooks';
import { EmptyState, LoadingSpinner } from './ui';
import type { Dispatch } from 'react';
import { api } from './api';
import type { DirChild, RecentFactEntry, LensSource } from './api';
import type { AppState, Action } from './state';
import { currentPath, isLive, isLensContext } from './state';
import { typeStyles, defaultTypeStyle, relativeTimeEpoch, repoHue, repoHueBg, repoHueBorder } from './utils';
import { TypeIcon, FolderIcon } from './icons';
import { LibraryHeader } from './LibraryHeader';
import { SourcesDropdown } from './SourcesDropdown';
import type { NavRequest } from './useNavigationManager';

type RowItem = { name: string; fullPath: string; is_dir: boolean };

// LensRow is one row of a lens union list: the RAW canonical path (its
// identity + what fact-open uses), a display title/type, and the source mount.
type LensRow = { path: string; title: string; type?: string; source: LensSource };

// displayLensPath strips the `kb://<id12>/` qualifier from a read-mount path so
// the displayed breadcrumb never shows the opaque mount id — the source badge
// already names the mount. Bare write-repo paths pass through unchanged.
function displayLensPath(path: string): string {
  return path.replace(/^kb:\/\/[^/]+\//, '');
}

// SourceBadge is the one persistent visual difference between a union row and a
// single-repo row: a small mono pill in the mount's deterministic hue.
function SourceBadge({ repo }: { repo: string }) {
  const c = repoHue(repo);
  return (
    <span
      data-testid="source-badge"
      data-repo={repo}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4, fontSize: 10,
        color: c, background: repoHueBg(repo), border: `1px solid ${repoHueBorder(repo)}`,
        borderRadius: 3, padding: '0 5px', fontFamily: 'var(--k-font-mono)', lineHeight: 1.6, flexShrink: 0,
      }}
    >
      <span style={{ width: 5, height: 5, borderRadius: '50%', background: c, flexShrink: 0 }} />
      {repo}
    </span>
  );
}

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
      <span style={{ color: '#e5a23c', fontSize: 10, fontFamily: 'var(--k-font-mono)' }}>
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

  // Lens context reads the union via the lens endpoints instead of the repo
  // ones. `lensName` is the active lens; the repo effects below early-return in
  // lens context so a read never leaks onto a repo endpoint.
  const isLens = isLensContext(state);
  const lensName = state.context.kind === 'lens' ? state.context.name : '';

  const [children, setChildren] = useState<DirChild[]>([]);
  const [selectedIdx, setSelectedIdx] = useState(-1);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);

  // ── Path sort: api.browse for directory entries ──
  useAsync((stale) => {
    if (isLens) return; // lens context reads via the lens effect below
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
  }, [path, state.headCommit, effectiveSort, state.repo, state.branch, state.ontologyRoot, state.factPath, isLens]);

  // ── Recent sort: api.recent for chrono entries (infinite-scroll paged) ──
  const [facts, setFacts] = useState<RecentFactEntry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const sentinelRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  // Stale ref for use inside the async useAsync callback (state updates between
  // dispatch and resolution would otherwise read closed-over stale values).
  const staleStateRef = useRef(state);
  staleStateRef.current = state;

  const { domains, entities, types, kinds, origins, eps } = useMemo(() => {
    const domains: string[] = [], entities: string[] = [], types: string[] = [], kinds: string[] = [], origins: string[] = [], eps: string[] = [];
    for (const f of state.filters) {
      if (f.category === 'domain') domains.push(f.value);
      else if (f.category === 'entity') entities.push(f.value);
      else if (f.category === 'type') types.push(f.value);
      else if (f.category === 'kind') kinds.push(f.value);
      else if (f.category === 'origin') origins.push(f.value);
      else if (f.category === 'ep') eps.push(f.value);
    }
    return { domains, entities, types, kinds, origins, eps };
  }, [state.filters]);
  const typeFilter = types.length === 1 ? types[0] : undefined;
  const filtersKey = state.filters.map(f => `${f.category}:${f.value}`).join('\0');

  useAsync((stale) => {
    if (isLens) return; // lens context reads via the lens effect below
    if (effectiveSort !== 'recent') return;
    setLoading(true);
    setFacts([]);
    setTotal(0);
    api.recent(state.repo, state.branch, path, state.freeText, 50, 0, {
      typeFilter,
      kinds: kinds.length ? kinds : undefined,
      origins: origins.length ? origins : undefined,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
      eps: eps.length ? eps : undefined,
    }).then(r => {
      if (stale()) return;
      setFacts(r.facts || []);
      setTotal(r.total);
      setLoading(false);
      const loaded = r.facts || [];
      const alreadyInList = loaded.some(f => f.path === staleStateRef.current.factPath);
      if (loaded.length > 0 && !alreadyInList) {
        dispatch({ type: 'AMEND_NAV', factPath: loaded[0].path });
      }
    }).catch(() => { if (!stale()) { setFacts([]); setLoading(false); } });
  }, [path, state.headCommit, state.freeText, state.repo, state.branch, typeFilter, filtersKey, effectiveSort, isLens]);

  // Recent mode highlights by index only (path/relevance sync inside their
  // fetch). Keep the highlighted row tied to the open fact so any factPath
  // change — notably returning to live from history — re-selects its row
  // instead of leaving the list unhighlighted.
  useEffect(() => {
    if (effectiveSort !== 'recent') return;
    if (!state.factPath) { setSelectedIdx(-1); return; }
    const idx = facts.findIndex(f => f.path === state.factPath);
    if (idx >= 0) setSelectedIdx(idx);
  }, [state.factPath, facts, effectiveSort]);

  // Infinite scroll: when the sentinel at the bottom of the Recent list scrolls
  // into view, fetch the next page and append. loadingRef keeps the callback
  // identity stable so the IntersectionObserver doesn't reconnect on every
  // loading-state flip (which would re-fire the trigger and double-load).
  const loadingRef = useRef(loading);
  loadingRef.current = loading;
  const loadMore = useCallback(() => {
    if (isLens) return; // lens list isn't paged here
    if (effectiveSort !== 'recent') return;
    if (loadingRef.current || facts.length >= total) return;
    setLoading(true);
    api.recent(state.repo, state.branch, path, state.freeText, 50, facts.length, {
      typeFilter,
      kinds: kinds.length ? kinds : undefined,
      origins: origins.length ? origins : undefined,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
      eps: eps.length ? eps : undefined,
    }).then(r => {
      setFacts(prev => [...prev, ...(r.facts || [])]);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [isLens, effectiveSort, facts.length, total, state.repo, state.branch, path, state.freeText, typeFilter, kinds, origins, domains, entities, eps]);

  useEffect(() => {
    if (effectiveSort !== 'recent') return;
    const sentinel = sentinelRef.current;
    const root = containerRef.current;
    if (!sentinel || !root) return;
    const observer = new IntersectionObserver(
      ([entry]) => { if (entry.isIntersecting) loadMore(); },
      { root, threshold: 0.1 }
    );
    observer.observe(sentinel);
    return () => observer.disconnect();
  }, [effectiveSort, loadMore]);

  // ── Relevance sort: api.search for free-text results ──
  useAsync((stale) => {
    if (isLens) return; // lens context reads via the lens effect below
    if (effectiveSort !== 'relevance') {
      dispatch({ type: 'SET_SEARCHING', value: false });
      return;
    }
    dispatch({ type: 'SET_SEARCHING', value: true });
    api.search(state.repo, state.branch, state.freeText, path, 0, {
      types: types.length ? types : undefined,
      kinds: kinds.length ? kinds : undefined,
      origins: origins.length ? origins : undefined,
      eps: eps.length ? eps : undefined,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
    }).then(r => {
      if (stale()) return;
      dispatch({ type: 'SET_SEARCHING', value: false });
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
    }).catch(() => { if (!stale()) { setChildren([]); dispatch({ type: 'SET_SEARCHING', value: false }); } });
  }, [path, state.headCommit, state.freeText, effectiveSort, state.repo, state.branch, filtersKey, isLens]);

  // ── Lens union list: api.listLensFacts (recent/path) or api.lensSearch
  // (relevance). `lensSources` narrows the fan-out: null = all mounts (no repos
  // param), an explicit subset re-sends repos=[…] and refetches, and [] means
  // "none selected" → an empty list (NOT a fetch, since an empty repos array
  // would otherwise read as "all" server-side). ──
  const [lensRows, setLensRows] = useState<LensRow[]>([]);
  const [lensLoading, setLensLoading] = useState(false);
  const lensSources = state.lensSources;
  const noneSelected = Array.isArray(lensSources) && lensSources.length === 0;
  // Stable dep key so the effect refetches when the selection changes.
  const lensSourcesKey = lensSources === null ? ' ALL' : lensSources.join('');

  useAsync((stale) => {
    if (!isLens) return;
    if (noneSelected) { setLensRows([]); setLensLoading(false); return; }
    const repos = lensSources ?? undefined;
    setLensLoading(true);
    setLensRows([]);
    if (effectiveSort === 'relevance') {
      dispatch({ type: 'SET_SEARCHING', value: true });
      api.lensSearch(lensName, state.freeText, repos).then(results => {
        if (stale()) return;
        dispatch({ type: 'SET_SEARCHING', value: false });
        setLensRows(results.map(r => ({ path: r.path, title: r.title, type: r.type, source: r.source })));
        setLensLoading(false);
      }).catch(() => { if (!stale()) { setLensRows([]); setLensLoading(false); dispatch({ type: 'SET_SEARCHING', value: false }); } });
      return;
    }
    dispatch({ type: 'SET_SEARCHING', value: false });
    api.listLensFacts(lensName, { path, query: state.freeText || undefined, limit: 50, offset: 0, repos }).then(r => {
      if (stale()) return;
      setLensRows((r.facts || []).map(f => ({ path: f.path, title: f.title, type: f.type, source: f.source })));
      setLensLoading(false);
    }).catch(() => { if (!stale()) { setLensRows([]); setLensLoading(false); } });
  }, [isLens, lensName, path, state.freeText, effectiveSort, lensSourcesKey, noneSelected, state.headCommit]);

  // Keep the highlighted lens row tied to the open fact (mirrors the repo
  // Recent behavior) so returning to a fact re-selects its row.
  useEffect(() => {
    if (!isLens) return;
    if (!state.factPath) { setSelectedIdx(-1); return; }
    const idx = lensRows.findIndex(r => r.path === state.factPath);
    if (idx >= 0) setSelectedIdx(idx);
  }, [isLens, state.factPath, lensRows]);

  // openFact is the single fact-open chokepoint. In a repo context it just
  // navigates (unchanged). In a lens context it additionally reads the fact
  // through the lens with its RAW canonical path to learn the source mount and
  // stores it via SET_FACT_SOURCE, then navigates so RightPanel renders it.
  const openFact = useCallback((fullPath: string) => {
    if (isLens) {
      api.getLensFact(lensName, fullPath)
        .then(f => dispatch({ type: 'SET_FACT_SOURCE', source: f.source }))
        .catch(() => { /* fact-open surfaces its own error via RightPanel */ });
    }
    navigate({ view: 'library', factPath: fullPath });
  }, [isLens, lensName, dispatch, navigate]);

  const activeList: RowItem[] = useMemo(() => {
    if (isLens) {
      return lensRows.map(r => ({ name: displayLensPath(r.path), fullPath: r.path, is_dir: false }));
    }
    if (effectiveSort === 'recent') {
      return facts.map(f => ({ name: f.path.split('/').pop() || f.path, fullPath: f.path, is_dir: false }));
    }
    return children.map(c => ({ name: c.name, fullPath: c.fullPath || '', is_dir: c.is_dir }));
  }, [isLens, lensRows, effectiveSort, facts, children]);

  const moveSelection = useCallback((delta: 1 | -1) => {
    const len = activeList.length;
    if (len === 0) return;
    const next = Math.max(0, Math.min(selectedIdx + delta, len - 1));
    setSelectedIdx(next);
    itemRefs.current[next]?.scrollIntoView({ block: 'nearest' });
    const item = activeList[next];
    if (item && !item.is_dir) {
      openFact(item.fullPath || `${path}/${item.name}`);
    }
  }, [activeList, selectedIdx, path, openFact]);

  const activateSelected = useCallback(() => {
    const item = activeList[selectedIdx];
    if (!item) return;
    if (item.is_dir) {
      dispatch({ type: 'NAVIGATE', path: `${path}/${item.name}` });
    } else {
      openFact(item.fullPath || `${path}/${item.name}`);
    }
  }, [activeList, selectedIdx, path, dispatch, openFact]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
      if (state.rightPanelFocused) return;
      // In history mode the Library is hidden behind TimelineNav but stays
      // mounted, so this global listener is still live. Ignore keys then —
      // otherwise arrows/Enter drive the hidden selection and can navigate
      // away from the read-only history view.
      if (!isLive(state)) return;

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
        const item = activeList[selectedIdx];
        if (!item) return;
        if (item.is_dir) activateSelected();
        else dispatch({ type: 'FOCUS_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, state.asOf.mode, moveSelection, activateSelected, activeList, selectedIdx, path, dispatch]);

  const hasPathChip = state.filters.some(f => f.category === 'path');

  return (
    <div data-testid="left-panel" data-sort={effectiveSort} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      {isLens && state.lens && (
        <SourcesDropdown lens={state.lens} selection={state.lensSources} dispatch={dispatch} />
      )}
      <LibraryHeader
        count={isLens ? lensRows.length : effectiveSort === 'recent' ? facts.length : children.length}
        scoped={hasPathChip}
        sort={effectiveSort}
        searchActive={searchActive}
        onSortChange={(sort) => dispatch({ type: 'SET_LIBRARY_SORT', sort })}
      />
      {!isLive(state) && (
        <ReadOnlyBanner message="Showing live library · history views not yet supported by backend" />
      )}
      <div ref={containerRef} style={{ flex: 1, overflowY: 'auto' }}>
        {isLens && (
          <>
            {lensRows.length === 0 && !lensLoading && (
              <EmptyState message={
                noneSelected ? 'No sources selected.'
                  : state.freeText ? 'No facts match the search.'
                  : 'No facts in this lens.'
              } />
            )}
            {lensRows.map((f, i) => {
              const ts = (f.type && typeStyles[f.type]) || defaultTypeStyle;
              return (
                <div
                  key={f.path}
                  data-testid="lens-item"
                  data-path={f.path}
                  ref={el => { itemRefs.current[i] = el; }}
                  onClick={() => { setSelectedIdx(i); openFact(f.path); }}
                  style={{
                    padding: '6px 12px', cursor: 'pointer',
                    background: i === selectedIdx ? '#2a2a3a' : 'transparent',
                    borderBottom: '1px solid #1a1a1a',
                    borderLeft: `2px solid ${i === selectedIdx ? repoHue(f.source.repo) : 'transparent'}`,
                  }}
                >
                  <div style={{ fontSize: 12.5, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ flexShrink: 0, display: 'flex', alignItems: 'center' }}><TypeIcon type={f.type || ''} color={ts.color} size={12} /></span>
                    {f.title}
                  </div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 3, paddingLeft: 18 }}>
                    <SourceBadge repo={f.source.repo} />
                    <span data-testid="lens-item-path" style={{ fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#666', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{displayLensPath(f.path)}</span>
                  </div>
                </div>
              );
            })}
            {lensLoading && <LoadingSpinner />}
          </>
        )}
        {!isLens && (effectiveSort === 'path' || effectiveSort === 'relevance') && children.map((c, i) => {
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
                  openFact(c.fullPath || `${path}/${c.name}`);
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
        {!isLens && (effectiveSort === 'path' || effectiveSort === 'relevance') && children.length === 0 && <EmptyState message={effectiveSort === 'relevance' ? 'No facts match the search.' : 'No items in this path.'} />}
        {!isLens && effectiveSort === 'recent' && (
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
                  onClick={() => { setSelectedIdx(i); openFact(f.path); }}
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
                    <span style={{ fontFamily: 'var(--k-font-mono)' }}>{f.path.split('/').pop()}</span>
                    <span>{relativeTimeEpoch(f.committed_at)}</span>
                  </div>
                </div>
              );
            })}
            {/* Infinite-scroll sentinel — IntersectionObserver fires loadMore
                when this scrolls into view. Only meaningful when more pages
                exist; otherwise stays parked at the bottom inert. */}
            <div ref={sentinelRef} data-testid="recent-sentinel" style={{ height: 1 }} />
            {loading && <LoadingSpinner />}
          </>
        )}
      </div>
    </div>
  );
}
