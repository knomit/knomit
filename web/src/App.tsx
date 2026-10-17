import { useReducer, useEffect, useLayoutEffect, useState, useRef, useCallback, useMemo } from 'react';
import type { Dispatch } from 'react';
import { reducer, init, isLive, selectTrail, currentPath, lensResolutionPending, remoteErrorText } from './state';
import type { Action, BrowseContext } from './state';
import { api, apiUrl, fetchVersion, repoAvailable, brokenLensMember, subscribeRepoEvents } from './api';
import type { RepoInfo, Lens, Status, RepoEvent, RepoDetails, ArchivedRepo } from './api';
import { pageview, track } from './telemetry';
import { useNavigationManager } from './useNavigationManager';
import { useFactEdges } from './useFactEdges';
import { useTimeTravel } from './useTimeTravel';
import { bootstrapStatusWithRetry } from './bootstrap';
import { useBoot } from './boot';
import { BootScreen } from './BootScreen';
import { openEventStream, streamErrorSuffix } from './eventStream';
import { adoptAPIBase, isDesktopBooting, pollBootStatus } from './bootStatus';
import { pickRepo, loadLastContext, saveLastContext, browsableRepo } from './repoSelection';
import { useRepoCreates, activeCreateByRepo, refreshRepoCreates } from './useRepoCreates';
import { useViewState, facetsFor, remoteFailing, displayedTarget, targetLabelFor, draftCancellable, serverPagesFor, railShown, FLEET_FACETS } from './facets';
import type { Facet, PinnedTarget, DraftKind, ServerPage } from './facets';
import { TopBar } from './TopBar';
import { TargetRail } from './TargetRail';
import { BrowseFacet } from './BrowseFacet';
import { ActivityFacet } from './ActivityFacet';
import { SettingsFacet } from './SettingsFacet';
import { FacetLayoutProvider } from './facetLayout';
import { ServerPages } from './ServerPages';
import { useOAuthAvailability } from './useOAuthAvailability';
import { CreateRepoWizard } from './CreateRepoWizard';
import { CreateLensForm } from './CreateLensForm';
import { CreateBlocked } from './CreateBlocked';
import { useFleet } from './useFleet';
import { FleetMembers, type FleetLanding } from './FleetMembers';
import { FleetActivity } from './FleetActivity';
import { FleetSettings } from './FleetSettings';
import { FleetWizard } from './FleetWizard';
import type { FleetStatus } from './api';
import { ErrorBoundary } from './ErrorBoundary';
import { StatusFooter } from './StatusFooter';
import { useVersion } from './hooks';
import { createOutageLog } from './streamOutage';
import './App.css';

// Library | RightPanel splitter sizing. Persisted to localStorage so the
// width survives reloads. Clamped on read + on every drag step.
const LEFT_PANEL_MIN = 180;
// Below this the Library header drops the ROOT ancestor and keeps the immediate
// parent — going up one level is the common move, and the root stays one click
// away inside the overflow menu.
const LIBRARY_NARROW_PX = 240;
const LEFT_PANEL_MAX_FRACTION = 0.6;       // never let the left panel exceed 60% of the viewport
const LEFT_PANEL_DEFAULT_FRACTION = 0.35;  // matches the previous fixed 35% width
const LEFT_PANEL_STORAGE_KEY = 'knomit.leftPanelWidth';

// How long a FINISHED task keeps the footer's task line before it retires. Long
// enough to read the outcome of something you just triggered, short enough that
// it never reads as current.
const TASK_LINGER_MS = 8_000;

// How often the remote-error banner re-checks the stored status while it is on
// screen. Nothing polls while the remote is healthy — see the recheck effect.
const REMOTE_RECHECK_MS = 60_000;

// How often the branch head is re-read as a fallback for the SSE `status`
// stream. Exported for the test that pins the behavior.
//
// The head is otherwise only ever moved by three one-shot events — the
// page-load bootstrap, a `status` event, and the post-task refresh — so a
// stream that goes quiet leaves the tab pinned to whatever it last heard, with
// no signal and no recovery short of a reload. That is not hypothetical: the
// server used to DROP a head broadcast whenever the commit landed while the
// previous notification's callback was still running (issue #178). This poll is
// the belt-and-braces half of that fix, and it covers the lossy-stream case the
// server fix cannot.
//
// It dispatches SET_HEAD, not SET_STATUS, and the distinction is the whole
// reason it is cheap: SET_HEAD returns the SAME state object for an unchanged
// head, so a quiet repo re-renders nothing, while SET_STATUS rebuilds state
// unconditionally and would re-render every open tab every 30s forever. The
// stream already carries full status; this only needs to carry the head.
export const HEAD_POLL_MS = 30_000;

function loadLeftPanelWidth(): number {
  const fallback = Math.max(LEFT_PANEL_MIN, Math.round(window.innerWidth * LEFT_PANEL_DEFAULT_FRACTION));
  try {
    const raw = localStorage.getItem(LEFT_PANEL_STORAGE_KEY);
    if (!raw) return fallback;
    const n = Number(raw);
    if (!Number.isFinite(n)) return fallback;
    return clampLeftPanelWidth(n);
  } catch {
    return fallback;
  }
}

function clampLeftPanelWidth(px: number): number {
  const max = Math.max(LEFT_PANEL_MIN, Math.floor(window.innerWidth * LEFT_PANEL_MAX_FRACTION));
  return Math.max(LEFT_PANEL_MIN, Math.min(max, Math.round(px)));
}

// resolveLens fetches the lens doc for a lens context and dispatches SET_LENS.
// On failure (e.g. the lens was deleted out from under a persisted context) it
// surfaces the error via the notice banner and falls back to the first
// available repo, so the app never strands in a broken lens context.
// Exported (with an injectable getLens) so the failure→fallback path is unit
// testable without mounting the whole App.
export async function resolveLens(
  name: string,
  fallbackRepos: RepoInfo[],
  dispatch: Dispatch<Action>,
  getLens: (name: string) => Promise<import('./api').Lens> = api.getLens,
  // I3: resolveLens is fire-and-forget, so a slow failing getLens(A) can reject
  // after the user already switched to lens B (or a repo). The SET_LENS success
  // path is reducer-guarded (it no-ops when the context drifted), but the failure
  // fallback dispatches SET_CONTEXT unconditionally — which would yank the user
  // out of B. Gate the whole fallback on the resolve still being the current lens.
  isCurrentLens: (name: string) => boolean = () => true,
): Promise<void> {
  try {
    const lens = await getLens(name);
    // The fetch SUCCEEDING is not the same as the lens being readable.
    // GET /lenses/{lens} sits outside LensMiddleware, so it answers 200 for a
    // lens whose member has no live store — and then every read endpoint under
    // it answers 503, because a lens binds all of its members or none. Without
    // this check the rescue below can never fire on that case, which is the
    // one it exists for: the user is dropped into a surface that fails on
    // arrival, from a persisted context they did not choose today.
    const broken = brokenLensMember(lens, fallbackRepos);
    if (broken !== null) throw new Error(`mount "${broken}" has no store`);
    dispatch({ type: 'SET_LENS', lens });
    // The lens response embeds its write member's branch root, so entering a
    // lens no longer needs a second hop to learn the branch. Absent when the
    // write member is a subscription (it has no agent branch) or on an older
    // server — then the ordinary bootstrap supplies it, exactly as before.
    if (lens.write_branch) dispatch(statusAction(lens.write_branch));
  } catch (err) {
    if (!isCurrentLens(name)) return; // context drifted — a newer surface owns the app
    dispatch({ type: 'SET_NOTICE', text: `Lens "${name}" is unavailable — showing a repo instead.` });
    diag('error', `[lens] ${name}: ${String(err)}`);
    // The fallback is a RESCUE, so it has to land somewhere readable. The repo
    // listing now includes registered repos with no live store, whose every
    // endpoint answers 409 — falling back onto one would swap a broken lens for
    // a broken repo. Gated here rather than trusting callers to pre-filter,
    // since this is the function that decides where the user ends up.
    const fallback = fallbackRepos.filter(browsableRepo)[0];
    if (fallback) dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: fallback.name } });
  }
}

