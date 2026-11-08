import { useCallback, useEffect, useRef, useState, useMemo } from 'react';
import type { CSSProperties, Dispatch, ReactNode } from 'react';
import { PANEL_BOUNDS_ATTR, useAsync } from './hooks';
import { api, isCommitNotInRepo } from './api';
import type { Fact, RefGroup } from './api';
import type { Action, FactReading } from './state';
import { selectAnchorCommit, isReadOnly, isLive, READ_ONLY_TITLE, readOnlyTitle as readOnlyTitleFor, isLensContext, factHistoryAnchor, factTitleKey, openFactSourceResolved } from './state';
import { MaterializeView } from './MaterializeView';
import type { MatFocusScope } from './useMatFocus';
import { parseMaterializeParams, writeMaterializeParams } from './materializeOptions';
import type { MatOptions } from './materializeApi';
import { NoVersionAtCommit } from './NoVersionAtCommit';
import { FactBody } from './FactBody';
import { FactActions, FactEdgesGroup, FactPage } from './FactChrome';
import type { ConnectionsSlot, MotifSlot } from './FactChrome';
import type { EdgeDir } from './utils';
import { orderMotifs } from './MotifRow';
import { useMotifClusters } from './useMotifClusters';
import { motifEndpointOf } from './motifEndpoint';
import type { OrderedMotifs } from './MotifRow';

