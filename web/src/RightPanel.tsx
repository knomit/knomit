import { memo, useEffect, useState } from 'react';
import type { Dispatch, ReactNode } from 'react';
import { useAsync } from './hooks';
import { api } from './api';
import type { Stats, ActivityStats, LensStats, RefGroup, RankAxis } from './api';
import type { AppState, Action } from './state';
import { currentPath, isLensContext } from './state';
import { relativeTime } from './utils';
import { FactSetView } from './FactSetView';
import { factsetView, fsListTotal, useFactSetView } from './factsetOptions';
import { searchKey } from './useFactSetStream';
import { FactDiffView } from './FactDiffView';
import { FactView } from './FactView';
import { HighlightsPanel } from './HighlightsPanel';
import { FacetPanel } from './FacetPanel';
import { motifEndpointOf } from './motifEndpoint';
import type { MotifEndpoint } from './motifEndpoint';
import { MotifsBlock } from './MotifsBlock';
import { RepoRows } from './RepoRows';
import type { NavRequest } from './useNavigationManager';

// ─── Summary-view components ─────────────────────────────────────────────────

// StatFigure is the compact inline stat used by the overview strip. The
// overview shows five of these, so a boxed form would cost ~100px of vertical
// space that the highlights list now uses.
function StatFigure({ label, value, color = '#e8edf3' }: {
  label: string; value: ReactNode; color?: string;
}) {
  return (
    <div>
      <span style={{ fontFamily: 'var(--k-font-display)', fontWeight: 600, fontSize: 17, color }}>{value}</span>
      <span style={{ display: 'block', fontSize: 9, letterSpacing: '.09em', textTransform: 'uppercase', color: '#5a6675', marginTop: 3 }}>{label}</span>
    </div>
  );
}

// LensStatsView is the lens-context summary: a union roll-up header (exact
// sums, total-weighted confidence, max last_commit — computed server-side by
// GET /lenses/{lens}/stats) over the merged histograms, then one compact row
// per mount.
function LensStatsView({ stats, dispatch, axis, onAxisChange, navigate, motifEndpoint, path }: {
  stats: LensStats;
  dispatch: Dispatch<Action>;
  axis: RankAxis;
  onAxisChange: (a: RankAxis) => void;
  navigate?: (req: NavRequest) => void;
  /** Passed in rather than derived here: RightPanel already holds the app
   *  state this comes from, and one derivation keeps the summary block and the
   *  fact header reading the same vocabulary. */
  motifEndpoint: MotifEndpoint;
  /** The ontology path the panel is describing — SCOPE for the motif block,
   *  exactly as it is for the repo summary's. */
  path: string;
}) {
  const domainCount = Object.keys(stats.domains).length;
  const entityCount = Object.keys(stats.entities).length;
  return (
    <>
      {/* Recency rides the totals row — on its own line it held one short
          string and cost the full height of a row. */}
      <div data-testid="lens-stats-header"
        style={{ display: 'flex', gap: 20, alignItems: 'baseline', paddingBottom: 11, borderBottom: '1px solid #1a1e24', marginBottom: 4 }}>
        <StatFigure label="Facts"      value={stats.total} />
        <StatFigure label="Confidence" value={stats.avg_confidence.toFixed(2)} color="#8af" />
        <StatFigure label="Domains"    value={domainCount} />
        <StatFigure label="Entities"   value={entityCount} />
        <StatFigure label="Repos"      value={stats.repo_count} />
        {/* The dashboard's own search box is gone: search lives in the chrome
            row now, always on screen, so a second field here would be the
            duplicate the box was originally added to avoid. */}
        {stats.last_commit && (
          <span title={new Date(stats.last_commit).toLocaleString()}
            style={{ marginLeft: 'auto', color: '#555', fontSize: 11 }}>
            updated {relativeTime(stats.last_commit)}
          </span>
        )}
      </div>
      <FacetPanel domains={stats.domains} entities={stats.entities} types={stats.types} dispatch={dispatch} />
      {/* The lens's own merged vocabulary, in the same slot the repo summary
          gives it. It used to be absent here because there was no single
          vocabulary across a lens to show; /lenses/{lens}/motifs is that
          vocabulary, so the block is a block in both contexts now rather than
          a feature with a switch on it. */}
      <MotifsBlock endpoint={motifEndpoint} path={path}
        onPick={motif => dispatch({ type: 'PIVOT_MOTIF', motif })} />
      <HighlightsPanel
        highlights={stats.highlights}
        axis={axis}
        onAxisChange={onAxisChange}
        // reveal: take the tree to the fact's folder as well as opening it, so
        // a highlight lands you somewhere you can look around, not on a fact
        // floating over whatever the left panel happened to be showing.
        onOpen={path => navigate?.({ view: 'library', factPath: path, reveal: true })}
      />
      <RepoRows repos={stats.repos} dispatch={dispatch} />
    </>
  );
}

