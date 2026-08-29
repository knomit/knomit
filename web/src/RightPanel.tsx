import { memo, useCallback, useEffect, useRef, useState, useMemo } from 'react';
import type { Dispatch, ReactNode } from 'react';
import { useAsync } from './hooks';
import { api } from './api';
import type { Fact, Stats, ActivityStats, LensStats, RefGroup, RankAxis } from './api';
import type { AppState, Action } from './state';
import { currentPath, selectAnchorCommit, isReadOnly, isLive, READ_ONLY_TITLE, isLensContext, factHistoryAnchor, factTitleKey } from './state';
import { relativeTime } from './utils';
import { RetractIcon } from './icons';
import { FactDiffView } from './FactDiffView';
import { FactBody } from './FactBody';
import { FactBand } from './FactBand';
import { ConnectionsCell } from './ConnectionsMenu';
import type { EdgeDir } from './utils';
import { ConnectionsPanel } from './ConnectionsPanel';
import { VersionWalker } from './VersionWalker';
import { HighlightsPanel } from './HighlightsPanel';
import { FacetPanel } from './FacetPanel';
import { MotifCell } from './MotifCell';
import { MotifOverflowCell, orderMotifs, OVERFLOW } from './MotifRow';
import { useMotifClusters } from './useMotifClusters';
import { MotifPanel } from './MotifPanel';
import type { OrderedMotifs } from './MotifRow';
import { RepoRows } from './RepoRows';
import type { NavRequest } from './useNavigationManager';

/** Everything the header's connections menu and its panel need. */
interface ConnectionsSlot {
  panelId: string;
  open: EdgeDir | null;
  incoming: RefGroup[];
  outgoing: RefGroup[];
  error: string | null;
  onToggle: (dir: EdgeDir) => void;
  onClose: () => void;
  onHop: (path: string, pinnedCommit: string) => void;
  menuRef: React.RefObject<HTMLSpanElement | null>;
  onMouseEnter: () => void;
  onMouseLeave: () => void;
}

