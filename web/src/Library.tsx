import { memo, useEffect, useRef, useState, useCallback, useMemo } from 'react';
import { useAsync } from './hooks';
import { EmptyState, LoadingSpinner } from './ui';
import type { Dispatch } from 'react';
import { api } from './api';
import type { DirChild, LensDirChild, RecentFactEntry, LensSource } from './api';
import type { AppState, Action } from './state';
import { currentPath, isLive, isLensContext, canGoBack } from './state';
import { typeStyles, defaultTypeStyle, relativeTimeEpoch, repoHue, displayLensPath, rowPath } from './utils';
import { TypeIcon, FolderIcon } from './icons';
import { LibraryHeader } from './LibraryHeader';
import { useMotifClusters } from './useMotifClusters';
import { factSubject, subjectSummary } from './motifSubject';
import type { NavRequest } from './useNavigationManager';

type RowItem = { name: string; fullPath: string; is_dir: boolean };

// Rows per page for the paged lists. Named because the union's terminator
// compares against it: a page that comes back SHORTER than it asked for is how
// the client learns the list is finished — see `lensExhausted`.
const PAGE_SIZE = 50;

// LensRow is one row of a lens union list: the RAW canonical path (its
// identity + what fact-open uses), a display title/type, and the source mount.
type LensRow = { path: string; title: string; type?: string; source: LensSource };

// SourceBadge is the one persistent visual difference between a union row and a
// single-repo row: a dot in the mount's deterministic hue and its plain name.
//
// No pill. The bordered, filled badge was the last of that treatment left in the
// app — the summary panel's Repo rows and the fact band both draw a mount as a
// dot plus a mono name, and one concept should not have two looks. It also cost
// width: on a narrow panel the padding and border were characters the title
// could have had.
function SourceBadge({ repo }: { repo: string }) {
  const c = repoHue(repo);
  return (
    <span
      data-testid="source-badge"
      data-repo={repo}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 5, fontSize: 10,
        color: '#8b95a6', fontFamily: 'var(--k-font-mono)', lineHeight: 1.6,
        minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
      }}
    >
      <span style={{ width: 5, height: 5, borderRadius: '50%', background: c, flexShrink: 0 }} />
      {repo}
    </span>
  );
}

// ── Row components ────────────────────────────────────────────────────────────
// The four lists below used to be inline .map blocks, so every row re-rendered
// (and re-allocated its 4–6 inline style objects) on any Library render — a
// selection move, a paged append, a parent state change. Extracted + memoized,
// a selection change now re-renders exactly the two rows whose `selected` flag
// moved. Callbacks are passed as stable identities from Library so the memo is
// not inert; the per-row closures below live INSIDE the memo boundary, so they
// are only rebuilt when the row itself actually re-renders.

interface EntryRowProps {
  testId: string;
  index: number;
  selected: boolean;
  name: string;
  title?: string;
  type?: string;
  isDir: boolean;
  /** Canonical path for this row ('' for a directory). Also the data-path attr. */
  path: string;
  /** Current directory, used to synthesize a path when the row carries none,
   *  and to trim the part of it the breadcrumb above already shows. */
  dirPath: string;
  /** Ontology root — the depth at which the header stops naming a location. */
  ontologyRoot: string;
  /** Lens union only — renders the mount badge. Undefined in repo context. */
  sourceRepo?: string;
  /** Lens tree titles truncate to one line; the repo dir list does not. */
  truncateTitle: boolean;
  onSelect: (index: number) => void;
  onEnterDir: (name: string) => void;
  onOpenFact: (fullPath: string) => void;
  registerRef: (index: number, el: HTMLDivElement | null) => void;
}

// EntryRow serves BOTH tree lists — the lens union tree and the repo directory
// browse. They were near-identical blocks; the only real differences are the
// test id, the source badge (lens only), and whether the title truncates.
const EntryRow = memo(function EntryRow({
  testId, index, selected, name, title, type, isDir, path, dirPath,
  ontologyRoot, sourceRepo, truncateTitle, onSelect, onEnterDir, onOpenFact, registerRef,
}: EntryRowProps) {
  const ts = (type && typeStyles[type]) || defaultTypeStyle;
  const sub = isDir ? '' : rowPath(path || `${dirPath}/${name}`, dirPath, ontologyRoot);
  const setRef = useCallback((el: HTMLDivElement | null) => registerRef(index, el), [registerRef, index]);
  const onClick = useCallback(() => {
    onSelect(index);
    if (isDir) onEnterDir(name);
    else onOpenFact(path || `${dirPath}/${name}`);
  }, [onSelect, index, isDir, onEnterDir, name, onOpenFact, path, dirPath]);

  // Hover yields to selection: repainting the selected row on hover would make
  // it look unselected the moment you reached for it.
  return (
    <div
      data-testid={testId}
      data-name={name}
      data-isdir={String(isDir)}
      data-path={path}
      ref={setRef}
      onClick={onClick}
      onMouseEnter={selected ? undefined : e => { e.currentTarget.style.background = ENTRY_HOVER; }}
      onMouseLeave={selected ? undefined : e => { e.currentTarget.style.background = 'transparent'; }}
      style={selected ? entryRowSelected : entryRow}
    >
      {isDir ? (
        <span style={entryIconDir}>
          <FolderIcon color="#7c9" size={12} />
        </span>
      ) : (
        <span data-testid="fact-type-icon" style={entryIcon}>
          <TypeIcon type={type || ''} color={ts.color} size={12} />
        </span>
      )}
      {/* Title above, mount below. Side by side the badge would not shrink, so
          the title absorbed the whole truncation and rows read "Agent
          failure…" beside a full-width mount name. The flat lens list already
          stacks them for the same reason. A folder, and any row with no mount,
          stays one line. */}
      <span style={{ display: 'flex', flexDirection: 'column', gap: 1, flex: 1, minWidth: 0 }}>
        <span style={truncateTitle ? entryTitleTruncated : entryTitle}>{title || name}</span>
        {/* The same meta line the flat union row carries, so a fact looks like
            a fact in both views. `sub` is the part of the path the breadcrumb
            above is not already showing — inside a directory that is the
            basename, and at the root it is the whole path. A directory row
            says nothing: the name IS its location. */}
        {!isDir && (sourceRepo || sub) && (
          <span style={entryMetaLine}>
            {sourceRepo && <SourceBadge repo={sourceRepo} />}
            {sub && <span data-testid="entry-path" style={lensPath}>{sub}</span>}
          </span>
        )}
      </span>
    </div>
  );
});