// THE fact view: everything a reader sees and can do on a fact. Browse mounts it
// (inside RightPanel) and so does the chat's side panel (ChatSidePanel), so the
// two cannot come to disagree. What it reads is a FactReading and what it can
// reach is its props; it has no other tie to Browse. Do not fork it, and do not
// add a chat rendering beside it.

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
  // The fact's motifs, already resolved by the panel above, and the slot that
  // owns which one is open. Both optional so the many call sites that render a
  // fact without the motif surface (diff views, tests) stay unchanged.
  motifs: OrderedMotifs = { shown: [], hidden: [] },
  motifSlot?: MotifSlot,
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
  // Swap this fact's view for its materialization. Optional so the many call
  // sites that render a fact without it (diff views, tests) stay unchanged.
  onMaterialize?: () => void,
  // The caller's own control at the end of the band's action group.
  bandAction?: ReactNode,
) {
  // Pinned commit for in-body ref hops (narrowed to string for the closure).
  const refAnchor = fact.commit_hash;
  // ROW 3 IS TWO THINGS: panel-openers on the left, everything else pinned
  // right.
  //
  // `edgesGroup` holds everything that opens the panel below — what cites this
  // fact, what it cites, and what shares its motif.
  //
  // IT USED TO BE A BORDERED STRIP with hairline dividers, so the border could
  // mean exactly one thing. Nothing else in the fact's chrome is drawn that
  // way: the meta line above it and the path line above that are bare text and
  // bare glyphs on the band's own ground. A box around three of the header's
  // cells made them read as a widget bolted onto the fact rather than as part
  // of it — so the box is gone and the cells sit on the band like everything
  // else. What the border was carrying, the cells already say for themselves:
  // colour for live, grey for inert, red for failed, and an inset underline in
  // currentColor for the one that is open. Those four states are untouched.
  //
  // `actions` is everything else on that row: the version (a mode change, not a
  // panel), the date it is read with, and retract, which destroys the fact
  // rather than inspecting it. They stay pinned right, where they always were.
  return (
    <FactPage fact={fact} dispatch={dispatch} lensMeta={lensMeta} filterable={filterable}
      edges={<FactEdgesGroup connections={connections} motifs={motifs} motifSlot={motifSlot} />}
      actions={
        <FactActions fact={fact} histAnchor={histAnchor} onScrub={onScrub} onRetract={onRetract}
          readOnly={readOnly} anchorCommit={anchorCommit} readOnlyTitle={readOnlyTitle} onMaterialize={onMaterialize}>{bandAction}</FactActions>
      }>
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
    </FactPage>
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

/**
 * The commit to materialize the shown fact at: the anchor, unless the fact
 * does not exist there. At the commit that retracted it (a version the
 * timeline lists) the anchored read used ?fallback=before and returned the
 * last live version, whose commit (as_of.commit → commit_hash) differs from
 * the anchor; materializing at the anchor would ask for a fact that is not
 * there. A read that did not fall back answers with the anchor itself, so this
 * is the anchor everywhere else. Live (no anchor): the branch tip.
 */
function materializeCommit(anchorCommit: string | null | undefined, fact: Pick<Fact, 'commit_hash'>): string | undefined {
  if (!anchorCommit) return undefined;
  return fact.commit_hash || anchorCommit;
}

const EMPTY_REF_COMMITS: Map<string, string> = new Map();
// Stable identities, so a default does not defeat the memo with a fresh array.
const EMPTY_EDGES: RefGroup[] = [];
const CONNECTIONS_PANEL_ID = 'connections-panel';
// The caller's corner control (the chat panel's ×): pinned to the view's top-right
// corner, inside the band's 28px right padding, so it overlaps nothing.
const cornerBox: CSSProperties = { position: 'absolute', top: 4, right: 2, zIndex: 2 };

export interface FactViewProps {
  /** The fact on screen and where it is read from. Browse hands its app reading. */
  reading: FactReading;
  /** Where the view's filter chips and bookkeeping go. */
  dispatch: Dispatch<Action>;
  onScrub?: (commit: string) => void;
  onHopRef?: (path: string, pinnedCommit: string) => void;
  /** Hop to an edge target at a pinned commit. */
  onHopEdge?: (path: string, pinnedCommit: string) => void;
  /** 12-hex KB-store id → mounted repo name; see FactBody's refLabel. */
  repoNames?: Record<string, string>;
  /** The open fact's edges, as useFactEdges answers them. */
  incoming?: RefGroup[];
  outgoing?: RefGroup[];
  edgesError?: string | null;
  refCommits?: Map<string, string>;
  /** The caller's own control in the view's top-right corner (the chat panel's
   *  ×), in every state: the fact, loading, an error, no version at the commit.
   *  NOT in the band's action row, where it sat beside retract and history until
   *  the chat review of 2026-11-08. The materialize view, which has a toolbar of
   *  its own, carries it at the end of that toolbar instead. */
  cornerAction?: ReactNode;
  /** No retract button. Nothing else changes. */
  hideRetract?: boolean;
  /** The materialize view keeps its options in the page's query string, as Browse
   *  does. Off, they stay here and the URL is left alone. */
  materializeInUrl?: boolean;
  /** See MatFocusScope. */
  materializeFocus?: MatFocusScope;
  /** What another of Browse's views shows in this fact's place (a FactSet, a diff). */
  replacedBy?: ReactNode;
  /** Shown when no fact is open. */
  noFact?: ReactNode;
}

export function FactView({ reading, dispatch, onScrub, onHopRef, onHopEdge, repoNames, refCommits = EMPTY_REF_COMMITS, incoming = EMPTY_EDGES, outgoing = EMPTY_EDGES, edgesError = null, cornerAction, hideRetract = false, materializeInUrl = true, materializeFocus, replacedBy, noFact }: FactViewProps) {
  const [fact, setFact] = useState<Fact | null>(null);
  const [error, setError] = useState<string | null>(null);
  // The commit an anchored read found this fact's repo does not hold (404
  // commit_not_in_repo), or null. A reading, not an error: see NoVersionAtCommit.
  const [noVersionAt, setNoVersionAt] = useState<string | null>(null);
  const [retracting, setRetracting] = useState(false);
  const [confirmRetract, setConfirmRetract] = useState(false);
  // The materialized view replaces the fact view. Its options live in the
  // page's query string (shareable, never persisted): read once at mount,
  // written back on every change.
  const [mat, setMat] = useState<{ open: boolean; options: MatOptions }>(
    () => parseMaterializeParams(materializeInUrl && typeof window !== 'undefined' ? window.location.search : ''));
  useEffect(() => {
    if (!materializeInUrl || typeof window === 'undefined') return;
    const search = writeMaterializeParams(window.location.search, mat);
    if (search !== window.location.search) {
      window.history.replaceState(window.history.state, '', window.location.pathname + search + window.location.hash);
    }
  }, [mat, materializeInUrl]);

  const factPath = reading.factPath;

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
  // The band's cited by, cites and motif cells open their menu IN PLACE, under
  // the band, wherever the view is mounted: Browse and the chat's side panel
  // alike. Only picking from the menu leaves (onHopEdge, the PIVOT_MOTIF
  // dispatch), and where that goes is the mount's business (chat review,
  // 2026-11-08; the panel used to hand the menu itself over to Browse).
  const toggleMotif = useCallback((motif: string) => {
    setConnectionsOpen(null);
    setMotifOpen(cur => (cur === motif ? null : motif));
  }, []);

  // The open fact's motifs, resolved to their clusters. The NAMES came with the
  // fact and cost nothing; each count is a request, so the row draws the names
  // at once and fills the counts in — see useMotifClusters.
  //
  // Resolved against the corpus the reader is BROWSING, not the mount the fact
  // happens to live on: in a lens that is the whole lens. The count sits beside
  // a pivot, and the pivot lists the lens — a mount-scoped count under a
  // lens-scoped pivot would promise one number and deliver another. (Edges and
  // history still anchor on the fact's own mount, via factHistoryAnchor: those
  // are questions about the fact's history, which is a property of where it
  // lives.)
  const motifEndpoint = motifEndpointOf(reading);
  const resolvedMotifs = useMotifClusters(motifEndpoint, fact?.motifs);
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
  // component immediately with the corrected reading, before committing anything
  // to the DOM, so the stale panel never paints. An effect would commit the
  // open panel over the new fact first and close it a frame later.
  const [prevFactPath, setPrevFactPath] = useState(factPath);
  if (factPath !== prevFactPath) {
    setPrevFactPath(factPath);
    setConnectionsOpen(null);
    // Another fact: its own view, not the last fact's materialization.
    if (mat.open) setMat(m => ({ ...m, open: false }));
  }

  const anchorCommit = selectAnchorCommit(reading);
  const inDiff = reading.asOf.mode === 'diff';

  // History asOf + anchor: opt into the backend's ?fallback=before so that
  // clicking a retracted file shows the pre-retraction content instead of a 404.
  const useFallback = reading.asOf.mode === 'history' && !!anchorCommit;

  // In a lens context the open fact must be read THROUGH the lens: factPath is
  // the RAW canonical address (bare for the write repo, kb://<id12>/… for a read
  // mount), which the repo-scoped api.fact endpoint can't resolve. getLensFact
  // resolves it and returns the source mount, which this view re-dispatches
  // (coherent with Library's row-click open) so a failed/racing open can't strand
  // a stale factSource on the new factPath (the m30 regression).
  const lensCtx = isLensContext(reading);
  const lensName = reading.lens?.name;
  const lensWrite = reading.lens?.write.name;

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
      .catch(() => { /* fall back to reading.branch (already the write agent branch) */ });
  }, [lensCtx, lensWrite]);

  // Whether factSource is the open fact's own mount (see openFactSourceResolved).
  const sourceResolved = openFactSourceResolved(reading);
  const resolvedDep = (anchorCommit || inDiff) ? sourceResolved : null;
  // Resolve the open lens fact's source mount without showing its (live) body:
  // the anchored and diff views read the body themselves, from that mount.
  const resolveSourceOnly = (stale: () => boolean) => {
    if (!lensName || !factPath) return;
    api.getLensFact(lensName, factPath)
      .then(f => { if (!stale()) dispatch({ type: 'SET_FACT_SOURCE', source: f.source }); })
      .catch(e => {
        if (stale()) return;
        setError(String(e));
        dispatch({ type: 'SET_FACT_SOURCE', source: null });
      });
  };

  // An anchored read that failed because this fact's repo does not hold the
  // commit shows the "no version" reading, not an error (user ruling
  // 2026-11-03). asOf is left as it is: history stays on, and the reader
  // scrubs or returns to live from the timeline.
  // showFact sets the fact a fetch for `factPath` returned and records which
  // open path it belongs to. The panel keeps rendering the PREVIOUS fact until
  // the new one loads (see openFactSourceResolved), so on a switch `fact` and
  // `factPath` disagree for a while — and when the new read never yields a fact
  // (no version at the anchored commit) they disagree for good. The title
  // cache below must not label the new crumb with the old fact's title.
  const factLoadedFor = useRef<string | null>(null);
  const showFact = (f: Fact) => {
    factLoadedFor.current = factPath;
    setFact(f);
  };
  const failAnchored = (e: unknown, commit: string) => {
    if (isCommitNotInRepo(e)) setNoVersionAt(commit);
    else setError(String(e));
  };

  useAsync((stale) => {
    // In diff mode, FactDiffView owns the fact fetching via api.factDiff.
    // Skip this effect's fetch entirely so we don't issue a single-sided
    // request that gets discarded and may flash a 404 error.
    //
    // In a lens, though, this effect is also the ONE place the open fact's
    // source mount is resolved (SET_FACT_SOURCE), and FactDiffView waits on it
    // (openFactSourceResolved). So a diff whose fact moved to another mount —
    // the mounts picker re-selects the first row while the anchor stays put —
    // still resolves the source here, without reading the fact body.
    if (inDiff) {
      setFact(null); setError(null); setNoVersionAt(null);
      if (factPath && lensCtx && lensName && !sourceResolved) resolveSourceOnly(stale);
      return;
    }
    if (!factPath) { setFact(null); setError(null); setNoVersionAt(null); return; }
    setError(null);
    setNoVersionAt(null);
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
      //
      // Only once factSource OWNS the open path, though. The open fact can move
      // to another mount while anchored (the mounts picker re-selects the first
      // row and keeps asOf), and the previous fact's mount would then be read
      // for this fact's path. Resolve the source first; the effect re-runs on
      // `sourceResolved` and reads the anchored version from the right mount.
      if (anchorCommit && !sourceResolved) { resolveSourceOnly(stale); return; }
      if (anchorCommit && reading.factSource) {
        const a = factHistoryAnchor(reading);
        api.fact(
          a.repo, a.branch, a.path,
          anchorCommit,
          useFallback ? { fallback: 'before' } : undefined,
        )
          .then(f => { if (!stale()) showFact(f); })
          .catch(e => { if (!stale()) failAnchored(e, anchorCommit); });
        return;
      }
      api.getLensFact(lensName, factPath)
        .then(f => {
          if (stale()) return;
          showFact(f);
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
      reading.repo, reading.branch, factPath,
      anchorCommit ?? undefined,
      useFallback ? { fallback: 'before' } : undefined,
    )
      .then(f => {
        if (stale()) return;
        showFact(f);
      })
      .catch(e => {
        if (stale()) return;
        if (anchorCommit) failAnchored(e, anchorCommit);
        else setError(String(e));
      });
    // `resolvedDep` re-runs an anchored or diff read once SET_FACT_SOURCE makes
    // the source own the open path. A live read neither needs nor wants it: it
    // dispatched that source itself, and re-running would re-fetch the fact.
  }, [factPath, anchorCommit, reading.repo, useFallback, inDiff, lensCtx, lensName, resolvedDep]);


  // Cache the loaded fact's title so the breadcrumb labels this crumb with the
  // title we already read — instead of a separate fetch that 404s for a
  // retracted fact. Keyed identically to the breadcrumb's crumb key.
  useEffect(() => {
    if (fact?.title && factPath && factLoadedFor.current === factPath) {
      dispatch({ type: 'CACHE_FACT_TITLE', key: factTitleKey(factPath, anchorCommit ?? undefined), title: fact.title });
    }
  }, [fact, factPath, anchorCommit, dispatch]);

  // The repo-scoped write target for edits/retracts: {reading.repo, reading.branch} in
  // a repo context (unchanged); {lens.write, write-agent-branch} in a lens context.
  // The bare fact path is already write-repo-relative, so the existing
  // api.updateFact/retractFact reach the fact. Until getAgentBranch resolves we
  // fall back to reading.branch, which in a lens context IS the write repo's agent
  // branch (App's status bootstrap resolved it the same way).
  const writeTarget = (lensCtx && lensWrite)
    ? { repo: lensWrite, branch: writeBranch ?? reading.branch }
    : { repo: reading.repo, branch: reading.branch };
  // A lens fact is writable only when it lives in the lens's WRITE repo; read-mount
  // facts render fully read-only. Repo context keeps its prior gate (isReadOnly).
  const isWriteFact = !lensCtx || reading.factSource?.repo === lensWrite;
  const factReadOnly = isReadOnly(reading) || !isWriteFact;
  // Most specific reason first: a lens read-mount names where edits DO go;
  // otherwise readOnlyTitle distinguishes a subscription (live anchor, still
  // unwritable) from a stale anchor, which the bare constant could not.
  const factReadOnlyTitle = (!isWriteFact && lensWrite)
    ? `Read-only mount — edits go to ${lensWrite}`
    : readOnlyTitleFor(reading);
  const lensMeta = lensCtx && reading.factSource
    ? { repo: reading.factSource.repo, branch: reading.factSource.branch }
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

  const withCorner = (content: ReactNode) => cornerAction
    ? <div style={{ position: 'relative', height: '100%', minHeight: 0 }}>{content}<div data-testid="fact-corner-action" style={cornerBox}>{cornerAction}</div></div>
    : content;

  if (replacedBy) return <>{replacedBy}</>;

  if (noVersionAt && factPath) return withCorner(<NoVersionAtCommit commit={noVersionAt} />);
  if (error && factPath) {
    return withCorner(<div style={{ padding: 24, color: '#f44' }}>{error}</div>);
  }
  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  if (!factPath) return <>{noFact}</>;

  // Fact view (normal or time-travel)
  if (!fact) return withCorner(<div style={{ padding: 24, color: '#666' }}>Loading...</div>);

  if (fact.parse_error) return withCorner(<FactEditor fact={fact} repo={writeTarget.repo} branch={writeTarget.branch} readOnly={factReadOnly} readOnlyTitle={factReadOnlyTitle} onSaved={setFact} />);

  const readOnly = factReadOnly;

  if (mat.open) {
    const anchor = factHistoryAnchor(reading, fact.path);
    return (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        <MaterializeView
          target={{ repo: anchor.repo, branch: anchor.branch, path: anchor.path, commit: materializeCommit(anchorCommit, fact) }}
          title={fact.title || fact.path}
          options={mat.options}
          onOptionsChange={options => setMat(m => ({ ...m, options }))}
          onBack={() => setMat(m => ({ ...m, open: false }))}
          focusScope={materializeFocus}
          barAction={cornerAction}
        />
      </div>
    );
  }

  return withCorner(
    <div data-testid="fact-view" style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      {confirmRetract && (
        <ConfirmModal
          message={`Are you sure you want to retract "${fact.title || fact.path}"?`}
          onConfirm={doRetract}
          onCancel={() => setConfirmRetract(false)}
        />
      )}
      {/* No scroll here: the fact view scrolls its own body BELOW the band, so
          the band can stay put. A scroller here would move the band with it.
          It is also what clips the header's hanging panels, so it is their
          clamp bounds: they shrink to fit inside it rather than being cut. */}
      <div {...{ [PANEL_BOUNDS_ATTR]: '' }} style={{ flex: 1, minHeight: 0, overflow: 'hidden' }}>
        {renderFact(
          fact,
          // History anchor (VersionWalker) — the fact's own source mount + relative
          // path, NOT the write target. Repo context: {reading.repo, reading.branch, path}.
          factHistoryAnchor(reading, fact.path),
          dispatch,
          hideRetract ? undefined : () => { if (!readOnly) setConfirmRetract(true); },
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
          orderedMotifs,
          { open: motifOpen, onToggle: toggleMotif, onClose: closeMotif, onPivot: pivotMotif, panelId: motifPanelId },
          isLive(reading),
          () => setMat(m => ({ ...m, open: true })),
        )}
      </div>
    </div>
  );
}