function renderFact(
  fact: Fact,
  // The fact's HISTORY anchor (factHistoryAnchor): its own source mount + the
  // RELATIVE path. VersionWalker reads versions through the mount's repo-scoped
  // /commits endpoint, so it must NOT use the write target or the raw kb:// path
  // (that pairing was the m36 no-op on read-mount facts).
  histAnchor: { repo: string; branch: string; path: string },
  dispatch: Dispatch<Action>,
  onRetract?: () => void,
  onScrub?: (commit: string) => void,
  onHopRef?: (path: string, pinnedCommit: string) => void,
  readOnly = false,
  anchorCommit?: string | null,
  lensMeta?: { repo: string; branch: string },
  readOnlyTitle: string = READ_ONLY_TITLE,
  // path → the outgoing DERIVED_FROM edge's target_commit (the version the
  // referrer reasoned over). Sourced from the open fact's own outgoing edges.
  refCommits: Map<string, string> = new Map(),
  // 12-hex KB-store id → mounted repo name, for the References labels.
  repoNames: Record<string, string> = {},
  // The header's connections menu + its panel. Bundled because these six move
  // together and renderFact already carries thirteen positional parameters.
  connections?: ConnectionsSlot,
  // The band lives OUTSIDE the scroller and needs to know when the fact's own
  // title has left it; RightPanel owns the observer, this owns the ref.
  band?: { pinned: boolean; titleRef: React.RefObject<HTMLDivElement | null>; scrollRef: React.RefObject<HTMLDivElement | null> },
  // The fact's motifs, already resolved by the panel above, and the slot that
  // owns which one is open. Both optional so the many call sites that render a
  // fact without the motif surface (diff views, tests) stay unchanged.
  motifs: OrderedMotifs = { shown: [], hidden: [] },
  motifSlot?: {
    open: string | null;
    onToggle: (motif: string) => void;
    onClose: () => void;
    onPivot: (motif: string) => void;
    panelId: string;
  },
  // Whether a tag or origin click can still become a filter chip.
  //
  // NOT `readOnly`, which is why this is its own parameter: read-only means
  // "your writes do not go here" — a read mount in a lens — and filtering is
  // navigation, not a write, so those facts stay filterable. This is about the
  // BAR. While the view is anchored, FilterBar renders the trail breadcrumb
  // instead of the chip row, so a chip minted here would be invisible,
  // unremovable, and waiting to narrow the list the moment the reader returns
  // to live.
  filterable = true,
) {
  const retractDisabled = readOnly;
  const retractTitle = retractDisabled ? readOnlyTitle : 'Retract fact';
  const retractColor = retractDisabled ? '#444' : '#f66';
  // Retracted-version badge: only when anchorCommit is set (history+history)
  // and fact.commit_hash is a different commit (the backend's ?fallback=before
  // walked back to a pre-retraction version). Compare 7-char prefixes since
  // anchorCommit may already be short.
  const anchorShort = anchorCommit ? anchorCommit.slice(0, 7) : '';
  const factShort = fact.commit_hash ? fact.commit_hash.slice(0, 7) : '';
  const retractedAt = anchorShort && factShort && anchorShort !== factShort ? anchorShort : '';
  // Pinned commit for in-body ref hops (narrowed to string for the closure).
  const refAnchor = fact.commit_hash;
  // ROW 3 IS TWO THINGS, and the border is the line between them.
  //
  // `edgesGroup` holds everything that opens the panel below — what cites this
  // fact, what it cites, and what has the same shape. One border, hairline
  // dividers, every child borderless: the recipe the connections cells were
  // built on, extended rather than replaced.
  //
  // `actions` is everything else on that row: the version (a mode change, not a
  // panel), the date it is read with, and retract, which destroys the fact
  // rather than inspecting it. None of them belong inside a border that means
  // "opens a panel".
  const edgesGroup = (
          <span
            ref={connections?.menuRef}
            onMouseEnter={connections?.onMouseEnter}
            onMouseLeave={connections?.onMouseLeave}
            style={{ position: 'relative', display: 'flex', minWidth: 0 }}
          >
            <span data-testid="fact-control-strip" style={controlStrip}>
              {connections && (
                <>
                  <span style={stripCell}>
                    <ConnectionsCell
                      dir="in"
                      count={connections.incoming.length}
                      open={connections.open === 'in'}
                      onToggle={connections.onToggle}
                      panelId={connections.panelId}
                      error={connections.error}
                    />
                  </span>
                  <span style={stripDivider} />
                  <span style={stripCell}>
                    <ConnectionsCell
                      dir="out"
                      count={connections.outgoing.length}
                      open={connections.open === 'out'}
                      onToggle={connections.onToggle}
                      panelId={connections.panelId}
                      error={connections.error}
                    />
                  </span>
                </>
              )}
              {/* The shape cells. Ordered by how many facts carry each motif,
                  never by the order the author typed them into the file — that
                  order means nothing, and the panel below sorts the same way so
                  the row and the panel cannot disagree about one list. */}
              {motifs.shown.map(m => (
                <span key={m.motif} style={{ display: 'inline-flex' }}>
                  <span style={stripDivider} />
                  <span style={stripCell}>
                    <MotifCell
                      motif={m}
                      open={motifSlot?.open === m.motif}
                      onToggle={motifSlot?.onToggle ?? (() => {})}
                      panelId={motifSlot?.panelId ?? ''}
                    />
                  </span>
                </span>
              ))}
              {motifs.shown.length === 0 && (
                <>
                  <span style={stripDivider} />
                  <span style={stripCell}>
                    <MotifCell motif={null} open={false} onToggle={() => {}} panelId="" />
                  </span>
                </>
              )}
              {motifs.hidden.length > 0 && (
                <>
                  <span style={stripDivider} />
                  <span style={stripCell}>
                    <MotifOverflowCell
                      hidden={motifs.hidden}
                      open={motifSlot?.open === OVERFLOW}
                      onToggle={motifSlot?.onToggle ?? (() => {})}
                      panelId={motifSlot?.panelId ?? ''}
                    />
                  </span>
                </>
              )}
            </span>
            {motifSlot?.open && motifs.shown.length + motifs.hidden.length > 0 && (
              <MotifPanel
                id={motifSlot.panelId}
                motifs={[...motifs.shown, ...motifs.hidden]}
                focused={motifSlot.open === OVERFLOW ? null : motifSlot.open}
                onClose={motifSlot.onClose}
                onPivot={motifSlot.onPivot}
                menuRef={connections?.menuRef ?? { current: null }}
                onMouseEnter={connections?.onMouseEnter ?? (() => {})}
                onMouseLeave={connections?.onMouseLeave ?? (() => {})}
              />
            )}
            {connections && (
              <ConnectionsPanel
                id={connections.panelId}
                open={connections.open}
                incoming={connections.incoming}
                outgoing={connections.outgoing}
                error={connections.error}
                onClose={connections.onClose}
                onHop={connections.onHop}
                menuRef={connections.menuRef}
                onMouseEnter={connections.onMouseEnter}
                onMouseLeave={connections.onMouseLeave}
              />
            )}
          </span>
  );

  const actions = (
    <>
          <span style={{ display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0 }}>
            {fact.commit_date && (
              // WHEN THIS VERSION OF THE FACT WAS COMMITTED — not the anchor's
              // date. It sits here, left of the control strip, for two reasons:
              // the band never scrolls, so it survives a long fact; and it lands
              // beside the version chip, so "v4 · 3d ago" reads as one
              // statement. Deliberately NOT inside the strip — every cell in
              // there is a control, and this is a readout.
              //
              // Amber when anchored, matching the timeline rail and the status
              // pill, where amber already means "you are not at HEAD".
              <span
                data-testid="fact-version-date"
                title={new Date(fact.commit_date).toLocaleString()}
                style={{
                  color: anchorCommit ? '#e5a23c' : '#555',
                  fontSize: 11, fontFamily: 'var(--k-font-mono)',
                  whiteSpace: 'nowrap', flexShrink: 0,
                }}
              >
                {anchorCommit
                  ? new Date(fact.commit_date).toLocaleDateString(undefined,
                      { day: 'numeric', month: 'short', year: 'numeric' })
                  : relativeTime(fact.commit_date)}
              </span>
            )}
            {/*
              THE CONTROL STRIP: connections in, connections out, version.
              One border and hairline dividers, and every child renders
              borderless inside it — three different treatments (two bare
              glyphs and a bordered chip) is what made these read as unrelated
              things floating beside the title.

              RETRACT IS DELIBERATELY OUTSIDE IT. Everything in the strip
              inspects the fact; retract destroys it. Sharing a cell wall with
              the navigation controls would make the destructive action look
              like one more place to click.
            */}
            {/* VERSION LEFT THE BORDER. Everything inside the group opens the
                panel below; the version walker does something else entirely —
                it puts the app into history and rotates the left rail into a
                timeline — and it removes itself while its own commits load and
                on a fact with no recorded versions. Outside, it can come and go
                without leaving a hole in a border, and it sits beside the date
                it is read with, which is where "v2 · 3d ago" reads as one
                statement. */}
            {fact.commit_hash && (
              <VersionWalker
                repo={histAnchor.repo}
                branch={histAnchor.branch}
                factPath={histAnchor.path}
                currentCommit={fact.commit_hash}
                onScrub={onScrub ?? (() => {})}
              />
            )}
            {retractedAt && (
              <span
                data-testid="retracted-version-badge"
                title={`This fact was retracted at ${retractedAt}; showing its content from ${factShort}`}
                style={{ color: '#e5a23c', fontFamily: 'var(--k-font-mono)', fontSize: 11, background: 'rgba(229,162,60,0.12)', border: '1px solid rgba(229,162,60,0.35)', padding: '1px 5px', borderRadius: 3 }}
              >
                retracted at {retractedAt}
              </span>
            )}
            {onRetract && (
              <button
                data-testid="retract-btn"
                title={retractTitle}
                disabled={retractDisabled}
                onClick={onRetract}
                style={{
                  background: 'none', border: 'none', padding: 2,
                  color: retractColor, cursor: retractDisabled ? 'not-allowed' : 'pointer',
                  display: 'flex', alignItems: 'center',
                  opacity: retractDisabled ? 0.4 : 0.6,
                }}
                onMouseEnter={e => { if (!retractDisabled) (e.currentTarget as HTMLElement).style.opacity = '1'; }}
                onMouseLeave={e => { if (!retractDisabled) (e.currentTarget as HTMLElement).style.opacity = '0.6'; }}
              ><RetractIcon color={retractColor} size={15} /></button>
            )}
          </span>
    </>
  );

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <FactBand fact={fact} dispatch={dispatch} lensMeta={lensMeta}
        pinned={band?.pinned ?? false} edges={edgesGroup} actions={actions} filterable={filterable} />
      <div ref={band?.scrollRef}
        style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: '20px 28px 24px', boxSizing: 'border-box' }}>
        <div ref={band?.titleRef} data-testid="fact-title" style={{
          fontFamily: 'var(--k-font-display)', fontSize: 18, fontWeight: 600,
          color: '#eee', letterSpacing: '-0.3px', marginBottom: 18,
        }}>
          {fact.title || fact.path}
        </div>

      <FactBody
        fact={fact}
        dispatch={dispatch}
        repoNames={repoNames}
        filterable={filterable}
        // Pin the hop to the ref's DERIVED_FROM edge target_commit — the exact
        // version of the target the referrer reasoned over, recorded per
        // ref-event at index time (resolveTargetCommit's first-parent walk from
        // the ORIGINAL source commit). This is authoritative across PR merges:
        // fact.commit_hash (the referrer's CURRENT version) + ?fallback=before
        // walks first-parent from the tip, which cannot reach a target version
        // that lives on a merge's second-parent line — yielding a 404 for refs
        // to targets retracted before the referrer's displayed version. Matches
        // what the connections panel's "OUT" rows already hop to. Falls back to the
        // referrer's own commit only when a ref has no matching edge.
        onRefClick={onHopRef ? (refPath: string) => {
          const pinned = refCommits.get(refPath) ?? refAnchor;
          if (pinned) onHopRef(refPath, pinned);
        } : undefined}
      />
      </div>
    </div>
  );
}

