import { useReducer, useEffect, useState, useRef, useCallback, useMemo } from 'react';
import type { Dispatch } from 'react';
import { reducer, init, isReadOnly, isLive, selectTrail, currentPath, lensResolutionPending, remoteErrorText } from './state';
import type { Action, BrowseContext } from './state';
import { api, apiUrl, fetchVersion } from './api';
import type { RepoInfo, Lens, Status } from './api';
import { pageview, track } from './telemetry';
import { useNavigationManager } from './useNavigationManager';
import { useFactEdges } from './useFactEdges';
import { useTimeTravel } from './useTimeTravel';
import { bootstrapStatusWithRetry } from './bootstrap';
import { pickRepo, loadLastContext, saveLastContext } from './repoSelection';
import { TopBar } from './TopBar';
import { RepoManager } from './RepoManager';
import { ErrorBoundary } from './ErrorBoundary';
import { FilterBar } from './FilterBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { StatusFooter } from './StatusFooter';
import { useVersion } from './hooks';
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

// SSE outage-log rate limiting. See the events effect for why the re-arm needs
// a ceiling: at EventSource's ~3s retry an unbounded flap fills the console's
// 500-entry ring in minutes.
const FLAP_WINDOW_MS = 60_000;
const FLAP_LIMIT = 3;

// How long a FINISHED task keeps the footer's task line before it retires. Long
// enough to read the outcome of something you just triggered, short enough that
// it never reads as current.
const TASK_LINGER_MS = 8_000;

// How often the remote-error banner re-checks the stored status while it is on
// screen. Nothing polls while the remote is healthy — see the recheck effect.
const REMOTE_RECHECK_MS = 60_000;

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
    dispatch({ type: 'SET_LENS', lens });
  } catch (err) {
    if (!isCurrentLens(name)) return; // context drifted — a newer surface owns the app
    dispatch({ type: 'SET_NOTICE', text: `Lens "${name}" is unavailable — showing a repo instead.` });
    diag('error', `[lens] ${name}: ${String(err)}`);
    const fallback = fallbackRepos[0];
    if (fallback) dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: fallback.name } });
  }
}