// refreshContextAfterChange re-syncs the browse surface after a repo/lens
// mutation reported by a settings page (I4). It re-fetches both lists and:
//   - lens context, `renamed` says the active lens is the one that got
//     renamed → follow it to the new name (I5, same reasoning as the repo
//     case below) rather than treating the old name as gone;
//   - lens context, lens still exists under its current name → re-run
//     resolveLens so edited mounts refresh state.lens (reads/write/description);
//   - lens context, lens gone → fall back to the first repo with the same
//     deleted-lens notice resolveLens uses, so no dead empty library is left;
//   - repo context → keep the prior behavior: if the active repo was
//     archived/removed, switch to a remaining one (the first in the list; no
//     repo name is privileged) — UNLESS `renamed` says the active repo is the
//     one that got renamed, in which case it follows to the new name instead
//     of landing on an unrelated repo (I5: a rename is not a disappearance);
//   - no repos left at all → clear the repo context outright, whatever the
//     browse surface was. This case precedes the other two: with an empty list
//     there is nothing to fall back TO, and leaving state.repo on the archived
//     repo is what kept its event stream alive.
// "Remaining"/"left" throughout means READABLE, not listed: a registered repo
// with no live store is in the list on purpose but cannot be browsed.
// Returns the fetched lists so the caller can update its local component state.
export async function refreshContextAfterChange(
  dispatch: Dispatch<Action>,
  context: BrowseContext,
  currentRepo: string,
  deps: {
    listLenses?: () => Promise<Lens[]>;
    repos?: () => Promise<RepoInfo[]>;
    getLens?: (name: string) => Promise<Lens>;
  } = {},
  isCurrentLens: (name: string) => boolean = () => true,
  // Set when the mutation that triggered this call WAS a rename. Lets the
  // repo-context branch below distinguish "the active repo is gone" (jump to
  // whatever is left) from "the active repo is still here, just renamed"
  // (follow it) — the Danger zone's `onChanged({from, to})`.
  renamed?: { from: string; to: string },
): Promise<{ lenses: Lens[]; repos: RepoInfo[] }> {
  const listLenses = deps.listLenses ?? api.listLenses;
  const listRepos = deps.repos ?? api.repos;
  const getLens = deps.getLens ?? api.getLens;
  const [lenses, repoList] = await Promise.all([listLenses(), listRepos()]);
  // Everywhere below chooses where the BROWSE surface points, so it works from
  // the readable repos, not the listed ones. The listing carries registered
  // repos with no live store — visible on purpose, since they used to vanish
  // silently — but every endpoint under one answers 409, so pointing the app at
  // it would render a repo whose content is a stack of errors. This is the same
  // rule pickRepo applies; it lives in both because this path never calls that
  // one. `repoList` is still what the function RETURNS: the caller's list is a
  // list, and the rail has to keep showing what is registered.
  const readable = repoList.filter(browsableRepo);
  if (readable.length === 0) {
    // Nothing left to browse. Clearing the repo is not cosmetic: state.repo and
    // state.branch key the SSE subscription, and leaving them pointed at the
    // repo that was just archived holds an EventSource open on a route that now
    // 404s — which EventSource retries roughly every 3s, forever, because
    // nothing else re-runs that effect until a repo exists again. SET_REPO ''
    // clears both, and every per-repo effect is guarded on one or the other.
    if (currentRepo) dispatch({ type: 'SET_REPO', repo: '' });
    return { lenses, repos: repoList };
  }
  if (context.kind === 'lens') {
    // A rename is not the lens vanishing: if this is the same lens under its
    // new name, follow it there rather than treating the old name as deleted
    // (I5, mirrored from the repo-rename handling below). Only SET_CONTEXT is
    // dispatched here — resolution is owned by the lensResolutionPending
    // effect, the same "single owner" rule every other lens-context entry
    // follows (a rail pick, a Mounted-in link, bootstrap restore); entering
    // the new context clears state.lens, which is what makes that effect fire.
    if (renamed && renamed.from === context.name && lenses.some(l => l.name === renamed.to)) {
      dispatch({ type: 'SET_CONTEXT', context: { kind: 'lens', name: renamed.to } });
    } else if (lenses.some(l => l.name === context.name)) {
      // `repoList`, NOT `readable`. resolveLens's readability check
      // (brokenLensMember) counts only POSITIVE evidence — a member the listing
      // carries in a broken state — so handing it a list with the broken repos
      // already filtered out guarantees the check finds nothing and the lens is
      // accepted, dropping the user onto a surface whose every read answers 503.
      // resolveLens re-filters for its own fallback, so the full listing is the
      // right input for both halves of what it does.
      await resolveLens(context.name, repoList, dispatch, getLens, isCurrentLens);
    } else {
      dispatch({ type: 'SET_NOTICE', text: `Lens "${context.name}" is unavailable — showing a repo instead.` });
      const fallback = readable[0];
      if (fallback) dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: fallback.name } });
    }
    // A repo that is merely LISTED is not one the app can browse: the current
    // repo going unavailable across a server restart has to move the surface,
    // exactly as an archived one does.
  } else if (!readable.some(r => r.name === currentRepo)) {
    // A rename is not the active repo vanishing: if this is the same repo
    // under its new name, follow it there rather than landing on whichever
    // unrelated repo happens to sort first.
    const to = renamed && renamed.from === currentRepo && readable.some(r => r.name === renamed.to)
      ? renamed.to
      : readable[0].name;
    dispatch({ type: 'SET_REPO', repo: to });
  }
  return { lenses, repos: repoList };
}

/**
 * diag reports a condition that has no place in the UI but must not vanish.
 *
 * These lines used to go to an in-app console panel. The panel is gone: none of
 * them was ever addressed to a user (SSE retry chatter, a bootstrap attempt that
 * will retry itself, a background status refresh that failed), and a panel whose
 * every line is developer diagnostics is a panel nobody opens. Anything a USER
 * must act on goes to SET_NOTICE or a banner instead — see resolveLens, which
 * does both.
 *
 * The browser console is the honest destination for the remainder: reachable
 * when debugging a report of "the head pill went stale", invisible otherwise.
 * Deleting them outright was the alternative, and it would have made the one
 * failure this app cannot otherwise show — an SSE stream that silently stopped
 * — completely undiagnosable.
 */
function diag(level: 'info' | 'error', message: string): void {
  if (level === 'error') console.error(message);
  else console.info(message);
}

/**
 * statusAction turns a branch-status response into the SET_STATUS action.
 *
 * Three call sites read that endpoint — the bootstrap, the indexing poll, and
 * the post-task refresh — and every one of them must apply the WHOLE payload.
 * A refresh that cherry-picks the head silently drops index_state, which is how
 * an index error survived the rebuild that fixed it.
 */