function FactEditor({ fact, repo, branch, readOnly, onSaved, readOnlyTitle = READ_ONLY_TITLE }: { fact: Fact; repo: string; branch: string; readOnly: boolean; onSaved: (updated: Fact) => void; readOnlyTitle?: string }) {
  const [raw, setRaw] = useState(fact.body);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const save = () => {
    if (readOnly) return;
    setSaving(true);
    setSaveError(null);
    api.updateFact(repo, branch, fact.path, raw)
      .then(updated => { setSaving(false); onSaved(updated); })
      .catch(e => { setSaving(false); setSaveError(String(e)); });
  };

  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box', display: 'flex', flexDirection: 'column', gap: 16 }}>
      <div style={{ background: '#2e1a1a', border: '1px solid rgba(255,80,80,0.3)', borderRadius: 6, padding: '10px 14px' }}>
        <div style={{ color: '#f88', fontSize: 11, textTransform: 'uppercase', letterSpacing: 1.2, marginBottom: 4 }}>Parse error</div>
        <div style={{ color: '#f44', fontSize: 12, fontFamily: 'var(--k-font-mono)' }}>{fact.parse_error}</div>
      </div>
      <div style={{ fontSize: 12, color: '#555' }}>{fact.path}</div>
      <textarea
        data-testid="fact-editor"
        value={raw}
        onChange={e => setRaw(e.target.value)}
        spellCheck={false}
        style={{
          flex: 1, minHeight: 320, background: '#0d0d14', color: '#ccc', border: '1px solid #2a2a3a',
          borderRadius: 6, padding: '12px 14px', fontFamily: 'var(--k-font-mono)', fontSize: 12,
          lineHeight: 1.6, resize: 'none', outline: 'none', boxSizing: 'border-box', width: '100%',
        }}
      />
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <button
          data-testid="fact-save-btn"
          onClick={save}
          disabled={saving || readOnly}
          title={readOnly ? readOnlyTitle : undefined}
          style={{
            background: '#1a2e1a', border: '1px solid rgba(119,204,153,0.35)', color: '#7c9',
            padding: '6px 16px', borderRadius: 4,
            cursor: (saving || readOnly) ? 'not-allowed' : 'pointer',
            fontSize: 13, opacity: (saving || readOnly) ? 0.6 : 1,
          }}
        >{saving ? 'Saving\u2026' : 'Save'}</button>
        {saveError && <span style={{ color: '#f88', fontSize: 12 }}>{saveError}</span>}
      </div>
    </div>
  );
}