// ─── Main RightPanel ─────────────────────────────────────────────────────────

const EMPTY_REF_COMMITS: Map<string, string> = new Map();
// Stable identities, so a default does not defeat the memo with a fresh array.
const EMPTY_EDGES: RefGroup[] = [];

// Browse's right-hand column: the summary when no fact is open, a FactSet or a
// diff when one of those is up, and otherwise the fact view (FactView).
export const RightPanel = memo(function RightPanel({ state, dispatch, navigate, onScrub, onHopRef, repoNames, refCommits = EMPTY_REF_COMMITS, incoming = EMPTY_EDGES, outgoing = EMPTY_EDGES, edgesError = null, onHopEdge }: {
  state: AppState;
  dispatch: Dispatch<Action>;
  /**
   * Opens a highlight through the SAME live path a Library row uses (path
   * only, no commit — kb/invariants/ui/navigation/every-hop-is-path-plus-commit
   * governs following a RECORDED reference with a target_commit; a highlights
   * listing of live facts is not that). Optional, like the other callback
   * props below, so the many RightPanel tests that never open a highlight
   * (empty/undefined `stats.highlights`) don't need to thread it through.
   */
  navigate?: (req: NavRequest) => void;
  onScrub?: (commit: string) => void;
  onHopRef?: (path: string, pinnedCommit: string) => void;
  /** 12-hex KB-store id → mounted repo name; see FactBody's refLabel. */
  repoNames?: Record<string, string>;
  /** The open fact's edges, from App's single fetch (useFactEdges). */
  incoming?: RefGroup[];
  outgoing?: RefGroup[];
  edgesError?: string | null;
  /** Hop to an edge target at a pinned commit. */
  onHopEdge?: (path: string, pinnedCommit: string) => void;
  /**
   * Outgoing edge path → target_commit, from App's single edge fetch
   * (useFactEdges). RightPanel used to fetch this itself with an api.explain
   * call identical to the connections panel's — same fact, same anchor, same
   * fallback — so a fact open cost two requests.
   */
  refCommits?: Map<string, string>;
}) {
  const [stats, setStats] = useState<Stats | null>(null);
  const [activity, setActivity] = useState<ActivityStats | null>(null);
  const [lensStats, setLensStats] = useState<LensStats | null>(null);
  const [lensStatsError, setLensStatsError] = useState<boolean>(false);

  // The FactSet of a search (opened from the results list's header) replaces
  // the right panel's view, as the single-fact view does (Q-UI14). It holds the
  // search it was opened with; the list may move on underneath (Q-UI4).
  const fsv = useFactSetView();
  const path = currentPath(state);
  const factPath = state.factPath;
  const motifEndpoint = motifEndpointOf(state);
  const inDiff = state.asOf.mode === 'diff';
  const lensCtx = isLensContext(state);
  const lensName = state.lens?.name;
  // Read-set fingerprint: an edit that adds/removes a mount keeps the lens NAME
  // but changes the reads, so the stats effect must re-fetch on it (a same-name
  // SET_LENS does not touch state.repo/headCommit).
  const lensReadSig = state.lens
    // uid AND name: the uid catches a mount being added or dropped, the name
    // catches a rename — which changes nothing about membership but does change
    // the mount names the stats fan-out is addressed by, and the labels shown.
    ? state.lens.reads.map(r => `${r.uid}:${r.name}@${r.branch ?? ''}`).join(',')
    : '';

  // Which of those mounts the reader currently has switched ON. Narrowing the
  // picker changes what the summary describes, so the stats effect re-fetches
  // on it — same signature trick as lensReadSig, because the array's identity
  // changes on every render while its CONTENT is the actual input.
  const lensSources = state.lensSources;
  /**
   * The selection filtered through the lens's ACTUAL read set, in mount order.
   *
   * `state.lensSources` can name a mount the lens no longer has: SET_LENS
   * replaces state.lens after an edit and deliberately leaves the selection
   * alone, so narrowing to X and then dropping X from the lens strands 'X' in
   * the array. The server 422s on an unknown mount, so sending it raw took the
   * dashboard down ("a mount failed to respond") beside a union list that was
   * fine — Library has always filtered the same value this way before sending
   * it, and the two must agree about what the reader selected.
   */
  const lensReadNames = state.lens ? state.lens.reads.map(r => r.name) : [];
  const selectedMounts = lensSources
    ? lensReadNames.filter(m => lensSources.includes(m))
    : null;
  // Signed by the FILTERED value, so a no-op edit (dropping a mount that was
  // already deselected) doesn't refetch, and dropping a SELECTED one does.
  const lensSourcesSig = selectedMounts ? selectedMounts.join(',') : '*';
  // Highlights ranking axis: null = follow the server's recommendation
  // (stats.default_axis / lensStats.default_axis); set = the user picked one.

  // Shared between the repo-summary view and LensStatsView rather than local
  // to each — they never render at once (one factPath-less summary view
  // shows either, gated on lensCtx), so one piece of state covers both, and
  // it's the state the stats-fetch effect below must read regardless of
  // which branch it takes.
  const [axis, setAxis] = useState<RankAxis | null>(null);

  // Reset DURING RENDER, not in an effect, when the summary scope (repo or
  // path or lens) changes — mirroring the connectionsOpen reset above. An
  // effect-based reset would still fire the stats fetch below with the STALE
  // axis for one pass, before the reset effect's own re-render corrected it:
  // a throwaway request on every navigation into a new folder.
  const axisScope = `${state.repo}\0${path}\0${lensName ?? ''}`;
  const [prevAxisScope, setPrevAxisScope] = useState(axisScope);
  if (axisScope !== prevAxisScope) {
    setPrevAxisScope(axisScope);
    setAxis(null);
  }

  useAsync((stale) => {
    if (factPath) return;
    if (lensCtx) {
      // Lens context: ONE union stats call through the lens endpoint. The
      // repo-scoped stats/activity pair would describe the WRITE mount only —
      // silently misleading while browsing a union (design 2026-07-20). Clear
      // prior state so a lens switch never flashes the old lens's rows, and a
      // failed fetch (the backend fails the WHOLE request on any mount error —
      // RFC §9.1) surfaces as an error, NOT a false "no facts" empty state.
      setLensStats(null);
      setLensStatsError(false);
      // Lens named but not yet resolved (state.lens still stale/null): wait for
      // the resolution effect rather than fetching the wrong lens or falling
      // through to a pointless repo-scoped fetch.
      if (!lensName) return;
      // No mount selected is a real state the picker can reach, and it is NOT
      // "all": sending no repo params would make the server fan out and answer
      // with every number the reader just switched off. Nothing to ask for, so
      // nothing is asked.
      // Empty AFTER the filter, too: a selection whose every mount has been
      // edited out of the lens describes no scope at all, which is the same
      // nothing-to-ask-for as an explicit "none" — and it is what the union list
      // shows, since Library treats that case as an empty scope as well.
      if (selectedMounts && selectedMounts.length === 0) return;
      // axis omitted entirely (not passed as an explicit `undefined`) when
      // unset, so a caller pinning the default call shape (no axis argument)
      // still matches — the server re-ranks over the full eligible set on a
      // picked axis; the client never re-sorts the truncated top-N. The mount
      // selection rides along the same way the union list sends it, so the
      // summary describes the scope the reader picked rather than the lens's
      // full read set.
      // Trailing arguments are omitted, never passed as explicit undefined, so
      // the default "all mounts, default axis" call is still getLensStats(lens,
      // path) — the shape callers and tests pin.
      const repos = selectedMounts ?? undefined;
      const req = repos
        ? (axis ? api.getLensStats(lensName, path, axis, repos) : api.getLensStats(lensName, path, undefined, repos))
        : (axis ? api.getLensStats(lensName, path, axis) : api.getLensStats(lensName, path));
      req
        .then(s => { if (!stale()) setLensStats(s); })
        .catch(() => { if (!stale()) setLensStatsError(true); });
      return;
    }
    Promise.all([
      (axis ? api.stats(state.repo, state.branch, path, axis) : api.stats(state.repo, state.branch, path))
        .catch(() => null),
      api.activity(state.repo, state.branch, path).catch(() => null),
    ]).then(([s, a]) => {
      if (stale()) return;
      setStats(s);
      setActivity(a);
    });
    // lensSourcesSig is the content-stable stand-in for lensSources, exactly as
    // lensReadSig is for state.lens.reads.
  }, [factPath, state.repo, path, state.headCommit, lensCtx, lensName, lensReadSig, lensSourcesSig, axis]);
  // Keyboard: ArrowLeft blurs right panel
  useEffect(() => {
    if (!state.rightPanelFocused) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'ArrowLeft') {
        e.preventDefault();
        dispatch({ type: 'BLUR_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, dispatch]);
  const replacedBy = fsv.search ? (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        <FactSetView
          key={searchKey(fsv.search)}
          search={fsv.search}
          options={fsv.options}
          listTotal={fsListTotal(fsv)}
          onOptionsChange={o => factsetView.setOptions(o)}
          onBack={() => factsetView.close()}
        />
      </div>
  ) : inDiff && state.factPath ? (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        <div style={{ flex: 1, overflow: 'auto', minHeight: 0 }}>
          <FactDiffView state={state as AppState & { factPath: string }} dispatch={dispatch} />
        </div>
      </div>
  ) : null;

  // Summary view: no fact selected
  const renderSummary = () => {
    if (lensCtx) {
      return (
        <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
          <div data-testid="stats-view" style={{ flex: 1, padding: '24px 28px', overflowY: 'auto', boxSizing: 'border-box' }}>
            {selectedMounts && selectedMounts.length === 0
              /* No mount selected. Distinct from "loading" — nothing is coming,
                 because there is nothing to ask for — and distinct from an
                 error. Same words the union list uses, so the two halves of the
                 screen agree about why they are both empty. */
              ? <div data-testid="lens-stats-empty" style={{ color: '#666' }}>No sources selected.</div>
              : lensStatsError
              ? <div data-testid="lens-stats-error" style={{ color: '#f88' }}>Couldn’t load lens stats — a mount failed to respond.</div>
              : lensStats
                ? <LensStatsView stats={lensStats} dispatch={dispatch} axis={axis ?? lensStats.default_axis}
                    onAxisChange={setAxis} navigate={navigate}
                    motifEndpoint={motifEndpoint} path={path} />
                : <div style={{ color: '#666' }}>Loading lens stats…</div>}
          </div>
        </div>
      );
    }
    const domainCount = stats ? Object.keys(stats.domains).length : 0;
    const entityCount = stats ? Object.keys(stats.entities).length : 0;
    const totalCommits = activity ? String(activity.total) : '\u2014';

    return (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        <div data-testid="stats-view" style={{ flex: 1, padding: '24px 28px', overflowY: 'auto', boxSizing: 'border-box' }}>
          {stats ? (
            <>
              {/* Recency rides the totals row — on its own line it held one
                  short string and cost the full height of a row. */}
              <div style={{ display: 'flex', gap: 20, alignItems: 'baseline', paddingBottom: 11, borderBottom: '1px solid #1a1e24', marginBottom: 4 }}>
                <StatFigure label="Facts"      value={stats.total} />
                <StatFigure label="Confidence" value={stats.avg_confidence.toFixed(2)} color="#8af" />
                <StatFigure label="Domains"    value={domainCount} />
                <StatFigure label="Entities"   value={entityCount} />
                <StatFigure label="Commits"    value={totalCommits} />
                {activity?.last_commit && (
                  <span title={new Date(activity.last_commit).toLocaleString()}
                    style={{ marginLeft: 'auto', color: '#555', fontSize: 11 }}>
                    {relativeTime(activity.last_commit)}
                  </span>
                )}
              </div>
              <FacetPanel domains={stats.domains} entities={stats.entities} types={stats.types} dispatch={dispatch} />
              {/* The repo's vocabulary. The lens summary above renders the
                  same block against its own merged vocabulary — cross-mount
                  cluster identity is what /lenses/{lens}/motifs defines — so
                  this is one block in two contexts, not a repo-only feature. */}
              <MotifsBlock endpoint={motifEndpoint} path={path}
                onPick={motif => dispatch({ type: 'PIVOT_MOTIF', motif })} />
              <HighlightsPanel
                highlights={stats.highlights}
                axis={axis ?? stats.default_axis}
                onAxisChange={setAxis}
                onOpen={p => navigate?.({ view: 'library', factPath: p, reveal: true })}
              />
            </>
          ) : <div style={{ color: '#666' }}>No facts indexed in this path.</div>}
        </div>
      </div>
    );
  };

  return (
    <FactView
      reading={state}
      dispatch={dispatch}
      onScrub={onScrub}
      onHopRef={onHopRef}
      onHopEdge={onHopEdge}
      repoNames={repoNames}
      incoming={incoming}
      outgoing={outgoing}
      edgesError={edgesError}
      refCommits={refCommits}
      replacedBy={replacedBy}
      noFact={factPath ? null : renderSummary()}
    />
  );
});