function statusAction(s: Status): Action {
  return {
    type: 'SET_STATUS',
    head: s.head,
    branch: s.branch,
    embeddingsEnabled: s.embeddings_enabled,
    ontologyRoot: s.ontology_root,
    indexState: s.index_state,
    indexDone: s.index_done,
    indexTotal: s.index_total,
    indexPercent: s.index_percent,
    branchWritable: s.writable,
    experiment: s.experiment ?? null,
  };
}

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);
  // The view state: which page of the shell is on screen. Its own reducer and
  // NOT AppState, because none of it is a way of looking at facts (see
  // facets.ts): whether the rail is open, the facet, the one-shot focus and
  // the connect lock. useViewState persists knomit.rail and knomit.facet.
  const [view, dispatchView] = useViewState();
  // Raised by ⌘K to ask the rail's search for focus, and cleared by the rail
  // once it has focused — so a rail that mounts BECAUSE of the ⌘K focuses on
  // arrival, and one that mounts later (the toggle, ⌘B) does not
  // re-serve a request that was already handled.
  const [railFocusRequest, setRailFocusRequest] = useState(0);
  const clearRailFocusRequest = useCallback(() => setRailFocusRequest(0), []);
  const toggleRail = useCallback(() => dispatchView({ type: 'TOGGLE_RAIL' }), [dispatchView]);
  // Latest-state ref for async callbacks that resolve after the context may have
  // drifted (resolveLens fallback guard I3, onChanged refresh I4). Reads the
  // committed context at resolution time, not the stale closure value.
  const stateRef = useRef(state);
  stateRef.current = state;
  // Stable identity (reads only the ref) so callbacks that close over it can be
  // memoized without re-creating on every render.
  const isCurrentLens = useCallback((name: string) =>
    stateRef.current.context.kind === 'lens' && stateRef.current.context.name === name, []);
  const { navigate } = useNavigationManager(state, dispatch);
  // DESKTOP ONLY, and declared FIRST because almost everything below is gated
  // on it. True when this page loaded while knomit-desktop was still booting
  // its server, which on a first launch means a multi-minute model download
  // with no API in existence.
  //
  // NOTHING may leave the page while this is true. There is no API base yet, so
  // a request resolves against the webview origin, where the desktop's SPA
  // fallback rewrites to /index.html and http.FileServer 301s that to "./" —
  // which for any NESTED path (every /api/v1/... there is) resolves to the same
  // parent and redirects again, until the client gives up. Measured: a Go
  // client stops after 10 hops. The request does not fail in a way any of these
  // best-effort catches can distinguish from "the server said no", so the
  // damage is silent and permanent for the session: an EventSource fixes its
  // URL at construction and never re-resolves, and a one-shot useEffect([])
  // never runs again.
  //
  // Initialised from the flag /config.js set, so the very first render already
  // knows and no effect can win the race. In the browser the flag is never set,
  // this is false forever, and every gate below is a no-op.
  const [serverBooting, setServerBooting] = useState(isDesktopBooting);
  const version = useVersion(!serverBooting);
  const [repos, setRepos] = useState<RepoInfo[]>([]);
  const [lenses, setLenses] = useState<Lens[]>([]);
  const [reposLoaded, setReposLoaded] = useState(false);
  // The archived repos: the rail's Archived group, and the pages pinned from
  // it. Read on boot and after every settings mutation, like the other lists.
  const [archived, setArchived] = useState<ArchivedRepo[]>([]);
  const [archivedLoaded, setArchivedLoaded] = useState(false);
  // Boot progress, so the pre-branch screen can say what it is waiting on
  // instead of showing an undifferentiated "Loading…".
  const { boot, dispatchBoot, retryBoot } = useBoot(isDesktopBooting() ? 'server' : 'connecting');
  // bootNonce re-fires the bootstrap effect when the user presses Retry; the
  // repo has not changed, so nothing else would.
  const [bootNonce, setBootNonce] = useState(0);
  // The speculative GET /repos/{remembered} fired in the same tick as the
  // repo list. Set only once the list CONFIRMS that repo, and consumed once by
  // the bootstrap — a later context switch must ask the server again.
  const speculativeRef = useRef<{ repo: string; details: RepoDetails } | null>(null);
  // The boot response for the current repo. `mode` on it is what the read-only
  // effect reads instead of issuing its own GET during boot.
  const [bootDetails, setBootDetails] = useState<{ repo: string; details: RepoDetails } | null>(null);
  // Retry re-runs the boot from the top: reset the phase/attempt/error, then
  // bump the nonce so the bootstrap effect fires again for the same repo.
  const onBootRetry = useCallback(() => {
    retryBoot();
    setBootNonce(n => n + 1);
  }, [retryBoot]);
  // The create still working on the repo being browsed, if any.
  //
  // A create's repo becomes listable and browsable at m.Add, well before the
  // job behind it is finished — and if the user has cancelled, before it is
  // deleted. Until this, browsing such a repo showed the ordinary "Indexing…"
  // banner, which is true of the index and silent about the thing that
  // matters: that this repository is still being made, or is being removed.
  // The poller is already running app-wide for the top bar's CreateIndicator,
  // so reading it here costs no extra requests.
  const creates = useRepoCreates();
  const repoCreate = activeCreateByRepo(creates).get(state.repo ?? '');
  // Raised while a connect commit is in flight in the Settings facet's
  // RepoSettings. Leaving the page unmounts the wizard, and the commit stream
  // has no abort and no undo: the store swap and index rebuild run on with
  // nothing listening for the result. So while it is up every app-level exit
  // holds: rail rows and `+`, facet tabs, the gear. It is the view's `locked`.
  const setLocked = useCallback((locked: boolean) => dispatchView({ type: 'SET_LOCKED', locked }), [dispatchView]);

  // Time-travel callbacks (scrub / hop / open-at / return-to-now), backed by
  // the reducer. RightPanel + LeftPanel + FilterBar all route their
  // navigation through these so a single action model drives now and history.
  const tt = useTimeTravel(state, dispatch);

  // The open fact's edges, fetched ONCE here and handed to every consumer.
  // RightPanel (in-body ref pins) and the connections panel used to issue the
  // same api.explain call independently, for the same fact at the same anchor.
  // The anchor rules — which mount, which commit, when to fall back — moved
  // into the hook with the fetch; see useFactEdges.
  // A live edges 404 means this tab's cached head is stale; the hook re-reads
  // the head to recover and hands it back here so the whole app stops being
  // stale, not just the fetch that noticed.
  const applyDiscoveredHead = useCallback((head: string) => dispatch({ type: 'SET_HEAD', head }), []);
  const edges = useFactEdges(state, applyDiscoveredHead);


  // 12-hex KB-store id → repo name, for the References labels in FactBody. The
  // repo list already carries both, so a kb://<id>/… ref to another MOUNTED
  // repo can render that repo's name instead of its hash. A src:// ref's id is
  // the SOURCE repo's root commit — a different namespace that will never match
  // here, and is deliberately left as-is (see refLabel).
  //
  // Memoized on `repos` so RightPanel's memo still absorbs unrelated renders.
  const repoNames = useMemo(() => {
    const m: Record<string, string> = {};
    for (const r of repos) if (r.id) m[r.id.toLowerCase()] = r.name;
    return m;
  }, [repos]);

  // Splitter between Library (left) and RightPanel. Width restored from
  // localStorage on mount; persisted on drag-end so transient frames during a
  // drag don't thrash localStorage.
  const [leftPanelWidth, setLeftPanelWidth] = useState<number>(() => loadLeftPanelWidth());

  // Crossing the narrow threshold is the ONLY thing the library needs to know
  // about the splitter. Derived as a boolean here so a drag re-renders LeftPanel
  // at most once (when it crosses) instead of on every frame — see the narrow
  // prop, and App.resilience.test.tsx's splitter assertion.
  const libraryNarrow = leftPanelWidth < LIBRARY_NARROW_PX;

  // Re-clamp on viewport shrink so the right panel can't disappear.
  useEffect(() => {
    const onResize = () => setLeftPanelWidth(w => clampLeftPanelWidth(w));
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);
  const startSplitterDrag = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startWidth = leftPanelWidth;
    const onMove = (ev: MouseEvent) => {
      setLeftPanelWidth(clampLeftPanelWidth(startWidth + (ev.clientX - startX)));
    };
    const onUp = () => {
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
      // Read the latest committed width via setState's functional form so we
      // don't capture a stale value if React batched the final move.
      setLeftPanelWidth(w => {
        try { localStorage.setItem(LEFT_PANEL_STORAGE_KEY, String(w)); } catch { /* quota / disabled */ }
        return w;
      });
    };
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  }, [leftPanelWidth]);
  // Every facet column lines up with the Library: one width, one handler.
  const facetLayout = useMemo(() => ({ width: leftPanelWidth, onSplitterDown: startSplitterDrag }), [leftPanelWidth, startSplitterDrag]);

  // Fetch the repo list on mount and select which repo to display. The repo
  // set is owned by the server, which privileges no name and may serve none at
  // all — a fresh knomit starts with zero repos. pickRepo derives the selection
  // from the live list, preferring the user's last explicit choice and falling
  // back to the first available repo. reposLoaded gates the "no repos" empty
  // state below, which is the ordinary first-run screen, not an error.
  // DESKTOP ONLY: wait out the desktop's own server boot before asking the API
  // anything, narrating it meanwhile. /boot/status is served by the desktop
  // process itself, so it answers during exactly the window when the knomit
  // server does not yet exist. See bootStatus.ts.
  useEffect(() => {
    if (!serverBooting) return;
    let cancelled = false;
    void pollBootStatus({
      shouldStop: () => cancelled,
      onStatus: (s) => {
        if (s.error) {
          dispatchBoot({ type: 'FAILED', error: s.error });
          return;
        }
        if (s.ready) {
          // Order matters: install the base BEFORE clearing the gate, or the
          // repo effect can fire in the same commit with no base to use.
          adoptAPIBase(s.api_base ?? '');
          dispatchBoot({ type: 'PHASE', phase: 'connecting' });
          setServerBooting(false);
          return;
        }
        dispatchBoot({ type: 'SERVER_PHASE', serverPhase: s.phase });
      },
    });
    return () => { cancelled = true; };
  }, [serverBooting, dispatchBoot]);

  useEffect(() => {
    // Held until the desktop's server is up. Re-runs when that flips, which is
    // why serverBooting is in the dependency list below.
    if (serverBooting) return;
    let cancelled = false;

    // The remembered context is read SYNCHRONOUSLY, before any request, so the
    // repo it names can be fetched in the SAME TICK as the list instead of
    // waiting a full round trip for it. localStorage is synchronous; there was
    // never a reason to serialise these.
    //
    // It is SPECULATIVE: the list is what decides. If pickRepo confirms the
    // same repo the result is used as-is; if it picks a different one (the
    // remembered repo was archived, or the speculative call 404s) the result
    // is discarded. Nothing here may dispatch SET_STATUS for a repo the list
    // has not confirmed.
    const last = loadLastContext();
    const speculativeRepo = last?.kind === 'repo' && last.repo ? last.repo : '';
    const speculative: Promise<RepoDetails | null> = speculativeRepo
      ? api.getRepo(speculativeRepo).catch(() => null)
      : Promise.resolve(null);

    api.repos()
      .then(async list => {
        if (cancelled) return;
        setRepos(list);
        setReposLoaded(true);
        // Restore the last browse context. A persisted lens is entered
        // immediately, then resolved (falling back to a repo if it's gone). A
        // repo (or no) context picks a repo from the live list as before.
        if (last?.kind === 'lens') {
          // Resolution is owned by the lensResolutionPending effect below.
          dispatchBoot({ type: 'PHASE', phase: 'opening', target: last.name });
          dispatch({ type: 'SET_CONTEXT', context: last });
          return;
        }
        const next = pickRepo('', list, last?.kind === 'repo' ? last.repo : null);
        if (!next) return;
        dispatchBoot({ type: 'PHASE', phase: 'opening', target: next });
        // Hand the speculative response to the bootstrap ONLY when the list
        // confirmed the same repo.
        if (next === speculativeRepo) {
          const details = await speculative;
          if (cancelled) return;
          if (details) speculativeRef.current = { repo: next, details };
        }
        dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: next } });
      })
      .catch(() => { if (!cancelled) setReposLoaded(true); });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [serverBooting]);

  // Boot is over the moment a branch is known — that is choice (a): the panels
  // below keep their own placeholders for the wave that follows, rather than
  // holding the whole screen hostage to it. Driven off state.branch rather
  // than the bootstrap's own `done`, because the lens path reaches a branch
  // through SET_LENS and never calls the bootstrap at all.
  useEffect(() => {
    if (state.branch) dispatchBoot({ type: 'PHASE', phase: 'done' });
  }, [state.branch, dispatchBoot]);

  // Lens resolution — single owner. Every surface that enters a lens context
  // (a rail pick, a Mounted-in link, bootstrap restore) only dispatches
  // SET_CONTEXT; this effect notices a context whose lens doc isn't resolved
  // yet (lensResolutionPending) and fetches it, falling back to a repo if the
  // lens is gone. Gated on reposLoaded so the fallback list is real; the
  // SET_LENS reducer guard + isCurrentLens keep late/stale resolutions inert.
  // Same-name re-resolution after an edit goes through refreshContextAfterChange.
  useEffect(() => {
    if (!reposLoaded || !lensResolutionPending(state)) return;
    if (state.context.kind !== 'lens') return; // narrows the type; implied by the check above
    void resolveLens(state.context.name, repos, dispatch, api.getLens, isCurrentLens);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.context, state.lens, repos, reposLoaded]);

  // Fetch the lens and archived lists on mount for the rail's Lenses section
  // and Archived group. Owned here (like repos) and threaded down; refreshed
  // after every settings mutation. Best-effort: an empty list just shows none.
  useEffect(() => {
    if (serverBooting) return;
    let cancelled = false;
    api.listLenses()
      .then(list => { if (!cancelled) setLenses(list); })
      .catch(() => { /* best-effort: no lenses on failure */ });
    api.listArchived()
      .then(list => { if (!cancelled) { setArchived(list); setArchivedLoaded(true); } })
      .catch(() => { /* best-effort: no Archived group on failure */ });
    return () => { cancelled = true; };
  }, [serverBooting]);

  // Fetch server read-only flag once the server exists and propagate to global
  // state. Gated like the rest: failing early would silently leave the app
  // WRITABLE on a read-only server, because the catch treats any failure as
  // "stay writable".
  useEffect(() => {
    if (serverBooting) return;
    let alive = true;
    fetchVersion()
      .then(v => { if (alive) dispatch({ type: 'SET_SERVER_READONLY', value: v.readOnly }); })
      .catch(() => { /* best-effort: stay writable on failure */ });
    return () => { alive = false; };
  }, [serverBooting]);

  // The browsed repo's own read-only state (a subscription). Lens browsing
  // writes to the lens's write repo, which can never be a subscription
  // (validateLensLocked refuses one), so the flag is cleared there.
  useEffect(() => {
    if (state.context.kind !== 'repo' || !state.repo) { dispatch({ type: 'SET_REPO_READONLY', value: false }); return; }
    // `mode` is on the boot response this repo already fetched, so read it
    // from there rather than racing a second GET against the bootstrap.
    if (bootDetails?.repo === state.repo) {
      dispatch({ type: 'SET_REPO_READONLY', value: bootDetails.details.mode === 'subscribe' });
      return;
    }
    // No boot response for THIS repo yet. If one is still in flight, waiting
    // costs nothing and the effect re-runs when it lands; the pre-existing
    // default is writable, which is also the failure default below.
    if (!bootDetails) return;
    // A later context switch: the boot response belongs to a different repo.
    let alive = true;
    api.getRepo(state.repo)
      .then(d => { if (alive) dispatch({ type: 'SET_REPO_READONLY', value: d.mode === 'subscribe' }); })
      .catch(() => { /* best-effort: stay writable on failure */ });
    return () => { alive = false; };
  }, [state.context, state.repo, bootDetails]);

  // Remember the user's browse context (repo | lens) so reloads land on the
  // same surface.
  useEffect(() => {
    saveLastContext(state.context);
  }, [state.context]);

  // --- Anonymous usage telemetry. Every call below is a no-op unless a host
  // (the public demo's reverse proxy) defined window.knomitTelemetry; stock
  // builds emit nothing. See web/src/telemetry.ts.

  // Page views on SPA navigation: the open fact, else the current directory.
  const navPath = state.factPath ?? currentPath(state);
  useEffect(() => {
    pageview(navPath);
  }, [navPath]);

  // Opening a fact (factPath transitions to a new non-empty value).
  const prevFact = useRef<string | null>(null);
  useEffect(() => {
    if (state.factPath && state.factPath !== prevFact.current) {
      track('fact_opened');
    }
    prevFact.current = state.factPath;
  }, [state.factPath]);

  // Repo switches (skip the initial empty -> first-repo assignment on load).
  const prevRepo = useRef(state.repo);
  useEffect(() => {
    if (state.repo && prevRepo.current && state.repo !== prevRepo.current) {
      track('repo_switched');
    }
    prevRepo.current = state.repo;
  }, [state.repo]);

  // First transition from live into a time-travel (history/diff) anchor.
  const wasLive = useRef(true);
  useEffect(() => {
    const live = isLive(state);
    if (wasLive.current && !live) track('time_travel_used');
    wasLive.current = live;
  }, [state.asOf]); // eslint-disable-line react-hooks/exhaustive-deps

  // Load status when repo changes (also fires on mount). Bootstrap fetches the
  // agent branch then the branch root for full status, retrying with
  // exponential backoff: a single transient hiccup (dev proxy first-request
  // hang, brief network blip, backend just-restarted) would otherwise leave
  // the page stuck on "Loading…" because this effect only re-fires when
  // state.repo changes. Each failed attempt is logged to the Console so a
  // permanently broken backend is visible instead of silent.
  useEffect(() => {
    if (!state.repo) return; // wait until a repo is selected from the server list
    let cancelled = false;
    const repo = state.repo;

    // getRepo for the boot, which reuses the speculative response when the
    // list confirmed the same repo — that is what makes a warm boot ONE
    // request. Consumed once: a later context switch is a different question
    // and must ask the server again.
    const getRepo = async (name: string): Promise<RepoDetails> => {
      const spec = speculativeRef.current;
      speculativeRef.current = null;
      const details = spec && spec.repo === name ? spec.details : await api.getRepo(name);
      if (!cancelled) setBootDetails({ repo: name, details });
      return details;
    };

    bootstrapStatusWithRetry({
      repo,
      initialBranch: state.branch,
      getRepo,
      getStatus: api.status,
      onSuccess: (s) => { dispatch(statusAction(s)); },
      onPhase: (phase) => { dispatchBoot({ type: 'PHASE', phase, target: repo }); },
      onAttemptFailed: (err, attempt) => {
        diag('error', `[bootstrap] attempt ${attempt + 1} failed: ${String(err)}`);
        // The same text the Console got, now also where the user is looking.
        dispatchBoot({ type: 'ATTEMPT_FAILED', error: String(err) });
      },
      shouldStop: () => cancelled,
    });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.repo, bootNonce]);

  // The indexing banner and every repo's index chip follow the index-event
  // stream — see the subscription below. There used to be a 2 s status poll
  // here, because no commit fires during a background heal so the `status`
  // event never refreshed it; the event now carries exactly that transition,
  // and a poll that exists only to notice a missing event is the thing this
  // replaces. The reconnect path (refetch on stream open) is the fallback.

  // Re-read the branch head on a slow cycle. See HEAD_POLL_MS: this is the
  // fallback for a `status` event that never arrives, whether because the
  // server dropped it or because the stream is dead.
  useEffect(() => {
    if (!state.repo || !state.branch) return;
    let cancelled = false;
    const id = setInterval(() => {
      // What the head was when this request went out. Nothing orders a poll
      // response against the SSE stream, so a response captured before a newer
      // `status` event can land after it — and writing it back would re-stale
      // the tab for another full interval, which is the opposite of this
      // effect's purpose. If anything moved the head while we were in flight,
      // that source is fresher than this answer by construction; drop it.
      const issuedAt = stateRef.current.headCommit;
      api.status(state.repo, state.branch)
        .then(s => {
          if (cancelled || stateRef.current.headCommit !== issuedAt) return;
          // Advance to the INDEXED head, not raw git HEAD. The two differ, and
          // only during the window this poll is most likely to fire in. The
          // branch root reports `head` straight off Branches().HeadCommit,
          // while the SSE `status` broadcast is emitted only AFTER the commit
          // observer's IndexManager().SyncLocked returns — so every head the
          // stream delivers is one the index has already absorbed, and this
          // one is not. Writing an un-indexed head back mid-sync points every
          // consumer keyed on state.headCommit (Library's search/chrono
          // effects, useFactEdges, RightPanel) at a half-built index; worse,
          // SET_HEAD short-circuits on an unchanged hash, so the post-sync
          // broadcast of that SAME hash is a no-op and the stale results stand
          // indefinitely rather than for the length of the sync. index_commit
          // is the sync watermark — the newest commit the index has absorbed —
          // which is exactly what the stream broadcasts.
          //
          // Fallback when the watermark is unavailable (SyncWatermark errored,
          // or the branch has never been indexed) is the raw head: it reports
          // "" then, and this poll is the recovery path for a dead or lossy
          // stream, so refusing to move the head would wedge the tab for as
          // long as that condition lasts — the failure this effect exists to
          // prevent. Preferring the watermark is a correctness win when it is
          // there; its absence must not cost us the recovery.
          //
          // An empty head is not an answer either way. defaultBranchRootReader
          // discards WithRead's error, and WithRead returns the Acquire error
          // without running the closure, so during a store swap or open the
          // branch endpoint answers 200 with head "". Same guard the SSE
          // `status` handler applies.
          const head = s.index_commit || s.head;
          if (!head) return;
          dispatch({ type: 'SET_HEAD', head });
        })
        .catch(() => {
          // Best-effort. A failed poll tells us nothing new about the head, and
          // the stream (or the next tick) is still the primary path — logging
          // it would put a line in the console every 30s for an outage the SSE
          // handler already reports.
        });
    }, HEAD_POLL_MS);
    return () => { cancelled = true; clearInterval(id); };
  }, [state.repo, state.branch]);

  // Reconcile the remote-error banner with the PERSISTED remote status.
  //
  // This is the only thing that reads that status back, and it must work in
  // BOTH directions. Raising it shows a failure that happened while the app was
  // closed (an expired token, say) without waiting for the next reconcile tick.
  // Lowering it is what keeps the banner honest: it is otherwise cleared only by
  // a clean remote event, so any client that missed that one event — asleep,
  // stream dropped, opened after the fact — kept a banner up for a failure the
  // server had long since recorded as "ok". That is the sticky banner a
  // long-lived desktop window used to show for hours after a blip had healed.
  const syncRemoteError = useCallback((repo: string, alive: () => boolean = () => true) => {
    if (!repo) return;
    api.getOrigin(repo).then(o => {
      if (!alive()) return;
      // Each side is applied from its OWN column, independently — this read is
      // the wholesale form of what the sync_*/push_* events do incrementally,
      // and the two must agree or the banner flaps between them. A repo with NO
      // remote (204 → null) cannot be out of sync on either side, so a banner
      // left over from before it was disconnected is stale too.
      const errorOn = (status?: string | null, message?: string | null) =>
        status === 'error' ? (message || 'remote sync failed') : '';
      dispatch({ type: 'SET_REMOTE_ERROR', side: 'sync', error: o ? errorOn(o.last_status, o.last_error) : '' });
      dispatch({ type: 'SET_REMOTE_ERROR', side: 'push', error: o ? errorOn(o.last_push_status, o.last_push_error) : '' });
    }).catch(() => {
      // The read itself failed, so we learned nothing about the remote. Leave
      // the banner as it is rather than clearing a real failure on the strength
      // of an unrelated hiccup.
    });
  }, [dispatch]);

  // Server-wide index events. ONE stream for every repo, held for the app's
  // whole life — unlike the branch stream below, which is scoped to the active
  // repo and so can only ever speak for one chip.
  //
  // The generation guard is load-bearing: the refetch below is async, and an
  // event can land while it is in flight. Without the guard a refetch dispatched
  // before an event could resolve after it and reinstate the state the event
  // had just corrected — the stale "indexing" chip, restored by the very
  // mechanism meant to clear it.
  const repoListGen = useRef(0);
  useEffect(() => {
    // Not before the API exists. This one is the worst of the gated calls to
    // get wrong: an EventSource resolves its URL ONCE, at construction, and
    // this effect is mounted once — so a stream opened against the webview
    // origin stays pointed there for the whole session. The visible cost is
    // that index chips freeze in whatever state the first /repos carried and
    // the `reconnect` refetch below never fires, with nothing on screen to say
    // the app stopped listening.
    if (serverBooting) return;
    const stop = subscribeRepoEvents((ev: RepoEvent) => {
      if ('type' in ev && ev.type === 'remote') {
        // A remote-health EDGE for one repo: replace that row's `remote` with
        // the payload, which is the row's whole new value. Nothing else: the
        // remote banner belongs to the per-branch stream's sync_*/push_*
        // events (which carry the error text) for the active repo, and is
        // re-read from GET /origin by syncRemoteError. Like an index event it
        // is newer than any list refetch in flight, so it retires them.
        repoListGen.current += 1;
        const remote = { sync: ev.sync, push: ev.push };
        setRepos(prev => prev.map(r => r.name === ev.repo ? { ...r, remote } : r));
        return;
      }
      if ('type' in ev) {
        // Reconnected: a terminal event is broadcast once and never replayed,
        // so anything that healed while we were disconnected is unrepresented.
        // Re-read the list once.
        const gen = ++repoListGen.current;
        api.repos()
          .then(list => { if (gen === repoListGen.current) setRepos(list); })
          .catch(() => {});
        return;
      }
      // An event is NEWER than any refetch in flight, so it retires them.
      repoListGen.current += 1;
      setRepos(prev => prev.map(r => r.name === ev.repo
        ? { ...r, index_state: ev.state, index_done: ev.done, index_total: ev.total, index_reason: ev.reason }
        : r));
      // The banner speaks for the ACTIVE repo only, and this stream carries
      // every repo — so it is filtered here rather than at the source.
      //
      // Read through stateRef, NOT from a closure over state.repo: this stream
      // is server-wide and must stay open for the app's whole life. Depending
      // on the active repo would tear it down and reopen it on every repo
      // switch — needless reconnects, and each one triggers the refetch above.
      if (ev.repo === stateRef.current.repo) {
        dispatch({ type: 'SET_INDEX', indexState: ev.state, indexDone: ev.done, indexTotal: ev.total });
      }
    });
    return stop;
    // Mounted once PER SERVER: dispatch is stable and the active repo is read
    // from the ref, so serverBooting flipping is the only thing that re-runs
    // this — which is exactly when the stream must finally be opened.
  }, [dispatch, serverBooting]);

  // SSE for task and status events — reconnects when repo/branch changes.
  useEffect(() => {
    // Both guards matter: branch waits for the status bootstrap, and repo goes
    // empty when the last repo is archived — subscribing to either would open a
    // stream on a URL that cannot resolve.
    if (!state.repo || !state.branch) return;
    const es = openEventStream(apiUrl(`/api/v1/repos/${state.repo}/branches/${state.branch.replaceAll('/', ':')}/events`));
    // EventSource silently auto-reconnects on disconnect. Without the error
    // handler below, a backend that 500s the stream produces a stale
    // LIVE/HISTORY pill (no SET_HEAD updates arrive) with no signal to the
    // user. See streamOutage.ts for why the reporting is flap-limited, and why
    // that reasoning now lives in one place rather than in each stream.
    const outage = createOutageLog(diag);
    es.addEventListener('open', () => {
      // A gap in the stream is a gap in the remote events, and the one that
      // clears the banner (sync_ok / push_ok) is broadcast once, never
      // replayed — so a failure that healed while we were disconnected would
      // otherwise leave the banner standing. Re-read the stored status.
      // Riding on the same condition as the log line keeps this off the hot
      // path of a flapping stream, which must not also become a request storm
      // against the origin endpoint.
      if (outage.recovered('[events] reconnected')) syncRemoteError(state.repo);
    });
    es.addEventListener('error', (e) => {
      outage.lost(es.readyState === EventSource.CLOSED
        ? `[events] stream closed${streamErrorSuffix(e)} — head pill may be stale`
        : '[events] connection lost — retrying');
    });
    es.addEventListener('task', (e) => {
      const ev = JSON.parse(e.data);
      dispatch({ type: 'SET_TASK', op: ev.op, status: ev.status, message: ev.message || '' });
      const repo = ev.repo ? `${ev.repo}/` : '';
      diag(ev.status === 'error' ? 'error' : 'info', `[${repo}${ev.op}] ${ev.message || ev.status}`);
      // Refresh the branch status when a task completes. The WHOLE payload is
      // applied, not just the head: a task like a rebuild changes index_state
      // too, and keeping only the head meant the index banner kept reporting a
      // failure the task had just repaired.
      if (ev.status === 'done' || ev.status === 'error') {
        api.status(state.repo, state.branch)
          .then(s => dispatch(statusAction(s)))
          .catch(err => diag('error', `[status] refresh failed: ${String(err)}`));
      }
    });
    es.addEventListener('status', (e) => {
      const s = JSON.parse(e.data);
      if (s.head) dispatch({ type: 'SET_HEAD', head: s.head });
    });
    const handleRemoteEvent = (e: MessageEvent) => {
      const ev = JSON.parse(e.data);
      // Every one of these events speaks for ONE half of the remote: sync_* for
      // the fetch/reconcile half, push_* for the push half, each mirroring the
      // column Sync/Push just wrote. Scoping the update by side makes them the
      // incremental form of the same truth syncRemoteError reads wholesale, so
      // the two can never contradict each other. Clearing both on any clean
      // event is what let a sync_ok lower a banner a failing push had raised.
      const side: 'sync' | 'push' = e.type.startsWith('push') ? 'push' : 'sync';
      if (ev.error) {
        dispatch({ type: 'SET_REMOTE_ERROR', side, error: ev.error });
        diag('error', `[remote] ${ev.error}`);
        return;
      }
      dispatch({ type: 'SET_REMOTE_ERROR', side, error: '' });
      // Sync events now carry structured Main + Agent reconcile detail.
      // Surface a human-readable summary in the console so users can see
      // *what* changed on each side of the reconcile.
      if (e.type === 'sync_ok' && (ev.main || ev.agent)) {
        const parts: string[] = [];
        switch (ev.main?.mode) {
          case 'ff':
            parts.push('main fast-forwarded');
            break;
          case 'rewound':
            parts.push('main rewound');
            break;
        }
        switch (ev.agent?.mode) {
          case 'merge':
            parts.push('main merged into agent');
            break;
          case 'ff':
            parts.push('agent fast-forwarded to main');
            break;
          case 'rebase':
            parts.push(`${ev.agent.num_replayed ?? 0} commit(s) replayed onto agent (rewind)`);
            break;
          case 'noop':
            // No agent change — only emit a line if main changed.
            break;
        }
        if (parts.length) {
          diag('info', `[remote] ${parts.join(', ')}`);
        }
      }
    };
    es.addEventListener('sync_ok', handleRemoteEvent);
    es.addEventListener('sync_error', handleRemoteEvent);
    es.addEventListener('push_ok', handleRemoteEvent);
    es.addEventListener('push_error', handleRemoteEvent);
    return () => es.close();
  }, [state.repo, state.branch, syncRemoteError]);

  useEffect(() => {
    let cancelled = false;
    syncRemoteError(state.repo, () => !cancelled);
    return () => { cancelled = true; };
  }, [state.repo, syncRemoteError]);

  // While the banner is UP, re-check on a timer. Every other way it comes down
  // is edge-triggered — a remote event, a reconnect, a repo switch, leaving
  // Settings — and none of those fire for a stream that stalls SILENTLY (a slept
  // laptop, a half-open proxy connection): the browser reports no 'error' and
  // no 'open', so nothing prompts a re-read. Sync and push events also have no
  // reconnect replay — TaskHub.Subscribe's snapshot carries task events only —
  // so a missed one is missed for good.
  //
  // This is the backstop that makes the banner self-healing whatever the stream
  // does, and it is deliberately conditional: a healthy remote polls nothing at
  // all. The cost is one request a minute, only while a failure is on screen,
  // and the benefit is that a stale banner cannot outlive its failure by more
  // than one interval.
  //
  // "Up" means EITHER side is failing, which is what remoteErrorText reports —
  // a recheck must keep running while a broken push is on screen even though
  // the fetch half is perfectly healthy.
  const remoteError = remoteErrorText(state);

  // ── Facets ──
  // The DISPLAYED target: a shown draft, else a pinned settings-only target,
  // else the browse context. With no repositories there is no context to show
  // and the repository draft is the target (its row is the rail's only
  // Repositories row, and its Cancel is withheld).
  // The fleet repository does not count: it is never a browse context, so an
  // instance whose only repository is the fleet's is a zero-repository shell
  // with the Fleet section (PR #528 B3).
  const noRepos = reposLoaded && !repos.some(r => !r.fleet);
  const displayed = displayedTarget(view, state.context, noRepos);
  const onContext = displayed.kind === 'repo' || displayed.kind === 'lens';
  // facetsFor is the one authority on which tabs the displayed target has; the
  // facet ON SCREEN is the chosen one when the target has it and Settings
  // otherwise (a lens whose mount broke under the reader, a repo whose create
  // is still running, every pinned target). The choice itself is left alone,
  // so it comes back with the next target that has it. A draft has no tabs and
  // no facet.
  const facets = useMemo(() => facetsFor(displayed, repos, lenses, creates), [displayed, repos, lenses, creates]);
  // The fleet: GET /fleet, polled app-wide (the rail's Fleet section, the
  // crumb's chip, the fleet target's pages).
  // Not while booting: nothing leaves the page until the boot is over.
  const { fleet, reload: reloadFleet } = useFleet(boot.phase === 'done' || noRepos);
  // The card the fleet's Members shows once after a create or a join.
  const [fleetLanding, setFleetLanding] = useState<FleetLanding | null>(null);
  const shownFacet: Facet | null = displayed.kind === 'draft' ? null : facets.includes(view.facet) ? view.facet : 'settings';
  const browseShown = view.server === null && onContext && shownFacet === 'browse';
  const targetLabel = useMemo(() => targetLabelFor(displayed, repos, archived, creates, fleet), [displayed, repos, archived, creates, fleet]);
  // The Settings tab's dot: the current repo's remote is failing, by the
  // banner's source OR by the listing. Only for a repo context — a lens's
  // Settings has no Remote block, and a dot pointing at nothing is a dead
  // control — and never on a read-only server, where the Remote block is
  // hidden for the same reason. A pinned target is not the current repo.
  const currentRepoInfo = repos.find(r => r.name === state.repo);
  const settingsDot = !state.serverReadOnly && onContext && state.context.kind === 'repo'
    && (Boolean(remoteError) || (currentRepoInfo ? remoteFailing(currentRepoInfo) : false));
  useEffect(() => {
    if (!remoteError) return;
    let cancelled = false;
    const id = setInterval(() => syncRemoteError(state.repo, () => !cancelled), REMOTE_RECHECK_MS);
    return () => { cancelled = true; clearInterval(id); };
  }, [remoteError, state.repo, syncRemoteError]);

  // The rows the rail draws for drafts. The repository draft is also there
  // whenever there are no repositories, because then it is the target.
  const drafts = useMemo(() => ({ repo: view.draft === 'repo' || noRepos, lens: view.draft === 'lens', fleet: view.draft === 'fleet' }), [view.draft, noRepos]);
  // With no repositories the draft is the target: make it a real draft, so
  // its registration lands like any other (DRAFT_REGISTERED) and picking a
  // pinned row and back again behaves as it does with repositories.
  useEffect(() => {
    // A fleet draft is left alone: a fresh instance may create or join a
    // fleet before it has a repository.
    if (noRepos && view.draft !== 'repo' && view.draft !== 'fleet' && view.pinned?.kind !== 'fleet') dispatchView({ type: 'OPEN_DRAFT', kind: 'repo', from: stateRef.current.context });
  }, [noRepos, view.draft, view.pinned, dispatchView]);
  // The fleet repository is never the browse context (B3): whatever pointed
  // it there (a remembered context, a link, a rename), the context moves to a
  // browsable repository, or to none, and the fleet target is shown instead.
  useEffect(() => {
    const ctx = state.context;
    if (ctx.kind !== 'repo' || !repos.some(r => r.name === ctx.repo && r.fleet)) return;
    const next = pickRepo('', repos, null);
    if (next) dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: next } });
    else dispatch({ type: 'SET_REPO', repo: '' });
    if (!view.locked && view.draft === null) {
      dispatchView({ type: 'SELECT_PINNED', target: { kind: 'fleet' }, available: FLEET_FACETS });
    }
  }, [state.context, repos, view.locked, view.draft, dispatch, dispatchView]);
  // The zero-repo shell IS the end of boot: there is nothing to open, so the
  // boot screen must not come back while the first repository is created.
  useEffect(() => {
    if (noRepos) dispatchBoot({ type: 'PHASE', phase: 'done' });
  }, [noRepos, dispatchBoot]);

  // A pinned target that no longer exists is unpinned: an archived repo purged
  // elsewhere, a repo with no store that came back (or went), a broken lens
  // that was deleted. The reader lands on the context's facet, as when leaving
  // any pinned target. Both corrections happen to the target UNDER the
  // server pages if they are open (keepServer): nobody is taken off them.
  //
  // A broken lens that was REPAIRED (its missing mount removed, or the store
  // back) is readable again, so it becomes what a readable lens is: the
  // context. The reader stays on its Settings, where the repair was made.
  useEffect(() => {
    const p = view.pinned;
    if (!p) return;
    if (p.kind === 'brokenLens' && reposLoaded) {
      const lens = lenses.find(l => l.name === p.name);
      if (lens && brokenLensMember(lens, repos) === null) {
        const ctx: BrowseContext = { kind: 'lens', name: p.name };
        dispatchView({ type: 'SELECT_CONTEXT_TARGET', available: facetsFor(ctx, repos, lenses, creates), keepServer: true });
        dispatchView({ type: 'SET_FACET', facet: 'settings', keepServer: true });
        dispatch({ type: 'SET_CONTEXT', context: ctx });
        return;
      }
    }
    // The fleet target is gone once GET /fleet names no fleet repository
    // (a leave, a decommission).
    const gone = (p.kind === 'fleet' && fleet !== null && !fleet.fleet_repo)
      || (p.kind === 'archived' && archivedLoaded && !archived.some(a => a.id === p.id))
      || (p.kind === 'unavailable' && reposLoaded && !repos.some(r => r.name === p.repo && !repoAvailable(r)))
      || (p.kind === 'brokenLens' && reposLoaded && !lenses.some(l => l.name === p.name));
    if (gone) dispatchView({ type: 'SELECT_CONTEXT_TARGET', available: facetsFor(state.context, repos, lenses, creates), keepServer: true });
  }, [view.pinned, archived, archivedLoaded, repos, reposLoaded, lenses, creates, state.context, dispatch, dispatchView, fleet]);

  // The server pages, behind the gear. They show OVER the displayed target,
  // which stays exactly as it was, so the gear again (or any rail pick) is the
  // way back to it. Which pages exist: Fleet always; Logs and Authorizations
  // never on a read-only server; Authorizations only with an OAuth issuer,
  // probed while the pages are open (useOAuthAvailability). A page whose tab
  // is gone falls back to Fleet.
  const serverOpen = view.server !== null;
  const oauth = useOAuthAvailability({ enabled: serverOpen && !state.serverReadOnly, pageOpen: view.server === 'auth' });
  const serverPages = useMemo(() => serverPagesFor(state.serverReadOnly, oauth.available), [state.serverReadOnly, oauth.available]);
  const serverPage: ServerPage | null = view.server === null ? null : serverPages.includes(view.server) ? view.server : (serverPages[0] ?? null);
  const onServerPage = useCallback((p: ServerPage) => dispatchView({ type: 'SET_SERVER_PAGE', page: p }), [dispatchView]);
  // The gear refuses while a connect commit runs in the Settings facet:
  // opening the server pages over it would unmount the wizard.
  const onGear = useCallback(() => {
    if (view.locked) return;
    dispatchView({ type: 'TOGGLE_SERVER' });
  }, [view.locked, dispatchView]);
  const serverBar = useMemo(() => serverPage === null ? null : ({
    page: serverPage, pages: serverPages, onPage: onServerPage, authCount: oauth.count,
  }), [serverPage, serverPages, onServerPage, oauth.count]);
  // Leaving the Settings facet (for another facet, another target, or the
  // server pages) re-reads the remote: Settings is where a remote is repaired,
  // and the banner must not outlive the fix. Switching TARGET while on
  // Settings needs nothing here — the repo change re-reads it already.
  const settingsShown = !serverOpen && onContext && shownFacet === 'settings';
  const wasSettingsShown = useRef(settingsShown);
  useEffect(() => {
    if (wasSettingsShown.current && !settingsShown) syncRemoteError(stateRef.current.repo);
    wasSettingsShown.current = settingsShown;
  }, [settingsShown, syncRemoteError]);

  const onFacet = useCallback((f: Facet) => {
    if (view.locked) return;
    dispatchView({ type: 'SET_FACET', facet: f });
  }, [view.locked, dispatchView]);
  const clearFocus = useCallback(() => dispatchView({ type: 'CLEAR_FOCUS' }), [dispatchView]);

  // Keyboard shortcuts.
  //
  // A LAYOUT effect, not a passive one, because this handler closes over state
  // that changes OUTSIDE discrete events. React runs the DOM commit and the
  // passive-effect flush in separate tasks for any such update, so a passive
  // registration leaves a window where the handler contradicts the screen.
  //
  // NOT for the view (the facet, the server pages): every route that changes
  // it is a click, and React flushes passive effects before the next discrete
  // event, so it is never stale. The path that IS reachable is `state.asOf`.
  // returnToNow dispatches APPLY_NAV with asOf live only AFTER an await
  // (useTimeTravel.ts), so the return to live lands in a promise continuation;
  // in the window after it paints, a passive handler still reads
  // !isLive(state) and answers Escape with a redundant returnToNow instead of
  // the CLEAR_FILTERS the reader is now asking for. Same for `h`.
  //
  // The cost, stated because it is not free: `state` is in the dep array, so
  // this now re-runs before paint on every dispatch — one removeEventListener
  // plus one addEventListener per render that changes state. That is the price
  // of the handler never describing a screen the user is not looking at.
  useLayoutEffect(() => {
    const handler = (e: KeyboardEvent) => {
      // The rail. Window-level like Back, so checked before the input guard:
      // ⌘K is how you get to the rail's search from inside the filter box, and
      // neither key types anything. ⌘B toggles; ⌘K opens if needed and asks
      // the search for focus. Nothing here — and nothing below — closes the
      // rail on Escape: it is a column of the layout, not a popup.
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === 'b' || e.key === 'B')) {
        e.preventDefault();
        dispatchView({ type: 'TOGGLE_RAIL' });
        return;
      }
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === 'k' || e.key === 'K')) {
        e.preventDefault();
        // Over the server pages there is no rail and no search to focus; a
        // request raised now would fire when the rail mounts again.
        if (view.server === null) {
          dispatchView({ type: 'SET_RAIL', open: true });
          setRailFocusRequest(n => n + 1);
        }
        return;
      }

      // Everything below is a BROWSE shortcut: it acts on the facts being
      // read. Off the Browse facet of the context — on Activity, on Settings,
      // on a pinned target, on a draft, on the server pages — there are none
      // on screen, so the keys are no-ops, Escape included: nothing at this
      // level is "exited" by it, the server pages neither (the gear is).
      //
      // Back is a WINDOW-level command, so it is checked BEFORE the input
      // guard below: nothing types ⌘[ / Alt+←, and going back from a focused
      // fact is exactly when you want them. Backspace/Delete stay behind the
      // guard, because those DO type.
      if (!browseShown) return;

      if ((e.metaKey && e.key === '[') || (e.altKey && e.key === 'ArrowLeft')) {
        e.preventDefault();
        dispatch({ type: 'NAV_BACK' });
        return;
      }

      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

      if (e.key === '/') {
        e.preventDefault();
        // One field now, always on screen while live — the dashboard's separate
        // box is gone, and with it the branch that had to guess which of two
        // searches the key meant.
        document.getElementById('filter-input')?.focus();
        return;
      }
      if (e.key === 'Escape') {
        // While history, Escape returns to now (the read-only excursion is the
        // thing the user wants to dismiss). When already live, Escape clears the
        // active filters as before.
        if (!isLive(state)) tt.returnToNow();
        else dispatch({ type: 'CLEAR_FILTERS' });
        return;
      }
      if (e.key === 'Backspace' || e.key === 'Delete') {
        e.preventDefault();
        dispatch({ type: 'NAV_BACK' });
        return;
      }
      if (e.key === 'h') {
        e.preventDefault();
        tt.returnToNow();
        return;
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [navigate, state, tt, browseShown, view.server, dispatchView]);

  // A finished task is news for a moment, not for the session. Nothing used to
  // return a task to idle, so the footer kept the LAST terminal result forever
  // — "[sync] ok" from three hours ago reading as something happening now.
  // Running tasks are untouched (see CLEAR_TASK), however long they run.
  useEffect(() => {
    const finished = Object.entries(state.tasks)
      .filter(([, t]) => t.status === 'done' || t.status === 'error')
      .map(([op]) => op);
    if (finished.length === 0) return;
    const timers = finished.map(op => setTimeout(() => dispatch({ type: 'CLEAR_TASK', op }), TASK_LINGER_MS));
    return () => timers.forEach(clearTimeout);
  }, [state.tasks]);

  // Transient amber notice (e.g. "fact was retracted — returned to now").
  // Auto-clears after ~6s; the effect re-arms whenever a new notice is set.
  useEffect(() => {
    if (!state.notice) return;
    const id = setTimeout(() => dispatch({ type: 'CLEAR_NOTICE' }), 6000);
    return () => clearTimeout(id);
  }, [state.notice]);

  // Stable handler identities for the memoized panels. An inline arrow here is
  // a fresh prop every render, which would make React.memo on TopBar/FilterBar
  // inert — the panels would re-render on every splitter drag frame and every
  // repos/lenses refresh even though nothing they display changed.
  const jumpTrail = useCallback((i: number) => {
    // A crumb records how many navStack entries lie between it and the current
    // view (one crumb can stand for a fact's live and timeline entries), so
    // jumping to crumb i means unwinding that many entries — pop, don't push. Reads the CURRENT
    // state through the ref so the callback identity can stay stable.
    const trail = selectTrail(stateRef.current);
    const pops = trail[i]?.pops ?? trail.length - 1 - i;
    for (let k = 0; k < pops; k++) dispatch({ type: 'NAV_BACK' });
  }, [dispatch]);
  // Re-sync after a repo/lens mutation. In a lens context this re-resolves the
  // browsed lens (so edited mounts refresh state.lens) or falls back with a
  // notice if it was deleted; in a repo context it switches off an
  // archived/removed active repo (I4) — or, when `renamed` says the active
  // repo is the one that got renamed, follows it to its new name instead. The
  // archived list is re-read alongside, since an archive, restore or purge is
  // what changes it. Resolves once every list is in.
  const refreshLists = useCallback(async (renamed?: { from: string; to: string }) => {
    const archivedRead = api.listArchived()
      .then(list => { setArchived(list); setArchivedLoaded(true); })
      .catch(() => { /* best-effort: the group keeps what it had */ });
    try {
      const { lenses: ls, repos: rs } = await refreshContextAfterChange(dispatch, stateRef.current.context, stateRef.current.repo, {}, isCurrentLens, renamed);
      setLenses(ls); setRepos(rs);
    } catch { /* best-effort: the lists keep what they had */ }
    await archivedRead;
  }, [dispatch, isCurrentLens]);
  /**
   * enterBranch switches the whole UI to a branch of the current repo.
   *
   * Entering an experiment IS this and nothing more, which is the point: the
   * branch row it fetches carries `writable` and `experiment`, so the
   * read-only gate, the in-experiment marker and the since-fork filter all
   * follow from one status read rather than from three flags set by hand.
   *
   * It lands on the Browse facet, because the action changes what you are
   * reading: staying on Settings (where the Experiments block lives) would
   * hide the result of the thing just clicked.
   */
  const enterBranch = useCallback((branch: string) => {
    if (!branch) return;
    dispatchView({ type: 'SET_FACET', facet: 'browse' });
    api.status(stateRef.current.repo, branch)
      .then(s => dispatch(statusAction(s)))
      .catch(() => { /* the status poll re-reads; a failed switch leaves the old branch shown */ });
  }, [dispatchView]);

  // The view, read from callbacks that must keep a stable identity. Updated
  // in a layout effect, so it is current before any event can reach them.
  const viewRef = useRef(view);
  useLayoutEffect(() => { viewRef.current = view; }, [view]);

  // A rail pick of a normal target. It switches the browse context, applying
  // the facet rule, and is a no-op on the target already shown — re-entering
  // it would reset the reader's navigation for nothing. Picking the context
  // while a pinned target or the draft is on screen just goes back to it. The
  // rail stays open: it is a column of the layout, not a menu that closes on
  // choice.
  const onSelectTarget = useCallback((ctx: BrowseContext) => {
    // The rail is disabled while locked; this is the same rule for any other
    // route here (Mounted in), so no path can unmount the commit's page.
    if (viewRef.current.locked) return;
    const cur = stateRef.current.context;
    const same = (ctx.kind === 'repo' && cur.kind === 'repo' && cur.repo === ctx.repo)
      || (ctx.kind === 'lens' && cur.kind === 'lens' && cur.name === ctx.name);
    const v = viewRef.current;
    if (same && v.pinned === null && !v.draftShown && v.server === null) return;
    dispatchView({ type: 'SELECT_CONTEXT_TARGET', available: facetsFor(ctx, repos, lenses, creates) });
    if (!same) dispatch({ type: 'SET_CONTEXT', context: ctx });
  }, [dispatch, dispatchView, repos, lenses, creates]);

  // A rail pick of a settings-only target: pinned over the context.
  const onPin = useCallback((t: PinnedTarget) => {
    if (viewRef.current.locked) return;
    dispatchView({ type: 'SELECT_PINNED', target: t });
  }, [dispatchView]);
  // The fleet row: the fleet target, pinned over the context with its own
  // tabs (Members, Activity, Settings).
  const onPickFleet = useCallback(() => {
    if (viewRef.current.locked) return;
    dispatchView({ type: 'SELECT_PINNED', target: { kind: 'fleet' }, available: FLEET_FACETS });
  }, [dispatchView]);
  // New fleet created or joined: the draft gives way to the fleet row and,
  // if the draft was on screen, the reader lands on the fleet's Members with
  // the landing card. GET /fleet is read first, so the rail has the row.
  const onFleetDraftDone = useCallback((landing: FleetLanding) => {
    void Promise.all([reloadFleet(), refreshLists()]).then(() => {
      const v = viewRef.current;
      if (v.draft !== 'fleet') return;
      const shown = v.draftShown;
      dispatchView({ type: 'DRAFT_REGISTERED', kind: 'fleet', facet: 'members' });
      if (shown) {
        setFleetLanding(landing);
        dispatchView({ type: 'SELECT_PINNED', target: { kind: 'fleet' }, available: FLEET_FACETS });
      }
    });
  }, [dispatchView, reloadFleet, refreshLists]);
  // A fleet action changed the fleet (a revoke, a leave, an import): read it
  // again now rather than at the next poll.
  const onFleetChanged = useCallback(() => {
    void reloadFleet();
    void refreshLists();
  }, [reloadFleet, refreshLists]);

  // A section's `+`, and a draft row. The draft remembers what was on screen,
  // for Cancel.
  const onNewDraft = useCallback((kind: DraftKind) => {
    if (viewRef.current.locked || stateRef.current.serverReadOnly) return;
    dispatchView({ type: 'OPEN_DRAFT', kind, from: stateRef.current.context });
  }, [dispatchView]);
  const onPickDraft = useCallback((kind: DraftKind) => {
    if (viewRef.current.locked) return;
    const v = viewRef.current;
    if (v.draft === kind) dispatchView({ type: 'SHOW_DRAFT', from: stateRef.current.context });
    else dispatchView({ type: 'OPEN_DRAFT', kind, from: stateRef.current.context });
  }, [dispatchView]);
  // Cancel: the draft goes and what was on screen before it comes back — the
  // pinned target and the facet from the view, the context from AppState. A
  // context that no longer exists falls back to a readable repo.
  const onCancelDraft = useCallback(() => {
    const r = viewRef.current.returnTo;
    dispatchView({ type: 'CANCEL_DRAFT' });
    if (!r) return;
    const cur = stateRef.current.context;
    const back = r.context;
    const same = (back.kind === 'repo' && cur.kind === 'repo' && cur.repo === back.repo)
      || (back.kind === 'lens' && cur.kind === 'lens' && cur.name === back.name);
    if (same) return;
    const exists = back.kind === 'repo'
      ? repos.some(x => x.name === back.repo && repoAvailable(x))
      : lenses.some(l => l.name === back.name);
    if (exists) { dispatch({ type: 'SET_CONTEXT', context: back }); return; }
    const fallback = pickRepo(cur.kind === 'repo' ? cur.repo : '', repos, null);
    if (fallback && !(cur.kind === 'repo' && cur.repo === fallback)) {
      dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: fallback } });
    }
  }, [dispatch, dispatchView, repos, lenses]);

  // The repository draft's repo now EXISTS (registered, the index under way):
  // the draft row gives way to the real row, chipped "creating", and — if the
  // draft was on screen — the reader lands on its Settings in creating mode
  // (E3), where Browse and Activity appear when the job is done. Lists first,
  // so the rail has the row to select.
  //
  // Whether the draft is on screen is read AFTER the refresh, and only for the
  // REPOSITORY draft. The wizard's create keeps polling once its draft is gone
  // (Cancel, or the lens draft taking the one slot), so this can fire for a
  // draft that is no longer there; then it retires nothing and moves nobody.
  const onRepoDraftRegistered = useCallback((name: string) => {
    const hadNoRepos = repos.length === 0;
    void refreshLists().then(() => {
      const v = viewRef.current;
      if (v.draft !== 'repo') return;
      // With no repositories the draft is the fallback target even when not
      // marked shown (nothing is pinned over it).
      const shown = v.draftShown || (hadNoRepos && v.pinned === null);
      if (shown && !v.draftShown) dispatchView({ type: 'SHOW_DRAFT', from: stateRef.current.context });
      dispatchView({ type: 'DRAFT_REGISTERED', kind: 'repo', facet: 'settings' });
      if (shown) dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: name } });
    });
  }, [dispatch, dispatchView, refreshLists, repos.length]);
  // The lens draft created its lens: land on its Browse — if the lens draft is
  // still the one on screen (the same guard as the repository draft's).
  const onLensDraftCreated = useCallback((name: string) => {
    void refreshLists().then(() => {
      const v = viewRef.current;
      if (v.draft !== 'lens') return;
      const shown = v.draftShown;
      dispatchView({ type: 'DRAFT_REGISTERED', kind: 'lens', facet: 'browse' });
      if (shown) dispatch({ type: 'SET_CONTEXT', context: { kind: 'lens', name } });
    });
  }, [dispatch, dispatchView, refreshLists]);

  // Leaving a pinned target for the context (a purge, a dismissed create):
  // the facet rule restores what the reader had.
  const unpin = useCallback(() => {
    dispatchView({ type: 'SELECT_CONTEXT_TARGET', available: facetsFor(stateRef.current.context, repos, lenses, creates) });
  }, [dispatchView, repos, lenses, creates]);
  // An archived repo was restored: it is a repository again, so it becomes the
  // context (the facet rule restores what the reader had before the pin).
  const onRestored = useCallback((name: string) => {
    void refreshLists().then(() => {
      dispatchView({ type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'] });
      dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: name } });
    });
  }, [dispatch, dispatchView, refreshLists]);
  const onPurged = useCallback(() => {
    void refreshLists().then(unpin);
  }, [refreshLists, unpin]);
  // A repo with no store was archived: its archived row is the new target.
  // Refresh FIRST — if it was the context, refreshContextAfterChange moves the
  // context off it — then pin, by the id the server returned.
  const onUnavailableArchived = useCallback((row: ArchivedRepo) => {
    void refreshLists().then(() => dispatchView({ type: 'SELECT_PINNED', target: { kind: 'archived', id: row.id } }));
  }, [refreshLists, dispatchView]);
  // A create not yet registered now has its repository: it becomes the
  // context, on Settings (its create is still working, so Settings is all it
  // has), and the reader lands on its creating mode.
  const onCreateRegistered = useCallback((name: string) => {
    void refreshLists().then(() => {
      dispatchView({ type: 'SELECT_CONTEXT_TARGET', available: ['settings'] });
      dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: name } });
    });
  }, [dispatch, dispatchView, refreshLists]);

  // The banner's Reconnect lands on the failing repo's Settings, on its
  // Remote block. In a lens context the failing repo is the lens's write repo
  // (state.repo), so it becomes the target: the lens's own Settings has no
  // Remote block to land on. A pinned target, the draft or the server pages
  // give way to it (SET_FACET leaves the server pages).
  const onReconnect = useCallback(() => {
    dispatch({ type: 'CLEAR_REMOTE_ERRORS' });
    const cur = stateRef.current;
    if (cur.context.kind === 'lens' && cur.repo) {
      dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: cur.repo } });
    }
    const v = viewRef.current;
    if (v.pinned || v.draftShown) dispatchView({ type: 'SELECT_CONTEXT_TARGET', available: ['settings'] });
    dispatchView({ type: 'SET_FACET', facet: 'settings', focus: 'remote' });
  }, [dispatch, dispatchView]);

  // A settings mutation: re-sync the lists and the context, then re-read the
  // remote, which a connect or disconnect changed.
  const onSettingsChanged = useCallback((renamed?: { from: string; to: string }) => {
    void refreshLists(renamed);
    syncRemoteError(stateRef.current.repo);
  }, [refreshLists, syncRemoteError]);
  const onCancelCreate = useCallback(async () => {
    const job = activeCreateByRepo(creates).get(stateRef.current.repo);
    if (!job) return;
    try { await api.cancelRepoCreate(job.create_id); } catch { /* the refresh tells the truth */ }
    await refreshRepoCreates();
  }, [creates]);
  // "Mounted in" on a repo's Settings: a lens with a mount that has no store is
  // pinned, as from the rail, never entered (the rescue would bounce it).
  const onSettingsSelectLens = useCallback((name: string) => {
    const lens = lenses.find(l => l.name === name);
    if (lens && brokenLensMember(lens, repos) !== null) { onPin({ kind: 'brokenLens', name }); return; }
    onSelectTarget({ kind: 'lens', name });
  }, [lenses, repos, onPin, onSelectTarget]);
  // The status footer's "N repositories being created": the creates are rows
  // in the rail, each with its chip, so the count opens the rail. Over the
  // server pages it does nothing: the rail is hidden there and keeps the
  // reader's preference (SET_RAIL is refused).
  const onOpenCreates = useCallback(() => {
    dispatchView({ type: 'SET_RAIL', open: true });
  }, [dispatchView]);

  // Before repo AND branch are known there is nothing to render, but the user
  // is owed an account of what is happening — which phase, how long, which
  // attempt, and a way out when it has failed. Zero repositories takes
  // precedence: an install with nothing to browse is the ordinary first run,
  // not a boot that is stuck, and its shell is the repository draft.
  //
  // FIRST BOOT ONLY. The boot reducer is monotonic and `done` is terminal (it
  // is reached once, when the first branch is known — or when the zero-repo
  // shell renders — and Retry exists only on this screen, before it). After
  // that, a context switch — which clears the branch until the new repo's
  // status lands — keeps the shell and the rail mounted; the Browse facet
  // shows its own placeholder meanwhile, and Activity and Settings need no
  // branch at all.
  if (!state.branch && boot.phase !== 'done' && !noRepos) {
    return <BootScreen boot={boot} onRetry={onBootRetry} />;
  }

  // The shell has three levels: the top bar, then a row of [rail | column],
  // then the status footer. The rail is a COLUMN of that row, so opening it
  // pushes everything beside it right instead of covering it. The server pages
  // hide it (railShown) and the reader's preference survives them.
  //
  // With no repositories the rail is forced open (not persisted): the draft is
  // the only row, and a row nobody can see would leave the reader with no
  // idea what they are creating or where it will appear.
  // The rail is forced open while the repository draft IS the page (no
  // repository to show); over the fleet target the reader's preference holds,
  // or a fleet-only instance at a phone width has no room for its pages.
  const showRail = railShown(view, noRepos && displayed.kind === 'draft');
  const pinnedOrContext = displayed.kind === 'draft' ? null : displayed;
  const draftShownKind = displayed.kind === 'draft' ? displayed.draft : null;
  return (
    <div data-testid={noRepos ? 'no-repos' : undefined} style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'var(--k-font-body)', overflow: 'hidden' }}>
      {/* Every top-level panel gets its own INLINE boundary: one bad fact body
          or a malformed lens doc now blanks a single pane instead of the SPA. */}
      <ErrorBoundary variant="inline" label="The top bar hit an error">
        <TopBar state={state} repos={repos} label={targetLabel}
          railOpen={showRail} onToggleRail={toggleRail}
          facets={facets} facet={shownFacet ?? undefined} onFacet={onFacet} settingsDot={settingsDot} locked={view.locked}
          onGear={onGear} server={serverBar} gear={serverPages.length > 0} />
      </ErrorBoundary>
      <div className="k-shell-body" style={{ flex: 1, minHeight: 0, display: 'flex', overflow: 'hidden' }}>
        {showRail && (
          <ErrorBoundary variant="inline" label="The rail hit an error">
            <TargetRail
              repos={repos}
              lenses={lenses}
              context={state.context}
              currentRepo={state.repo}
              remoteError={remoteError}
              serverReadOnly={state.serverReadOnly}
              onSelect={onSelectTarget}
              archived={archived}
              pinned={displayed.kind === 'draft' ? null : view.pinned}
              onPin={onPin}
              drafts={drafts}
              draftShown={draftShownKind}
              onNewDraft={onNewDraft}
              onPickDraft={onPickDraft}
              fleet={fleet}
              onPickFleet={onPickFleet}
              focusRequest={railFocusRequest}
              onFocusHandled={clearRailFocusRequest}
              locked={view.locked}
            />
          </ErrorBoundary>
        )}
        <div style={{ flex: 1, minWidth: 0, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
          {state.notice && (
            <div data-testid="notice" style={{ background: '#2a200e', color: '#f5c47a', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #a36a18', flexShrink: 0 }}>
              {state.notice}
            </div>
          )}
          {/* The banner reports a REMEMBERED failure, so it needs a way out that
              does not depend on the next remote event arriving. Both buttons clear
              it: acting on it (Reconnect…) and acknowledging it (✕). Nothing is
              lost by clearing — the repo's Remote block still shows the stored
              "✗ sync failed" line, and a remote that is still broken raises the
              banner again on the next failing tick. */}
          {remoteError && (
            <div data-testid="remote-error-banner" style={{ background: '#2b1c1c', color: '#e0a0a0', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #3a2a2a', flexShrink: 0, display: 'flex', alignItems: 'center', gap: 10 }}>
              <span>⚠ Remote sync failed — {remoteError}</span>
              <button
                type="button"
                data-testid="remote-error-reconnect"
                onClick={onReconnect}
                style={{ background: '#7f1d1d', color: '#eee', border: '1px solid #5c2a2a', borderRadius: 4, padding: '2px 8px', fontSize: 11, cursor: 'pointer', flexShrink: 0 }}
              >
                Reconnect…
              </button>
              <div style={{ flex: 1 }} />
              <button
                type="button"
                data-testid="remote-error-dismiss"
                aria-label="Dismiss the remote sync error"
                title="Dismiss"
                onClick={() => dispatch({ type: 'CLEAR_REMOTE_ERRORS' })}
                style={{ background: 'none', color: '#b98080', border: 'none', fontSize: 14, lineHeight: 1, padding: '0 2px', cursor: 'pointer', flexShrink: 0 }}
              >
                ✕
              </button>
            </div>
          )}
          {/* The body: the displayed target's facet, or its draft — or, over
              them, the server pages. The drafts stay mounted under the server
              pages (hidden, as under another target), so nothing typed into
              one is lost by a look at the log. */}
          <FacetLayoutProvider value={facetLayout}>
            {serverPage !== null && (
              <ErrorBoundary variant="inline" label="The server pages hit an error">
                <ServerPages page={serverPage} onAuthCount={oauth.setCount} />
              </ErrorBoundary>
            )}
            {/* The drafts stay MOUNTED while another target is shown, hidden
                rather than unmounted, so a half-filled form and a probe or a
                create in flight survive a detour through the rail. */}
            {drafts.repo && (
              <div data-testid="draft-repo" style={draftBody(!serverOpen && draftShownKind === 'repo')}>
                <ErrorBoundary variant="inline" label="New repository hit an error">
                  {state.serverReadOnly ? <CreateBlocked what="repository" /> : (
                    <CreateRepoWizard
                      // Fires the moment the repo EXISTS, not when the job
                      // ends — see maybeEnterRepo.
                      onDone={onRepoDraftRegistered}
                      onCancel={draftCancellable('repo', repos) ? onCancelDraft : undefined}
                    />
                  )}
                </ErrorBoundary>
              </div>
            )}
            {drafts.lens && (
              <div data-testid="draft-lens" style={draftBody(!serverOpen && draftShownKind === 'lens')}>
                <ErrorBoundary variant="inline" label="New lens hit an error">
                  {state.serverReadOnly ? <CreateBlocked what="lens" /> : (
                    <CreateLensForm
                      repos={repos}
                      lenses={lenses}
                      onDone={onLensDraftCreated}
                      onCancel={draftCancellable('lens', repos) ? onCancelDraft : undefined}
                      onError={() => { /* the form shows its own error */ }}
                    />
                  )}
                </ErrorBoundary>
              </div>
            )}
            {drafts.fleet && (
              <div data-testid="draft-fleet" style={draftBody(!serverOpen && draftShownKind === 'fleet')}>
                <ErrorBoundary variant="inline" label="New fleet hit an error">
                  {/* The fleet `+` is off on a read-only server, so no fleet
                      draft opens there. */}
                  <FleetWizard fleet={fleet} onDone={onFleetDraftDone} onCancel={onCancelDraft} />
                </ErrorBoundary>
              </div>
            )}
            {/* The fleet target's pages. */}
            {!serverOpen && displayed.kind === 'fleet' && fleet?.fleet_repo && (
              <ErrorBoundary variant="inline" label="The fleet hit an error">
                {shownFacet === 'members' && (
                  <FleetMembers fleet={fleet} landing={fleetLanding} onDismissLanding={() => setFleetLanding(null)} onChanged={onFleetChanged} />
                )}
                {shownFacet === 'activity' && <FleetActivity fleet={fleet} />}
                {shownFacet === 'settings' && (
                  <FleetSettings fleet={fleet as FleetStatus} onChanged={onFleetChanged} onBusyChange={setLocked} />
                )}
              </ErrorBoundary>
            )}
            {!serverOpen && pinnedOrContext && pinnedOrContext.kind !== 'fleet' && shownFacet === 'browse' && (
              <BrowseFacet
                state={state}
                dispatch={dispatch}
                navigate={navigate}
                tt={tt}
                edges={edges}
                repoNames={repoNames}
                leftPanelWidth={leftPanelWidth}
                libraryNarrow={libraryNarrow}
                onSplitterDown={startSplitterDrag}
                onJumpTrail={jumpTrail}
                onEnterBranch={enterBranch}
              />
            )}
            {!serverOpen && pinnedOrContext && pinnedOrContext.kind !== 'fleet' && shownFacet === 'activity' && (
              <ErrorBoundary variant="inline" label="Activity hit an error">
                <ActivityFacet
                  // Keyed on the target so a switch starts a fresh read
                  // rather than showing the last target's rows meanwhile.
                  key={state.context.kind === 'repo' ? `repo:${state.context.repo}` : `lens:${state.context.name}`}
                  target={state.context.kind === 'repo'
                    ? { kind: 'repo', uid: currentRepoInfo?.uid ?? '' }
                    : { kind: 'lens', uid: lenses.find(l => l.name === (state.context.kind === 'lens' ? state.context.name : ''))?.uid ?? '' }}
                />
              </ErrorBoundary>
            )}
            {!serverOpen && pinnedOrContext && pinnedOrContext.kind !== 'fleet' && shownFacet === 'settings' && (
              <ErrorBoundary variant="inline" label="Settings hit an error">
                <SettingsFacet
                  target={pinnedOrContext}
                  repos={repos}
                  lenses={lenses}
                  archived={archived}
                  serverReadOnly={state.serverReadOnly}
                  createJob={repoCreate ?? null}
                  onCancelCreate={onCancelCreate}
                  focus={view.focus}
                  onFocused={clearFocus}
                  onChanged={onSettingsChanged}
                  onSelectLens={onSettingsSelectLens}
                  onEnterBranch={enterBranch}
                  currentBranch={state.branch}
                  onBusyChange={setLocked}
                  onRestored={onRestored}
                  onPurged={onPurged}
                  onUnavailableArchived={onUnavailableArchived}
                  onCreateClosed={unpin}
                  onCreateRegistered={onCreateRegistered}
                />
              </ErrorBoundary>
            )}
          </FacetLayoutProvider>
        </div>
      </div>
      <ErrorBoundary variant="inline" label="The status footer hit an error">
        {/* Neither hint applies off the Browse facet: nothing else has a time
            axis to return from or a filter field to focus. */}
        <StatusFooter state={state} version={version}
          searchKey={isLive(state) && browseShown}
          historyKey={!isLive(state) && browseShown}
          onOpenCreates={onOpenCreates} />
      </ErrorBoundary>
    </div>
  );
}

// A draft's body: mounted always, shown only while it is the displayed target.
const draftBody = (shown: boolean): React.CSSProperties => ({
  flex: '1 1 0', minHeight: 0, flexDirection: 'column', overflow: 'hidden',
  display: shown ? 'flex' : 'none',
});