// ─── Confirm Modal ───────────────────────────────────────────────────────────

function ConfirmModal({ message, onConfirm, onCancel }: {
  message: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  // Close on Escape
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCancel();
      if (e.key === 'Enter') onConfirm();
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onConfirm, onCancel]);

  return (
    <div
      onClick={onCancel}
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
        display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000,
      }}
    >
      <div
        onClick={e => e.stopPropagation()}
        style={{
          background: '#1a1a2a', border: '1px solid #333', borderRadius: 8,
          padding: '24px 28px', maxWidth: 400, width: '90%', boxShadow: '0 8px 32px rgba(0,0,0,0.6)',
        }}
      >
        <div style={{ fontSize: 13, color: '#ccc', lineHeight: 1.6, marginBottom: 20 }}>{message}</div>
        <div style={{ display: 'flex', gap: 10, justifyContent: 'flex-end' }}>
          <button
            onClick={onCancel}
            style={{
              background: 'none', border: '1px solid #333', borderRadius: 4,
              color: '#888', cursor: 'pointer', padding: '6px 16px', fontSize: 12,
            }}
          >Cancel</button>
          <button
            data-testid="retract-confirm-btn"
            onClick={onConfirm}
            style={{
              background: '#2e1a1a', border: '1px solid rgba(255,80,80,0.4)', borderRadius: 4,
              color: '#f66', cursor: 'pointer', padding: '6px 16px', fontSize: 12,
            }}
          >Retract</button>
        </div>
      </div>
    </div>
  );
}