// NO separator between rows. A hairline under each one turned a list of sixteen
// topics into sixteen boxes — a grid, when the panel should read as a single
// surface. The row's own padding is what keeps them apart now.
//
// Which means hover has to do the work the border was accidentally doing: with
// neither, there is nothing to say which row you are about to open. It is the
// quietest fill that still reads (#191922 against the #141414 panel), and it
// yields to selection — see EntryRow.
const entryRowBase: React.CSSProperties = {
  padding: '8px 12px', cursor: 'pointer',
  display: 'flex', alignItems: 'center', gap: 8,
};
const entryRow: React.CSSProperties = { ...entryRowBase, background: 'transparent' };
const entryRowSelected: React.CSSProperties = { ...entryRowBase, background: '#2a2a3a' };
const ENTRY_HOVER = '#191922';
const entryIcon: React.CSSProperties = { flexShrink: 0, display: 'flex', alignItems: 'center' };
const entryIconDir: React.CSSProperties = { ...entryIcon, opacity: 0.7 };
const entryTitle: React.CSSProperties = { fontSize: 13, color: '#ddd' };
const entryTitleTruncated: React.CSSProperties = { ...entryTitle, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 };

// LensFactRow is a flat lens-union row: title line + mount badge + the
// id12-stripped display path.
const LensFactRow = memo(function LensFactRow({
  row, index, selected, dirPath, ontologyRoot, onSelect, onOpenFact, registerRef,
}: {
  row: LensRow;
  index: number;
  selected: boolean;
  dirPath: string;
  ontologyRoot: string;
  onSelect: (index: number) => void;
  onOpenFact: (fullPath: string) => void;
  registerRef: (index: number, el: HTMLDivElement | null) => void;
}) {
  const ts = (row.type && typeStyles[row.type]) || defaultTypeStyle;
  const setRef = useCallback((el: HTMLDivElement | null) => registerRef(index, el), [registerRef, index]);
  const onClick = useCallback(() => { onSelect(index); onOpenFact(row.path); }, [onSelect, index, onOpenFact, row.path]);

  return (
    <div
      data-testid="lens-item"
      data-path={row.path}
      ref={setRef}
      onClick={onClick}
      style={{
        ...(selected ? factRowSelected : factRow),
        borderLeft: `2px solid ${selected ? repoHue(row.source.repo) : 'transparent'}`,
      }}
    >
      <div style={factTitleLine}>
        <span style={entryIcon}><TypeIcon type={row.type || ''} color={ts.color} size={12} /></span>
        {row.title}
      </div>
      <div style={lensMetaLine}>
        <SourceBadge repo={row.source.repo} />
        <span data-testid="lens-item-path" style={lensPath}>{rowPath(row.path, dirPath, ontologyRoot)}</span>
      </div>
    </div>
  );
});

// ChronoRow is a repo Recent row: title line + basename + relative commit time.
const ChronoRow = memo(function ChronoRow({
  fact, index, selected, subject, onSelect, onOpenFact,
}: {
  fact: RecentFactEntry;
  index: number;
  selected: boolean;
  /** The fact's subject, shown only on a motif pivot — elsewhere every row in
   *  the list shares one, so printing it would be the same word N times. */
  subject?: string;
  onSelect: (index: number) => void;
  onOpenFact: (fullPath: string) => void;
}) {
  const ts = (fact.type && typeStyles[fact.type]) || defaultTypeStyle;
  const onClick = useCallback(() => { onSelect(index); onOpenFact(fact.path); }, [onSelect, index, onOpenFact, fact.path]);

  return (
    <div
      data-testid="chrono-item"
      data-path={fact.path}
      onClick={onClick}
      style={selected ? factRowSelected : factRow}
    >
      <div style={chronoTitleLine}>
        <span style={entryIcon}><TypeIcon type={fact.type || ''} color={ts.color} size={12} /></span>
        {fact.title}
      </div>
      <div style={chronoMetaLine}>
        {/* On a pivot, what this fact is ABOUT — path segment 3. The whole
            point of the list is how unlike each other these subjects are, and
            saying it per row is what makes that readable as you scan rather
            than only stated in the heading. */}
        {subject && <><span data-testid="chrono-subject" style={chronoSubject}>{subject}</span><span style={{ color: '#2f3540' }}>·</span></>}
        <span style={chronoName}>{fact.path.split('/').pop()}</span>
        <span>{relativeTimeEpoch(fact.committed_at)}</span>
      </div>
    </div>
  );
});