// refreshContextAfterChange re-syncs the browse surface after a repo/lens
// mutation reported by the RepoManager (I4). It re-fetches both lists and:
//   - lens context, lens still exists → re-run resolveLens so edited mounts
//     refresh state.lens (reads/write/description);
//   - lens context, lens gone → fall back to the first repo with the same
//     deleted-lens notice resolveLens uses, so no dead empty library is left;
//   - repo context → keep the prior behavior: if the active repo was
//     archived/removed, switch to a remaining one (prefer core).
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
): Promise<{ lenses: Lens[]; repos: RepoInfo[] }> {
  const listLenses = deps.listLenses ?? api.listLenses;
  const listRepos = deps.repos ?? api.repos;
  const getLens = deps.getLens ?? api.getLens;
  const [lenses, repoList] = await Promise.all([listLenses(), listRepos()]);
  if (context.kind === 'lens') {
    if (lenses.some(l => l.name === context.name)) {
      await resolveLens(context.name, repoList, dispatch, getLens, isCurrentLens);
    } else {
      dispatch({ type: 'SET_NOTICE', text: `Lens "${context.name}" is unavailable — showing a repo instead.` });
      const fallback = repoList[0];
      if (fallback) dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: fallback.name } });
    }
  } else if (repoList.length && !repoList.some(r => r.name === currentRepo)) {
    const next = repoList.find(r => r.name === 'core') ?? repoList[0];
    dispatch({ type: 'SET_REPO', repo: next.name });
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
  };
}

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);
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
  const version = useVersion();
  const [repos, setRepos] = useState<RepoInfo[]>([]);
  const [lenses, setLenses] = useState<Lens[]>([]);
  const [reposLoaded, setReposLoaded] = useState(false);
  const [repoMgrOpen, setRepoMgrOpen] = useState(false);

  // Time-travel callbacks (scrub / hop / open-at / return-to-now), backed by
  // the reducer. RightPanel + LeftPanel + FilterBar all route their
  // navigation through these so a single action model drives now and history.
  const tt = useTimeTravel(state, dispatch);

  // The open fact's edges, fetched ONCE here and handed to every consumer.
  // RightPanel (in-body ref pins) and the connections panel used to issue the
  // same api.explain call independently, for the same fact at the same anchor.
  // The anchor rules — which mount, which commit, when to fall back — moved
  // into the hook with the fetch; see useFactEdges.
  const edges = useFactEdges(state);


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
  const startSplitterDrag = (e: React.MouseEvent) => {
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
  };

  // Fetch the repo list on mount and select which repo to display. The repo
  // set is owned by the server — the UI never hardcodes a name, so it can't
  // assume the default ("core") still exists. pickRepo derives the selection
  // from the live list, preferring the user's last explicit choice and falling
  // back to the first available repo. reposLoaded gates the "no repos" empty
  // state below so an empty server doesn't hang on "Loading…".
  useEffect(() => {
    let cancelled = false;
    api.repos()
      .then(list => {
        if (cancelled) return;
        setRepos(list);
        setReposLoaded(true);
        // Restore the last browse context. A persisted lens is entered
        // immediately, then resolved (falling back to a repo if it's gone). A
        // repo (or no) context picks a repo from the live list as before.
        const last = loadLastContext();
        if (last?.kind === 'lens') {
          // Resolution is owned by the lensResolutionPending effect below.
          dispatch({ type: 'SET_CONTEXT', context: last });
          return;
        }
        const next = pickRepo('', list, last?.kind === 'repo' ? last.repo : null);
        if (next) dispatch({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: next } });
      })
      .catch(() => { if (!cancelled) setReposLoaded(true); });
    return () => { cancelled = true; };
  }, []);

  // Lens resolution — single owner. Every surface that enters a lens context
  // (TopBar switcher, manager Browse, bootstrap restore) only dispatches
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

  // Fetch the lens list on mount for the TopBar context switcher's Lenses group.
  // Owned here (like repos) and threaded down; refreshed when the repo manager
  // reports a change (lens create/delete). Best-effort: an empty list just hides
  // the Lenses group.
  useEffect(() => {
    let cancelled = false;
    api.listLenses()
      .then(list => { if (!cancelled) setLenses(list); })
      .catch(() => { /* best-effort: no Lenses group on failure */ });
    return () => { cancelled = true; };
  }, []);

  // Fetch server read-only flag once on mount and propagate to global state.
  useEffect(() => {
    let alive = true;
    fetchVersion()
      .then(v => { if (alive) dispatch({ type: 'SET_SERVER_READONLY', value: v.readOnly }); })
      .catch(() => { /* best-effort: stay writable on failure */ });
    return () => { alive = false; };
  }, []);

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
    bootstrapStatusWithRetry({
      repo: state.repo,
      initialBranch: state.branch,
      getAgentBranch: api.getAgentBranch,
      getStatus: api.status,
      onSuccess: (s) => { dispatch(statusAction(s)); },
      onAttemptFailed: (err, attempt) => {
        diag('error', `[bootstrap] attempt ${attempt + 1} failed: ${String(err)}`);
      },
      shouldStop: () => cancelled,
    });
    return () => { cancelled = true; };
  }, [state.repo]);

  // While a repo indexes in the background, poll status so the indexing banner
  // updates and clears when it reaches "ready" (no commits fire during a
  // background rebuild, so SSE 'status' events wouldn't refresh it).
  useEffect(() => {
    if (state.indexState !== 'indexing' || !state.branch) return;
    let cancelled = false;
    const id = setInterval(() => {
      api.status(state.repo, state.branch)
        .then(s => { if (!cancelled) dispatch(statusAction(s)); })
        .catch(() => {});
    }, 2000);
    return () => { cancelled = true; clearInterval(id); };
  }, [state.indexState, state.repo, state.branch]);

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

  // SSE for task and status events — reconnects when repo/branch changes.
  useEffect(() => {
    if (!state.branch) return; // wait until branch is known from status bootstrap
    const es = new EventSource(apiUrl(`/api/v1/repos/${state.repo}/branches/${state.branch.replaceAll('/', ':')}/events`));
    // EventSource silently auto-reconnects on disconnect. Without the error
    // handler below, a backend that 500s the stream produces a stale
    // LIVE/HISTORY pill (no SET_HEAD updates arrive) with no signal to the user.
    //
    // Logged once per OUTAGE, and 'open' re-arms it — without the re-arm "once
    // per outage" was really once per SUBSCRIPTION LIFETIME, so a second outage
    // on a long-lived stream logged nothing and the user saw a stale head pill
    // with no explanation.
    //
    // The re-arm needs a companion, though: a backend that accepts and then
    // immediately drops re-arms on every EventSource retry (~3s), and logging
    // the disconnect + reconnect pair each cycle is ~40 lines/min — enough to
    // flush the 500-entry ring of the task/remote lines the console exists for
    // in about 12 minutes. So outages are counted over a rolling window: the
    // first few are reported normally, and past FLAP_LIMIT the pair goes quiet
    // behind ONE summary line until the stream has been calm for a full window.
    let loggedDisconnect = false;
    let windowStart = 0;   // start of the current flap-counting window
    let outages = 0;       // outages reported inside it
    let suppressed = false;
    const logOutage = (message: string) => {
      const now = Date.now();
      if (now - windowStart > FLAP_WINDOW_MS) { windowStart = now; outages = 0; suppressed = false; }
      outages += 1;
      if (outages > FLAP_LIMIT) {
        if (!suppressed) {
          suppressed = true;
          diag('error', '[events] stream flapping — suppressing further connection lines');
        }
        return;
      }
      diag('error', message);
    };
    es.addEventListener('open', () => {
      // Only report a recovery for an outage that was actually REPORTED. The
      // very first open has nothing to recover from, and a reconnect during a
      // suppressed flap storm must stay as quiet as the disconnect that paired
      // with it — otherwise suppression would halve the noise instead of
      // stopping it.
      if (loggedDisconnect && !suppressed) {
        diag('info', '[events] reconnected');
        // A gap in the stream is a gap in the remote events, and the one that
        // clears the banner (sync_ok / push_ok) is broadcast once, never
        // replayed — so a failure that healed while we were disconnected would
        // otherwise leave the banner standing. Re-read the stored status.
        // Riding on the same condition as the log line keeps this off the hot
        // path of a flapping stream, which must not also become a request storm
        // against the origin endpoint.
        syncRemoteError(state.repo);
      }
      loggedDisconnect = false;
    });
    es.addEventListener('error', () => {
      if (loggedDisconnect) return;
      loggedDisconnect = true;
      logOutage(es.readyState === EventSource.CLOSED
        ? '[events] stream closed — head pill may be stale'
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
  // is edge-triggered — a remote event, a reconnect, a repo switch, a manager
  // close — and none of those fire for a stream that stalls SILENTLY (a slept
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
  useEffect(() => {
    if (!remoteError) return;
    let cancelled = false;
    const id = setInterval(() => syncRemoteError(state.repo, () => !cancelled), REMOTE_RECHECK_MS);
    return () => { cancelled = true; clearInterval(id); };
  }, [remoteError, state.repo, syncRemoteError]);

  // Keyboard shortcuts
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      // Back is a WINDOW-level command, so it is checked BEFORE the two guards
      // below. Both exist to stop list keys firing in the wrong place — typing
      // "d" in the filter box must not be treated as a list shortcut, and the
      // right panel owns its own arrows — but neither reason applies to ⌘[ /
      // Alt+←: nothing types them, and going back from a focused fact is
      // exactly when you want them. Backspace/Delete stay behind the guards,
      // because those DO type.
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
  }, [navigate, state, tt]);

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
  const openRepoMgr = useCallback(() => setRepoMgrOpen(true), []);
  const closeRepoMgr = useCallback(() => {
    setRepoMgrOpen(false);
    // The manager is where a remote is repaired, replaced, or disconnected —
    // exactly the state the banner reports. Re-read it on the way out instead
    // of leaving the user to wonder why the banner outlived the fix.
    syncRemoteError(stateRef.current.repo);
  }, [syncRemoteError]);
  const jumpTrail = useCallback((i: number) => {
    // Crumbs map 1:1 to navStack hops since the live root, so jumping to crumb i
    // means unwinding (depth - i) entries — pop, don't push. Reads the CURRENT
    // state through the ref so the callback identity can stay stable.
    const depth = selectTrail(stateRef.current).length - 1; // index of the current crumb
    for (let k = 0; k < depth - i; k++) dispatch({ type: 'NAV_BACK' });
  }, [dispatch]);
  const onRepoMgrChanged = useCallback(() => {
    // Re-sync after a repo/lens mutation. In a lens context this re-resolves the
    // browsed lens (so edited mounts refresh state.lens) or falls back with a
    // notice if it was deleted; in a repo context it switches off an
    // archived/removed active repo (I4).
    void refreshContextAfterChange(dispatch, stateRef.current.context, stateRef.current.repo, {}, isCurrentLens)
      .then(({ lenses: ls, repos: rs }) => { setLenses(ls); setRepos(rs); })
      .catch(() => {});
  }, [dispatch, isCurrentLens]);
  const onRepoMgrBrowse = useCallback((ctx: BrowseContext) => {
    // Switch the browse surface and close the manager. Lens resolution is owned
    // by the lensResolutionPending effect.
    setRepoMgrOpen(false);
    dispatch({ type: 'SET_CONTEXT', context: ctx });
  }, [dispatch]);

  if (reposLoaded && repos.length === 0) {
    return (
      <div data-testid="no-repos" style={{ display: 'flex', flexDirection: 'column', gap: 8, alignItems: 'center', justifyContent: 'center', height: '100vh', width: '100vw', background: '#141414', color: '#888', fontFamily: 'var(--k-font-body)' }}>
        <div>No repositories found.</div>
        <div style={{ fontSize: 12, color: '#666' }}>Create one with <code style={{ color: '#7c9' }}>knomit init</code>, then reload.</div>
      </div>
    );
  }

  if (!state.branch) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100vh', width: '100vw', background: '#141414', color: '#888', fontFamily: 'var(--k-font-body)' }}>
        Loading…
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'var(--k-font-body)', overflow: 'hidden' }}>
      {/* Every top-level panel gets its own INLINE boundary: one bad fact body
          or a malformed lens doc now blanks a single pane instead of the SPA.
          The overlay variant is reserved for the repo manager below, which
          already owns the screen when it is open. */}
      <ErrorBoundary variant="inline" label="The top bar hit an error">
        {/* Search rides in the chrome row. It governs the fact LIST, and it
            used to render as a band above the right pane — which reads
            state.filters exactly zero times. History mode passes nothing: the
            trail breadcrumb takes that job below, and there is no filtering
            while anchored. */}
        <TopBar state={state} repos={repos} lenses={lenses} dispatch={dispatch} onManageRepos={openRepoMgr} leftWidth={leftPanelWidth}
          search={isLive(state) ? (
            <ErrorBoundary variant="inline" label="Search hit an error">
              <FilterBar state={state} dispatch={dispatch} embedded />
            </ErrorBoundary>
          ) : undefined} />
      </ErrorBoundary>
      {state.indexState === 'indexing' && (
        <div data-testid="indexing-banner" style={{ background: '#1c2b1c', color: '#9c9', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #2a3a2a', flexShrink: 0, display: 'flex', alignItems: 'center', gap: 8 }}>
          <span>⟳ Indexing{state.indexTotal > 0 ? ` ${state.indexPercent}% (${state.indexDone}/${state.indexTotal})` : '…'}</span>
          <span style={{ color: '#6a8a6a' }}>search and lists may be incomplete until this finishes</span>
        </div>
      )}
      {state.indexState === 'error' && (
        <div data-testid="index-error-banner" style={{ background: '#2b1c1c', color: '#e0a0a0', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #3a2a2a', flexShrink: 0, display: 'flex', alignItems: 'center', gap: 8 }}>
          <span>⚠ Indexing did not complete — search and lists may be incomplete. It will retry on the next restart.</span>
        </div>
      )}
      {state.notice && (
        <div data-testid="notice" style={{ background: '#2a200e', color: '#f5c47a', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #a36a18', flexShrink: 0 }}>
          {state.notice}
        </div>
      )}
      {/* The banner reports a REMEMBERED failure, so it needs a way out that
          does not depend on the next remote event arriving. Both buttons clear
          it: acting on it (Reconnect…) and acknowledging it (✕). Nothing is
          lost by clearing — the repo manager's remote card still shows the
          stored "✗ sync failed" line, and a remote that is still broken raises
          the banner again on the next failing tick. */}
      {remoteError && (
        <div data-testid="remote-error-banner" style={{ background: '#2b1c1c', color: '#e0a0a0', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #3a2a2a', flexShrink: 0, display: 'flex', alignItems: 'center', gap: 10 }}>
          <span>⚠ Remote sync failed — {remoteError}</span>
          <button
            type="button"
            data-testid="remote-error-reconnect"
            onClick={() => { dispatch({ type: 'CLEAR_REMOTE_ERRORS' }); setRepoMgrOpen(true); }}
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
      <ErrorBoundary label="The repo manager hit an error" onReset={closeRepoMgr}>
        <RepoManager
          open={repoMgrOpen}
          repos={repos}
          currentRepo={state.repo}
          readOnly={isReadOnly(state)}
          hideRemoteConfig={state.serverReadOnly}
          onClose={closeRepoMgr}
          onChanged={onRepoMgrChanged}
          onBrowse={onRepoMgrBrowse}
        />
      </ErrorBoundary>

      {/* Unified now/history surface: a rotating LeftPanel (Library ⇄ timeline
          nav), a trail-aware FilterBar, and the fact RightPanel — which carries
          the connections panel in its header. Time-travel (scrub/hop/
          return-to-now) routes through `tt` so the same layout serves live and
          history reads. */}
      <div style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
          <div style={{ width: leftPanelWidth, flexShrink: 0, overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
            <ErrorBoundary variant="inline" label="The library hit an error">
              <LeftPanel state={state} dispatch={dispatch} navigate={navigate} onScrub={tt.scrub} onOpenFileAt={tt.openFileAt} onReturnToLive={tt.returnToNow} narrow={libraryNarrow} />
            </ErrorBoundary>
          </div>
          {/* Drag handle. 4px visible separator + 8px hit zone via negative
              margins on either side so the cursor target is easier to grab
              than the visible line. */}
          <div
            data-testid="library-splitter"
            onMouseDown={startSplitterDrag}
            title="Drag to resize"
            style={{
              width: 4, marginLeft: -2, marginRight: -2,
              cursor: 'ew-resize', flexShrink: 0, zIndex: 1,
              background: 'transparent',
              borderLeft: '1px solid #222',
            }}
            onMouseEnter={e => { (e.currentTarget as HTMLElement).style.background = 'rgba(136,170,255,0.15)'; }}
            onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = 'transparent'; }}
          />
          <div style={{ flex: 1, overflow: 'hidden', minWidth: 0, display: 'flex', flexDirection: 'column' }}>
            {/* Filter bar lives over the content pane only, so the fact-list
                column runs clean to the splitter. When history it swaps to the
                trail breadcrumb. */}
            {/* Anchored reads swap the filter input for the trail breadcrumb.
                It stays over the content pane because it describes the hop
                path that got you to the open FACT, not the list. */}
            {!isLive(state) && (
              <ErrorBoundary variant="inline" label="The trail bar hit an error">
                <FilterBar state={state} dispatch={dispatch} onJumpTrail={jumpTrail} />
              </ErrorBoundary>
            )}
            <div style={{ flex: 1, minHeight: 0, display: 'flex', overflow: 'hidden' }}>
              <div style={{ flex: 1, minWidth: 0, overflow: 'hidden' }}>
                <ErrorBoundary variant="inline" label="This fact could not be displayed">
                  <RightPanel
                    state={state}
                    dispatch={dispatch}
                    navigate={navigate}
                    onScrub={tt.scrub}
                    onHopRef={tt.hopEdge}
                    repoNames={repoNames}
                    refCommits={edges.refCommits}
                    incoming={edges.incoming}
                    outgoing={edges.outgoing}
                    edgesError={edges.error}
                    onHopEdge={tt.hopEdge}
                  />
                </ErrorBoundary>
              </div>
            </div>
          </div>
        </div>
        <ErrorBoundary variant="inline" label="The status footer hit an error">
          <StatusFooter state={state} version={version}
            searchKey={isLive(state)} />
        </ErrorBoundary>
      </div>
    </div>
  );
}