// ─── Summary-view components ─────────────────────────────────────────────────

// StatFigure is the compact inline form of StatBox used by the overview strip.
// The overview shows five of these, so the boxed StatBox form cost ~100px of
// vertical space that the highlights list now uses.
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
function LensStatsView({ stats, dispatch, axis, onAxisChange, navigate }: {
  stats: LensStats;
  dispatch: Dispatch<Action>;
  axis: RankAxis;
  onAxisChange: (a: RankAxis) => void;
  navigate?: (req: NavRequest) => void;
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

// The fact header's control strip. One border, one fill, hairline dividers —
// the children draw none of their own.
const controlStrip: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'stretch',
  border: '1px solid #2a2a2a', borderRadius: 4, background: '#141414',
  overflow: 'hidden', flexShrink: 0,
};
const stripCell: React.CSSProperties = { display: 'inline-flex', alignItems: 'center' };
const stripDivider: React.CSSProperties = { width: 1, background: '#2a2a2a', flexShrink: 0 };

const EMPTY_REF_COMMITS: Map<string, string> = new Map();
// Stable identities, so a default does not defeat the memo with a fresh array.
const EMPTY_EDGES: RefGroup[] = [];
const CONNECTIONS_PANEL_ID = 'connections-panel';

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
  const [fact, setFact] = useState<Fact | null>(null);
  const [stats, setStats] = useState<Stats | null>(null);
  const [activity, setActivity] = useState<ActivityStats | null>(null);
  const [lensStats, setLensStats] = useState<LensStats | null>(null);
  const [lensStatsError, setLensStatsError] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);
  const [retracting, setRetracting] = useState(false);
  const [confirmRetract, setConfirmRetract] = useState(false);

  // path → target_commit for the open fact's outgoing DERIVED_FROM edges, so an
  // in-body ref hop lands on the version the referrer reasoned over. See the
  // FactBody onRefClick in renderFact for why this beats fact.commit_hash.
  const path = currentPath(state);

  const factPath = state.factPath;

  // ── Connections menu ──────────────────────────────────────────────────────
  // The open direction lives here rather than in App because the menu is part
  // of the fact header, which this component renders.
  const [connectionsOpen, setConnectionsOpen] = useState<EdgeDir | null>(null);
  const connectionsMenuRef = useRef<HTMLSpanElement>(null);
  const connectionsTimer = useRef<number | undefined>(undefined);

  // Which motif's panel is open, if any. Its own state rather than a widened
  // `connectionsOpen`: the two panels show different things and only one may be
  // open at a time, which the toggles below enforce by closing the other.
  const [motifOpen, setMotifOpen] = useState<string | null>(null);
  const toggleMotif = useCallback((motif: string) => {
    setConnectionsOpen(null);
    setMotifOpen(cur => (cur === motif ? null : motif));
  }, []);

  // The open fact's motifs, resolved to their clusters. The NAMES came with the
  // fact and cost nothing; each count is a request, so the row draws the names
  // at once and fills the counts in — see useMotifClusters.
  //
  // The anchor is the fact's own history anchor, the same one the edges use, so
  // a lens fact resolves against the mount it actually lives in rather than the
  // write repo.
  const motifAnchor = factHistoryAnchor(state);
  const resolvedMotifs = useMotifClusters(motifAnchor.repo, motifAnchor.branch, fact?.motifs);
  const orderedMotifs = useMemo(() => orderMotifs(resolvedMotifs), [resolvedMotifs]);
  const motifPanelId = 'motif-panel';

  const closeMotif = useCallback(() => setMotifOpen(null), []);
  // The pivot: one intent-named action in the reducer, which sets the chip, the
  // sort, the tier and the open fact together and pushes exactly one history
  // entry. The panel closes because the view underneath it is about to be a
  // different list.
  const pivotMotif = useCallback((motif: string) => {
    setMotifOpen(null);
    dispatch({ type: 'PIVOT_MOTIF', motif });
  }, [dispatch]);

  const closeConnections = useCallback(() => setConnectionsOpen(null), []);
  const toggleConnections = useCallback(
    (dir: EdgeDir) => {
      setMotifOpen(null);
      setConnectionsOpen(cur => (cur === dir ? null : dir));
    }, []);

  const cancelConnectionsClose = useCallback(() => {
    window.clearTimeout(connectionsTimer.current);
    connectionsTimer.current = undefined;
  }, []);

  /**
   * Hover-out closes, after a grace period.
   *
   * The delay is not decoration: the menu and the panel are separated by a 6px
   * gap that belongs to neither, so crossing it fires a leave. It also stops a
   * pointer clipping a corner on its way elsewhere from dismissing the panel.
   *
   * There used to be a second guard here, suppressing the close while a
   * multi-version edge's dropdown was open — that dropdown portalled to
   * document.body, so reaching for it fired the panel's mouseleave. The picker
   * is gone (edges pin to one version now), and with it the only portal inside
   * the panel, so the guard went too. Re-add one if anything in here starts
   * rendering outside the panel's DOM subtree.
   */
  const scheduleConnectionsClose = useCallback(() => {
    cancelConnectionsClose();
    connectionsTimer.current = window.setTimeout(() => setConnectionsOpen(null), 250);
  }, [cancelConnectionsClose]);

  useEffect(() => cancelConnectionsClose, [cancelConnectionsClose]);

  // Close when the fact changes, or a panel opened on one fact hangs over the
  // next one's header while you arrow down the list.
  //
  // Adjusted DURING RENDER rather than in an effect: React re-runs this
  // component immediately with the corrected state, before committing anything
  // to the DOM, so the stale panel never paints. An effect would commit the
  // open panel over the new fact first and close it a frame later.
  const [prevFactPath, setPrevFactPath] = useState(factPath);
  if (factPath !== prevFactPath) {
    setPrevFactPath(factPath);
    setConnectionsOpen(null);
  }
  const anchorCommit = selectAnchorCommit(state);
  const inDiff = state.asOf.mode === 'diff';

  // History asOf + anchor: opt into the backend's ?fallback=before so that
  // clicking a retracted file shows the pre-retraction content instead of a 404.
  const useFallback = state.asOf.mode === 'history' && !!anchorCommit;

  // In a lens context the open fact must be read THROUGH the lens: factPath is
  // the RAW canonical address (bare for the write repo, kb://<id12>/… for a read
  // mount), which the repo-scoped api.fact endpoint can't resolve. getLensFact
  // resolves it and returns the source mount, which RightPanel re-dispatches
  // (coherent with Library's row-click open) so a failed/racing open can't strand
  // a stale factSource on the new factPath (the m30 regression).
  const lensCtx = isLensContext(state);
  const lensName = state.lens?.name;
  const lensWrite = state.lens?.write.name;
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

  // Resolve the write repo's AGENT branch for lens writes. The open fact's
  // factSource.branch is the WRITE MOUNT's READ branch (WriteMountBranch) — which
  // Lens.normalize preserves when the write repo is pinned (e.g. core@main), a
  // NON-agent branch. Writes must land on the agent branch (the only branch the
  // repo write handlers accept as WritableBranch), so resolve it explicitly and
  // cache it; the read-mount branch stays purely a display concern (meta line).
  const [writeBranch, setWriteBranch] = useState<string | null>(null);
  useAsync((stale) => {
    if (!lensCtx || !lensWrite) { setWriteBranch(null); return; }
    api.getAgentBranch(lensWrite)
      .then(b => { if (!stale()) setWriteBranch(b); })
      .catch(() => { /* fall back to state.branch (already the write agent branch) */ });
  }, [lensCtx, lensWrite]);

  useAsync((stale) => {
    // In diff mode, FactDiffView owns the fact fetching via api.factDiff.
    // Skip this effect's fetch entirely so we don't issue a single-sided
    // request that gets discarded and may flash a 404 error.
    if (inDiff) { setFact(null); setError(null); return; }
    if (!factPath) { setFact(null); setError(null); return; }
    setError(null);
    setFact(null);
    if (lensCtx && lensName) {
      // Anchored lens read (C1): a scrub/diff entered from an open fact carries
      // an anchorCommit drawn from the fact's OWN mount timeline (VersionWalker),
      // and factSource is already set to that mount. getLensFact ignores the
      // anchor (always live), which would show the live body while the retracted-
      // badge/scrub UI thinks it's off-live. Read the anchored version through the
      // mount's repo-scoped commit endpoint instead — exactly as the repo-context
      // branch does — via factHistoryAnchor (mount repo/branch + RELATIVE path).
      // factSource is unchanged (same fact, same mount) so we don't re-dispatch it.
      if (anchorCommit && state.factSource) {
        const a = factHistoryAnchor(state);
        api.fact(
          a.repo, a.branch, a.path,
          anchorCommit,
          useFallback ? { fallback: 'before' } : undefined,
        )
          .then(f => { if (!stale()) setFact(f); })
          .catch(e => { if (!stale()) setError(String(e)); });
        return;
      }
      api.getLensFact(lensName, factPath)
        .then(f => {
          if (stale()) return;
          setFact(f);
          dispatch({ type: 'SET_FACT_SOURCE', source: f.source });
        })
        .catch(e => {
          if (stale()) return;
          setError(String(e));
          // m30: never leave a stale source paired with the new (failed) fact.
          dispatch({ type: 'SET_FACT_SOURCE', source: null });
        });
      return;
    }
    api.fact(
      state.repo, state.branch, factPath,
      anchorCommit ?? undefined,
      useFallback ? { fallback: 'before' } : undefined,
    )
      .then(f => {
        if (stale()) return;
        setFact(f);
      })
      .catch(e => { if (!stale()) setError(String(e)); });
  }, [factPath, anchorCommit, state.repo, useFallback, inDiff, lensCtx, lensName]);


  // Cache the loaded fact's title so the breadcrumb labels this crumb with the
  // title we already read — instead of a separate fetch that 404s for a
  // retracted fact. Keyed identically to the breadcrumb's crumb key.
  useEffect(() => {
    if (fact?.title && factPath) {
      dispatch({ type: 'CACHE_FACT_TITLE', key: factTitleKey(factPath, anchorCommit ?? undefined), title: fact.title });
    }
  }, [fact, factPath, anchorCommit, dispatch]);

  // Highlights ranking axis: null = follow the server's recommendation
  // (stats.default_axis / lensStats.default_axis); set = the user picked one.

  // Shared between the repo-summary view and LensStatsView rather than local
  // to each — they never render at once (one factPath-less summary view
  // shows either, gated on lensCtx), so one piece of state covers both, and
  // it's the state the stats-fetch effect below must read regardless of
  // which branch it takes.
  const [axis, setAxis] = useState<RankAxis | null>(null);

  // The band pins once the fact's OWN title has scrolled out of the body below
  // it, and then carries the title itself. An IntersectionObserver rather than
  // a scroll listener: the question is literally "is this element visible in
  // that box", which is what the observer answers, and it does not fire on
  // every frame of a scroll.
  const titleRef = useRef<HTMLDivElement | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [titlePinned, setTitlePinned] = useState(false);
  useEffect(() => {
    const title = titleRef.current;
    const root = scrollRef.current;
    if (!title || !root || typeof IntersectionObserver === 'undefined') return;
    const io = new IntersectionObserver(
      ([entry]) => setTitlePinned(!entry.isIntersecting),
      { root, threshold: 0 },
    );
    io.observe(title);
    return () => io.disconnect();
    // Re-observes per fact: a new fact re-renders a new title node, and opening
    // one always starts unpinned because its body starts at the top.
  }, [state.factPath, fact?.path]);
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

  // The repo-scoped write target for edits/retracts: {state.repo, state.branch} in
  // a repo context (unchanged); {lens.write, write-agent-branch} in a lens context.
  // The bare fact path is already write-repo-relative, so the existing
  // api.updateFact/retractFact reach the fact. Until getAgentBranch resolves we
  // fall back to state.branch, which in a lens context IS the write repo's agent
  // branch (App's status bootstrap resolved it the same way).
  const writeTarget = (lensCtx && lensWrite)
    ? { repo: lensWrite, branch: writeBranch ?? state.branch }
    : { repo: state.repo, branch: state.branch };
  // A lens fact is writable only when it lives in the lens's WRITE repo; read-mount
  // facts render fully read-only. Repo context keeps its prior gate (isReadOnly).
  const isWriteFact = !lensCtx || state.factSource?.repo === lensWrite;
  const factReadOnly = isReadOnly(state) || !isWriteFact;
  const factReadOnlyTitle = (!isWriteFact && lensWrite)
    ? `Read-only mount — edits go to ${lensWrite}`
    : READ_ONLY_TITLE;
  const lensMeta = lensCtx && state.factSource
    ? { repo: state.factSource.repo, branch: state.factSource.branch }
    : undefined;

  const doRetract = useCallback(() => {
    if (!fact || retracting || factReadOnly) return;
    setConfirmRetract(false);
    setRetracting(true);
    api.retractFact(writeTarget.repo, writeTarget.branch, fact.path)
      .then(() => {
        setRetracting(false);
        // Clear the fact without touching headCommit. The git observer will
        // sync the index and then broadcast a status event with the new commit
        // hash, which triggers SET_HEAD in App.tsx. Only then will headCommit
        // change, ensuring the search/chrono re-fire against a fresh index.
        dispatch({ type: 'AMEND_NAV', factPath: null });
      })
      .catch(e => { setRetracting(false); setError(String(e)); });
  }, [fact, retracting, factReadOnly, writeTarget.repo, writeTarget.branch, dispatch]);

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

  // Diff mode with a selected fact renders FactDiffView in the detail area.
  if (inDiff && state.factPath) {
    return (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        <div style={{ flex: 1, overflow: 'auto', minHeight: 0 }}>
          <FactDiffView state={state as AppState & { factPath: string }} dispatch={dispatch} />
        </div>
      </div>
    );
  }

  if (error && factPath) {
    return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;
  }
  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  // Summary view: no fact selected
  if (!factPath) {
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
                    onAxisChange={setAxis} navigate={navigate} />
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
  }

  // Fact view (normal or time-travel)
  if (!fact) return <div style={{ padding: 24, color: '#666' }}>Loading...</div>;

  if (fact.parse_error) return <FactEditor fact={fact} repo={writeTarget.repo} branch={writeTarget.branch} readOnly={factReadOnly} readOnlyTitle={factReadOnlyTitle} onSaved={setFact} />;

  const readOnly = factReadOnly;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      {confirmRetract && (
        <ConfirmModal
          message={`Are you sure you want to retract "${fact.title || fact.path}"?`}
          onConfirm={doRetract}
          onCancel={() => setConfirmRetract(false)}
        />
      )}
      {/* No scroll here: the fact view scrolls its own body BELOW the band, so
          the band can stay put. A scroller here would move the band with it. */}
      <div style={{ flex: 1, minHeight: 0, overflow: 'hidden' }}>
        {renderFact(
          fact,
          // History anchor (VersionWalker) — the fact's own source mount + relative
          // path, NOT the write target. Repo context: {state.repo, state.branch, path}.
          factHistoryAnchor(state, fact.path),
          dispatch,
          () => { if (!readOnly) setConfirmRetract(true); },
          onScrub,
          onHopRef,
          readOnly,
          // Only pass the anchor in history+history mode — the retracted-
          // version badge is only meaningful there. In live/diff/tree the
          // anchor either matches the fact's commit_hash (no badge) or is
          // null (badge suppressed).
          useFallback ? anchorCommit : null,
          lensMeta,
          factReadOnlyTitle,
          refCommits,
          repoNames,
          {
            panelId: CONNECTIONS_PANEL_ID,
            open: connectionsOpen,
            incoming,
            outgoing,
            error: edgesError,
            onToggle: toggleConnections,
            onClose: closeConnections,
            onHop: onHopEdge ?? (() => {}),
            menuRef: connectionsMenuRef,
            onMouseEnter: cancelConnectionsClose,
            onMouseLeave: scheduleConnectionsClose,
          },
          { pinned: titlePinned, titleRef, scrollRef },
          orderedMotifs,
          { open: motifOpen, onToggle: toggleMotif, onClose: closeMotif, onPivot: pivotMotif, panelId: motifPanelId },
          isLive(state),
        )}
      </div>
    </div>
  );
});