const factRowBase: React.CSSProperties = { padding: '6px 12px', cursor: 'pointer', borderBottom: '1px solid #1a1a1a' };
const factRow: React.CSSProperties = { ...factRowBase, background: 'transparent' };
const factRowSelected: React.CSSProperties = { ...factRowBase, background: '#2a2a3a' };
const factTitleLine: React.CSSProperties = { fontSize: 12.5, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', display: 'flex', alignItems: 'center', gap: 6 };
const chronoTitleLine: React.CSSProperties = { ...factTitleLine, fontSize: 12 };
const entryMetaLine: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 };
const lensMetaLine: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 6, marginTop: 3, paddingLeft: 18 };
const lensPath: React.CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#666', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const chronoMetaLine: React.CSSProperties = { fontSize: 10, color: '#666', marginTop: 1, display: 'flex', gap: 8 };
const chronoName: React.CSSProperties = { fontFamily: 'var(--k-font-mono)' };
const chronoSubject: React.CSSProperties = { fontFamily: 'var(--k-font-mono)', color: '#7f8b9c' };

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
  /** Library column is below the width where the root ancestor still fits. */
  narrow?: boolean;
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

export function Library({ state, dispatch, navigate, narrow = false }: Props) {
  const path = currentPath(state);
  // Every chip except `path` is a content filter, and a content filter ranks:
  // it flips the list into relevance mode. `path` is a location, so it does not.
  // (Mount scope is state.lensSources, which likewise must never flip the
  // ranking — narrowing WHICH repos are read is not asking for a ranking.)
  // Relevance means "ordered by how well each fact matches", so it needs
  // something to match AGAINST. Free text is that; a chip is not.
  //
  // This used to be `hasNonPathFilters || freeText`, which made any content chip
  // a search — so clicking "ai · 666" on the dashboard left the paged facts
  // endpoint for the relevance one: retrieval-capped, unpaged here (its
  // infinite-scroll sentinel is disabled for relevance), and reporting the size
  // of the candidate set it fetched. The reader clicked 666 and got 50 rows
  // that would not scroll.
  //
  // With no text there is no ranking to impose, so the chip is just a filter:
  // the sort stays whatever the reader had, and the facts endpoint — which
  // already forwards every content filter, and pages against a real COUNT(*) —
  // answers it.
  const searchActive = !!state.freeText;
  // A chip cannot be honoured by the tree: the ontology browse is a directory
  // walk and the topics endpoint takes no content filters, so a chip applied in
  // Path mode is silently ignored — which is exactly what clicking a dashboard
  // facet looked like, a chip in the bar above an unchanged list of topics. A
  // filter needs a flat list to be a filter OF, so it borrows Recent. Nothing
  // is written to librarySort, so removing the chip returns to the tree.
  const hasContentFilters = state.filters.some(f => f.category !== 'path');
  const effectiveSort = searchActive ? 'relevance'
    : hasContentFilters && state.librarySort === 'path' ? 'recent'
    : state.librarySort;

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
  // Scroll memory, keyed by location. Returning to a long folder at the top
  // instead of at the row you left from is the difference between the header's
  // back button being usable and being a reset.
  //
  // A REF, not nav-stack state: the reducer is pure and cannot read scrollTop,
  // so recording it in a NavEntry would mean threading a DOM measurement
  // through every navigating dispatch. Keying by path instead of by history
  // entry also means arriving from anywhere restores the same position, which
  // is what "where I was in this folder" means to a reader.
  //
  // Deliberately not persisted: it is a within-session convenience, and a
  // remembered offset into a list whose contents have since changed is worse
  // than the top.
  const scrollMemory = useRef<Map<string, number>>(new Map());
  // Stale ref for use inside the async useAsync callback (state updates between
  // dispatch and resolution would otherwise read closed-over stale values).
  const staleStateRef = useRef(state);
  staleStateRef.current = state;

  const { domains, entities, types, kinds, origins, eps, motifs } = useMemo(() => {
    const domains: string[] = [], entities: string[] = [], types: string[] = [], kinds: string[] = [], origins: string[] = [], eps: string[] = [], motifs: string[] = [];
    for (const f of state.filters) {
      if (f.category === 'domain') domains.push(f.value);
      else if (f.category === 'entity') entities.push(f.value);
      else if (f.category === 'type') types.push(f.value);
      else if (f.category === 'kind') kinds.push(f.value);
      else if (f.category === 'origin') origins.push(f.value);
      else if (f.category === 'ep') eps.push(f.value);
      else if (f.category === 'motif') motifs.push(f.value);
    }
    return { domains, entities, types, kinds, origins, eps, motifs };
  }, [state.filters]);
  // Motif chips ride filtersKey like every other chip. There is no match tier
  // to carry alongside them: the client asks for the motif itself and nothing
  // looser, so `exact` is the server's default and we send no override.
  const motifOpts = useMemo(() => (motifs.length ? { motifs } : {}), [motifs]);
  const filtersKey = state.filters.map(f => `${f.category}:${f.value}`).join('\0');

  // THE PIVOT HEADING. One motif chip and no free text means this list IS that
  // motif's carriers, so the header stops reporting a location — a motif cuts
  // across the ontology and there is no folder to be in. Two chips is a union
  // of two shapes and has no single name, so it keeps the ordinary header.
  const pivotMotif = (motifs.length === 1 && !state.freeText) ? motifs[0] : null;
  const pivotResolved = useMotifClusters(state.repo, state.branch, pivotMotif ? [pivotMotif] : undefined);
  const pivotCluster = pivotResolved[0]?.cluster;

  useAsync((stale) => {
    if (isLens) return; // lens context reads via the lens effect below
    if (effectiveSort !== 'recent') return;
    setLoading(true);
    setFacts([]);
    setTotal(0);
    api.recent(state.repo, state.branch, path, state.freeText, PAGE_SIZE, 0, {
      types: types.length ? types : undefined,
      kinds: kinds.length ? kinds : undefined,
      origins: origins.length ? origins : undefined,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
      eps: eps.length ? eps : undefined,
      ...motifOpts,
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
  }, [path, state.headCommit, state.freeText, state.repo, state.branch, filtersKey, effectiveSort, isLens]);

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
      ...motifOpts,
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
  const [lensTree, setLensTree] = useState<LensDirChild[]>([]);
  const [lensLoading, setLensLoading] = useState(false);
  /**
   * Whether the union has handed over everything it has.
   *
   * `total` cannot answer this on its own. When any mount was truncated the
   * server reports the summed per-mount COUNT(*) — a deliberate UPPER BOUND,
   * off by exactly the cross-mount path collisions (see the note at the
   * count-vs-transfer invariant: bounding transfer must never bound the count,
   * so the number stays honest about matching rows and is simply not a page
   * ledger). Used as a terminator it never converges: with a fork beside its
   * upstream, every real row can be loaded while `lensRows.length >= total`
   * stays false, so the sentinel keeps asking for pages the server answers with
   * an empty slice, forever.
   *
   * A SHORT PAGE is the signal that does converge. Fewer rows than the limit
   * asked for means the union had no more to give at that offset, whatever the
   * count says.
   */
  const [lensExhausted, setLensExhausted] = useState(false);
  // Generation token for the lens union. The primary union effect bumps it on
  // every scope change (repos/path/query/sort/…); a paged loadMore captures it
  // at call time and drops its response if the token has moved on, so a fetch
  // in flight when the scope narrows can never append stale-scope rows onto the
  // fresh page-1 list the effect just set.
  const lensGenRef = useRef(0);
  const lensSources = state.lensSources;
  const noneSelected = Array.isArray(lensSources) && lensSources.length === 0;

  // The `repos` param comes from ONE narrowing: the sources selection
  // (state.lensSources; null = all mounts), which both the sources dropdown and
  // the summary's Repos rows drive. It used to be the intersection of that and a
  // set of repo: filter chips — a second control over the same scope, which
  // could disagree with the dropdown describing it. The chip facet is gone.
  // Mount names come from the lens's read set (which carries the write repo as
  // a self-mount).
  const allMounts = useMemo(() => (state.lens ? state.lens.reads.map(r => r.name) : []), [state.lens]);
  const sourcesSel = lensSources ?? allMounts;
  // Filtered through allMounts to preserve mount order and to drop any name
  // that is not a real mount (an unknown mount would 422 server-side).
  const effectiveRepos = allMounts.filter(m => sourcesSel.includes(m));
  // Send a repos param only when the selection narrows something; otherwise omit
  // it so the server fans out to every mount (the null-selection contract).
  const constrained = lensSources !== null;
  const reposParam = constrained ? effectiveRepos : undefined;
  // An empty scope — nothing selected, or a selection naming no real mount — is
  // a valid "nothing to show" state, never a fetch (an empty repos array would
  // otherwise read as "all mounts" server-side).
  const emptyScope = noneSelected || (constrained && effectiveRepos.length === 0);
  // Stable dep key so the effect refetches when either narrowing changes.
  const reposKey = reposParam === undefined ? ' ALL' : reposParam.join('\0');

  useAsync((stale) => {
    if (!isLens) return;
    // A fresh scope invalidates any paged loadMore still in flight.
    lensGenRef.current += 1;
    if (emptyScope) { setLensRows([]); setLensTree([]); setTotal(0); setLensExhausted(true); setLensLoading(false); return; }
    const repos = reposParam;
    // Landing on a result is half of what picking a facet means: the chip
    // narrows the list, the first row opens. The repo list has always done this
    // (api.recent and api.search both AMEND_NAV); the union did not, so a domain
    // pick in a lens filtered the left panel and left the right panel sitting on
    // the folder dashboard — the one visible difference between the two contexts.
    //
    // Row 0 only when the open fact did NOT survive the refetch: re-selecting on
    // every fetch would yank the reader off the fact they are reading each time
    // an unrelated chip moved. AMEND_NAV, not navigate(), so the facet click
    // stays ONE back-stack entry — same reason the repo branches use it.
    //
    // Not the path branch: browsing a tree is not searching, and a folder that
    // opened its first fact on arrival would fight the reader walking the tree.
    const openFirstRow = (rows: LensRow[]) => {
      if (rows.length === 0) return;
      const open = staleStateRef.current.factPath;
      if (rows.some(r => r.path === open)) return;
      dispatch({ type: 'AMEND_NAV', factPath: rows[0].path });
    };
    setLensLoading(true);
    setLensRows([]);
    setLensTree([]);
    setTotal(0);
    // A new scope is unpaged until its first page says otherwise.
    setLensExhausted(false);
    if (effectiveSort === 'relevance') {
      dispatch({ type: 'SET_SEARCHING', value: true });
      // The lens search handler accepts the full content-filter set the repo
      // /search does (the facts handler does not) — forward path scope + chips.
      api.lensSearch(lensName, state.freeText, repos, {
        path,
        types: types.length ? types : undefined,
        kinds: kinds.length ? kinds : undefined,
        origins: origins.length ? origins : undefined,
        eps: eps.length ? eps : undefined,
        domains: domains.length ? domains : undefined,
        entities: entities.length ? entities : undefined,
        ...motifOpts,
      }).then(results => {
        if (stale()) return;
        dispatch({ type: 'SET_SEARCHING', value: false });
        const rows = results.map(r => ({ path: r.path, title: r.title, type: r.type, source: r.source }));
        setLensRows(rows);
        setLensLoading(false);
        openFirstRow(rows);
      }).catch(() => { if (!stale()) { setLensRows([]); setLensLoading(false); dispatch({ type: 'SET_SEARCHING', value: false }); } });
      return;
    }
    if (effectiveSort === 'path') {
      // Unified tree browse: ONE lazy level per call (the lens twin of
      // api.browse), honoring the same repos narrowing as the flat union.
      // Not paged — no sentinel, no offset.
      dispatch({ type: 'SET_SEARCHING', value: false });
      api.lensBrowse(lensName, path, state.ontologyRoot, repos).then(r => {
        if (stale()) return;
        setLensTree(r.children || []);
        setLensLoading(false);
      }).catch(() => { if (!stale()) { setLensTree([]); setLensLoading(false); } });
      return;
    }
    dispatch({ type: 'SET_SEARCHING', value: false });
    api.listLensFacts(lensName, {
      path, query: state.freeText || undefined, limit: PAGE_SIZE, offset: 0, repos,
        types: types.length ? types : undefined,
        kinds: kinds.length ? kinds : undefined,
        origins: origins.length ? origins : undefined,
        eps: eps.length ? eps : undefined,
        domains: domains.length ? domains : undefined,
        entities: entities.length ? entities : undefined,
        ...motifOpts,
    }).then(r => {
      if (stale()) return;
      const rows = (r.facts || []).map(f => ({ path: f.path, title: f.title, type: f.type, source: f.source }));
      setLensRows(rows);
      // Keep total so the header can report what MATCHES (I5). It is not the
      // paging terminator — see `lensExhausted` for why a short page is.
      setTotal(r.total);
      setLensExhausted(rows.length < PAGE_SIZE);
      setLensLoading(false);
      openFirstRow(rows);
    }).catch(() => { if (!stale()) { setLensRows([]); setLensLoading(false); } });
  }, [isLens, lensName, path, state.freeText, effectiveSort, reposKey, emptyScope, filtersKey, state.headCommit, state.ontologyRoot]);

  // Keep the highlighted lens row tied to the open fact (mirrors the repo
  // Recent behavior) so returning to a fact re-selects its row.
  useEffect(() => {
    if (!isLens) return;
    if (!state.factPath) { setSelectedIdx(-1); return; }
    if (effectiveSort === 'path') {
      const idx = lensTree.findIndex(c => !c.is_dir && c.path === state.factPath);
      if (idx >= 0) setSelectedIdx(idx);
      return;
    }
    const idx = lensRows.findIndex(r => r.path === state.factPath);
    if (idx >= 0) setSelectedIdx(idx);
  }, [isLens, state.factPath, lensRows, lensTree, effectiveSort]);

  // Infinite scroll: when the sentinel at the bottom of a paged list scrolls into
  // view, fetch the next page and append. The *Ref mirrors keep the callback
  // identity stable so the IntersectionObserver doesn't reconnect on every
  // loading-state flip (which would re-fire the trigger and double-load). Defined
  // after the lens union declarations so it can page either list.
  const loadingRef = useRef(loading);
  loadingRef.current = loading;
  const lensLoadingRef = useRef(lensLoading);
  lensLoadingRef.current = lensLoading;
  // A paged list shows the sentinel: repo Recent, or a lens union in a
  // non-relevance sort (lensSearch results aren't paged; an empty scope has none).
  const paged = isLens
    ? (effectiveSort !== 'relevance' && effectiveSort !== 'path' && !emptyScope && !lensExhausted)
    : effectiveSort === 'recent';
  const loadMore = useCallback(() => {
    if (isLens) {
      // Lens union paging (I5): fetch the next offset with the SAME params
      // (path/query/repos intersection) and append. Relevance/empty scopes and a
      // fully-loaded union don't page.
      if (effectiveSort === 'relevance' || effectiveSort === 'path' || emptyScope) return;
      // `lensExhausted` is the terminator, not `total`: with an overlapping
      // mount the count is an upper bound the row list can never reach, and the
      // sentinel would ask for empty pages forever. `total` is still checked —
      // it stops us one request EARLIER in the exact case (no mount truncated).
      if (lensLoadingRef.current || lensExhausted || lensRows.length >= total) return;
      // Close the double-fire window: the ref mirror only updates on re-render,
      // so a second observer tick before then would otherwise pass this guard
      // and double-load. Setting it synchronously blocks that.
      lensLoadingRef.current = true;
      setLensLoading(true);
      // Snapshot the scope generation; if it advances before we resolve, the
      // union has been reset to a new scope and these rows are stale.
      const gen = lensGenRef.current;
      api.listLensFacts(lensName, {
        path, query: state.freeText || undefined, limit: PAGE_SIZE, offset: lensRows.length, repos: reposParam,
        types: types.length ? types : undefined,
        kinds: kinds.length ? kinds : undefined,
        origins: origins.length ? origins : undefined,
        eps: eps.length ? eps : undefined,
        domains: domains.length ? domains : undefined,
        entities: entities.length ? entities : undefined,
        ...motifOpts,
      })
        .then(r => {
          if (gen !== lensGenRef.current) return;
          const page = (r.facts || []).map(f => ({ path: f.path, title: f.title, type: f.type, source: f.source }));
          setLensRows(prev => [...prev, ...page]);
          // Re-read the count on every page. It is the server's answer for this
          // query, and a later page can carry a better one than page 1 did: the
          // handler reports the exact deduped length whenever no mount was
          // truncated at that depth, and the depth grows with the offset.
          setTotal(r.total);
          if (page.length < PAGE_SIZE) setLensExhausted(true);
          setLensLoading(false);
        }).catch(() => { if (gen === lensGenRef.current) setLensLoading(false); });
      return;
    }
    if (effectiveSort !== 'recent') return;
    if (loadingRef.current || facts.length >= total) return;
    setLoading(true);
    api.recent(state.repo, state.branch, path, state.freeText, PAGE_SIZE, facts.length, {
      types: types.length ? types : undefined,
      kinds: kinds.length ? kinds : undefined,
      origins: origins.length ? origins : undefined,
      domains: domains.length ? domains : undefined,
      entities: entities.length ? entities : undefined,
      eps: eps.length ? eps : undefined,
      ...motifOpts,
    }).then(r => {
      setFacts(prev => [...prev, ...(r.facts || [])]);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [isLens, effectiveSort, emptyScope, lensExhausted, lensRows.length, lensName, reposKey, facts.length, total, state.repo, state.branch, path, state.freeText, types, kinds, origins, domains, entities, eps]);

  // The observer calls loadMore through a ref, and depends only on `paged`.
  // Depending on `loadMore` itself re-created the observer on every input to its
  // dep list — the row count, the filter arrays, the free text — so a paged list
  // churned through a GENERATION of observers, each holding a stale closure. The
  // live one usually won, but the window between a DOM commit and the passive
  // effect that registers the replacement was real: a sentinel tick landing in
  // it ran a generation-0 loadMore whose `lensRows.length >= total` guard read
  // `0 >= 0` and silently no-op'd, so the page was never fetched. One observer
  // for the life of the list, always calling the freshest loadMore, removes the
  // generation entirely.
  // Mirrored in an effect rather than during render (unlike loadingRef above,
  // which the loadMore guard has to read synchronously): an IntersectionObserver
  // callback only ever fires after a commit, so post-commit freshness is enough.
  const loadMoreRef = useRef(loadMore);
  useEffect(() => { loadMoreRef.current = loadMore; });

  useEffect(() => {
    if (!paged) return;
    const sentinel = sentinelRef.current;
    const root = containerRef.current;
    if (!sentinel || !root) return;
    const observer = new IntersectionObserver(
      ([entry]) => { if (entry.isIntersecting) loadMoreRef.current(); },
      { root, threshold: 0.1 }
    );
    observer.observe(sentinel);
    return () => observer.disconnect();
  }, [paged]);

  // openFact is the fact-open chokepoint: it navigates with the RAW canonical
  // path (bare for the write repo, kb://<id12>/… for a read mount). It does NOT
  // fetch the fact or dispatch SET_FACT_SOURCE — RightPanel is the single owner
  // of that fetch, with stale() guards. Prefetching+dispatching here as well
  // races under rapid re-open (keyboard moveSelection): a slow earlier response
  // could land after a newer fact opened, and RightPanel (keyed on factPath)
  // wouldn't correct the mismatched source.
  const openFact = useCallback((fullPath: string) => {
    navigate({ view: 'library', factPath: fullPath });
  }, [navigate]);

  // Stable row callbacks. Each row is memoized, so an inline arrow per row would
  // make the memo inert — every row would re-render on every Library render.
  const registerRef = useCallback((i: number, el: HTMLDivElement | null) => {
    itemRefs.current[i] = el;
  }, []);
  // Entering a directory is ONE action, whichever input triggers it. Clicking a
  // folder row used to dispatch ADD_FILTER{category:'path'} while Enter on the
  // same row dispatched NAVIGATE — two paths for one intent. Both already ended
  // at replacePathChip, so the only real divergence was rightPanelFocused, which
  // NAVIGATE clears and the chip did not: entering a folder by CLICK left focus
  // asserted on a right panel whose fact had just been closed.
  //
  // NAVIGATE is the survivor because it names the intent rather than the
  // mechanism, and because anything that wants to observe navigation (a back
  // stack, a location header) has one action to hook instead of two.
  const enterDir = useCallback((name: string) => {
    dispatch({ type: 'NAVIGATE', path: `${path}/${name}` });
  }, [dispatch, path]);

  // The list is keyed by location AND by sort axis: the same folder in Path and
  // in Recent is two different lists, so one offset cannot serve both.
  const scrollKey = `${path}|${effectiveSort}`;

  const activeList: RowItem[] = useMemo(() => {
    if (isLens && effectiveSort === 'path') {
      return lensTree.map(c => ({ name: c.name, fullPath: c.is_dir ? '' : (c.path || ''), is_dir: c.is_dir }));
    }
    if (isLens) {
      return lensRows.map(r => ({ name: displayLensPath(r.path), fullPath: r.path, is_dir: false }));
    }
    if (effectiveSort === 'recent') {
      return facts.map(f => ({ name: f.path.split('/').pop() || f.path, fullPath: f.path, is_dir: false }));
    }
    return children.map(c => ({ name: c.name, fullPath: c.fullPath || '', is_dir: c.is_dir }));
  }, [isLens, lensRows, lensTree, effectiveSort, facts, children]);

  // Restore on arrival, after the rows for this key have rendered. Depending on
  // activeList (not just the key) is what makes it land AFTER the fetch — a
  // restore into an empty list would clamp to 0 and lose the position.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const remembered = scrollMemory.current.get(scrollKey);
    // A key never visited scrolls to the top, which is also what a first visit
    // should do — so no branch for "unseen".
    el.scrollTop = remembered ?? 0;
  }, [scrollKey, activeList.length]);


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
      // Modified keys belong to window-level commands, not the list. Without
      // this, Alt+← ran BOTH — App's handler dispatching NAV_BACK and this one
      // dispatching GO_UP off the same event, since preventDefault does not
      // stop a second window listener. The list owns bare arrows only.
      if (e.metaKey || e.ctrlKey || e.altKey) return;
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

  // Location for the header. currentPath NEVER returns '' — it falls back to
  // the ontology root — so "am I at the root" is a comparison against that root,
  // not an emptiness check. Getting this wrong renders the root as a folder
  // called `kb` instead of the context + "All facts".
  const ontologyRoot = state.ontologyRoot || 'kb';
  const atRoot = path === ontologyRoot;
  const pathSegs = path.split('/');
  const ancestors = atRoot ? [] : pathSegs.slice(0, -1);
  const leaf = atRoot ? null : pathSegs[pathSegs.length - 1];
  // Ancestor index is into the FULL chain, so the target is that prefix. Split
  // inside the callback rather than closing over pathSegs: `path` is the simple
  // string the dep array wants, and the split costs nothing on a click.
  const jumpAncestor = useCallback((i: number) => {
    dispatch({ type: 'NAVIGATE', path: path.split('/').slice(0, i + 1).join('/') });
  }, [dispatch, path]);
  const goBack = useCallback(() => dispatch({ type: 'NAV_BACK' }), [dispatch]);

  // Whole names or none, ordered by how many rows carry each — the same grammar
  // the fact header's motif cells follow, so the two surfaces cannot drift.
  const pivotSubjects = useMemo(() => {
    if (!pivotMotif) return undefined;
    const rows = isLens ? lensRows.map(r => r.path) : facts.map(f => f.path);
    const s = subjectSummary(rows, 3);
    if (s.shown.length === 0) return undefined;
    return `across ${s.shown.join(' · ')}${s.more > 0 ? ` · +${s.more} more` : ''}`;
  }, [pivotMotif, isLens, lensRows, facts]);

  return (
    <div data-testid="left-panel" data-sort={effectiveSort} style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      <LibraryHeader
        /* The server's total for the current query, NOT the rows rendered.
           The paged tabs load 50 at a time, so rows.length is how far the
           reader has scrolled — a 385-fact repo read "50", then "100". `total`
           was already here gating the infinite-scroll sentinel; it just was
           not the number on screen. Path browse is unpaged, so there the two
           are the same thing and rows.length stays. */
        /* Three lists, three honest counts. Path browse is unpaged, so its rows
           ARE the count. Recent is paged, so the rows are a scroll position and
           the count must come from the server's total. Relevance is not paged
           either — the sentinel is disabled for it and the whole result set
           arrives in one call — so its rows are the count too; reading `total`
           there rendered 0, because nothing on the relevance path sets it. */
        count={effectiveSort === 'recent' ? total
          : isLens ? (effectiveSort === 'path' ? lensTree.length : lensRows.length)
          : children.length}
        ancestors={ancestors}
        leaf={leaf}
        narrow={narrow}
        sort={effectiveSort}
        searchActive={searchActive}
        contentFiltered={hasContentFilters}
        onSortChange={(sort) => dispatch({ type: 'SET_LIBRARY_SORT', sort })}
        onExitSearch={() => dispatch({ type: 'EXIT_SEARCH' })}
        onExitMotif={() => dispatch({ type: 'EXIT_MOTIF' })}
        // No liveness gate: in history this whole layer is inert (LeftPanel
        // swaps in TimelineNav, which carries its own back button), so gating
        // here only made the control look conditional when it is not.
        canBack={canGoBack(state)}
        onBack={goBack}
        onJumpAncestor={jumpAncestor}
        motif={pivotMotif ? {
          // canonical, never the spelling this fact happened to use and never
          // the cluster_key — the corpus reads by one name.
          canonical: pivotCluster?.canonical ?? pivotMotif,
          definition: pivotCluster?.definition,
          interim: pivotCluster?.definition_state === 'stale',
          // Computed from the landed rows: this list IS the result, so unlike
          // the fact panel's version of the same line it is complete rather
          // than drawn from a capped preview.
          subjects: pivotSubjects,
        } : undefined}
      />
      {!isLive(state) && (
        <ReadOnlyBanner message="Showing live library · history views not yet supported by backend" />
      )}
      <div
        ref={containerRef}
        onScroll={e => scrollMemory.current.set(scrollKey, (e.target as HTMLDivElement).scrollTop)}
        style={{ flex: 1, overflowY: 'auto' }}
      >
        {isLens && effectiveSort === 'path' && (
          <>
            {lensTree.length === 0 && !lensLoading && (
              <EmptyState message={
                noneSelected ? 'No sources selected.'
                  : emptyScope ? 'No sources match the selection.'
                  : 'No items in this path.'
              } />
            )}
            {lensTree.map((c, i) => (
              <EntryRow
                key={c.is_dir ? `dir:${c.name}` : (c.path || c.name)}
                testId="lens-tree-entry"
                index={i}
                selected={i === selectedIdx}
                name={c.name}
                title={c.title}
                type={c.type}
                isDir={c.is_dir}
                path={c.path || ''}
                dirPath={path}
                ontologyRoot={ontologyRoot}
                sourceRepo={c.source?.repo}
                truncateTitle
                onSelect={setSelectedIdx}
                onEnterDir={enterDir}
                onOpenFact={openFact}
                registerRef={registerRef}
              />
            ))}
            {lensLoading && <LoadingSpinner />}
          </>
        )}
        {isLens && effectiveSort !== 'path' && (
          <>
            {lensRows.length === 0 && !lensLoading && (
              <EmptyState message={
                noneSelected ? 'No sources selected.'
                  : emptyScope ? 'No sources match the selection.'
                  : state.freeText ? 'No facts match the search.'
                  : 'No facts in this lens.'
              } />
            )}
            {lensRows.map((f, i) => (
              <LensFactRow
                key={f.path}
                row={f}
                index={i}
                selected={i === selectedIdx}
                dirPath={path}
                ontologyRoot={ontologyRoot}
                onSelect={setSelectedIdx}
                onOpenFact={openFact}
                registerRef={registerRef}
              />
            ))}
            {/* Infinite-scroll sentinel — shared with repo Recent; only one list
                mounts at a time. Pages the union when more rows exist (I5). */}
            <div ref={sentinelRef} data-testid="recent-sentinel" style={{ height: 1 }} />
            {lensLoading && <LoadingSpinner />}
          </>
        )}
        {!isLens && (effectiveSort === 'path' || effectiveSort === 'relevance') && children.map((c, i) => (
          <EntryRow
            key={c.name}
            testId="dir-entry"
            index={i}
            selected={i === selectedIdx}
            name={c.name}
            title={c.title}
            type={c.type}
            isDir={c.is_dir}
            path={c.fullPath || ''}
            dirPath={path}
            ontologyRoot={ontologyRoot}
            truncateTitle={false}
            onSelect={setSelectedIdx}
            onEnterDir={enterDir}
            onOpenFact={openFact}
            registerRef={registerRef}
          />
        ))}
        {!isLens && (effectiveSort === 'path' || effectiveSort === 'relevance') && children.length === 0 && <EmptyState message={effectiveSort === 'relevance' ? 'No facts match the search.' : 'No items in this path.'} />}
        {!isLens && effectiveSort === 'recent' && (
          <>
            {facts.length === 0 && !loading && (
              <EmptyState message={state.freeText ? 'No facts match the search.' : 'No facts in this path.'} />
            )}
            {facts.map((f, i) => (
              <ChronoRow
                key={f.path}
                fact={f}
                index={i}
                selected={i === selectedIdx}
                subject={pivotMotif ? factSubject(f.path) : undefined}
                onSelect={setSelectedIdx}
                onOpenFact={openFact}
              />
            ))}
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
