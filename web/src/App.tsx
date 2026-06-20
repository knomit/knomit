import { useReducer, useEffect, useState } from 'react';
import { reducer, init, isReadOnly, isLive, selectTrail, selectAnchorCommit } from './state';
import { api, apiUrl } from './api';
import { useNavigationManager } from './useNavigationManager';
import { useTimeTravel } from './useTimeTravel';
import { bootstrapStatusWithRetry } from './bootstrap';
import { pickRepo, loadLastRepo, saveLastRepo } from './repoSelection';
import type { RepoInfo } from './api';
import { TopBar } from './TopBar';
import { RepoManager } from './RepoManager';
import { ErrorBoundary } from './ErrorBoundary';
import { FilterBar } from './FilterBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { EdgesRail } from './EdgesRail';
import { Console } from './Console';
import './App.css';

// Library | RightPanel splitter sizing. Persisted to localStorage so the
// width survives reloads. Clamped on read + on every drag step.
const LEFT_PANEL_MIN = 180;
const LEFT_PANEL_MAX_FRACTION = 0.6;       // never let the left panel exceed 60% of the viewport
const LEFT_PANEL_DEFAULT_FRACTION = 0.35;  // matches the previous fixed 35% width
const LEFT_PANEL_STORAGE_KEY = 'knomit.leftPanelWidth';

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

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);
  const { navigate } = useNavigationManager(state, dispatch);
  const [repos, setRepos] = useState<RepoInfo[]>([]);
  const [reposLoaded, setReposLoaded] = useState(false);
  const [repoMgrOpen, setRepoMgrOpen] = useState(false);

  // Time-travel callbacks (scrub / hop / open-at / return-to-now), backed by
  // the reducer. EdgesRail + RightPanel + LeftPanel + FilterBar all route their
  // navigation through these so a single action model drives now and history.
  const tt = useTimeTravel(state, dispatch);

  // The anchor for EdgesRail and ref-hops: the scrubbed/diff anchor when not
  // live, else the repo HEAD commit. Reading a fact/edges at HEAD resolves the
  // fact's current version — correct immediately on load, no callback needed.
  const liveEdgeAnchor = selectAnchorCommit(state) ?? state.headCommit;

  // Splitter between Library (left) and RightPanel. Width restored from
  // localStorage on mount; persisted on drag-end so transient frames during a
  // drag don't thrash localStorage.
  const [leftPanelWidth, setLeftPanelWidth] = useState<number>(() => loadLeftPanelWidth());
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
  // assume the default ("trunk") still exists. pickRepo derives the selection
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
        const next = pickRepo('', list, loadLastRepo());
        if (next) dispatch({ type: 'SET_REPO', repo: next });
      })
      .catch(() => { if (!cancelled) setReposLoaded(true); });
    return () => { cancelled = true; };
  }, []);

  // Remember the user's repo choice so reloads land on the same repo.
  useEffect(() => {
    saveLastRepo(state.repo);
  }, [state.repo]);

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
      onSuccess: (s) => {
        dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled, ontologyRoot: s.ontology_root, indexState: s.index_state, indexDone: s.index_done, indexTotal: s.index_total, indexPercent: s.index_percent });
      },
      onAttemptFailed: (err, attempt) => {
        dispatch({
          type: 'CONSOLE_LOG',
          level: 'error',
          message: `[bootstrap] attempt ${attempt + 1} failed: ${String(err)}`,
        });
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
        .then(s => { if (!cancelled) dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled, ontologyRoot: s.ontology_root, indexState: s.index_state, indexDone: s.index_done, indexTotal: s.index_total, indexPercent: s.index_percent }); })
        .catch(() => {});
    }, 2000);
    return () => { cancelled = true; clearInterval(id); };
  }, [state.indexState, state.repo, state.branch]);

  // SSE for task and status events — reconnects when repo/branch changes.
  useEffect(() => {
    if (!state.branch) return; // wait until branch is known from status bootstrap
    const es = new EventSource(apiUrl(`/api/v1/repos/${state.repo}/branches/${state.branch.replaceAll('/', ':')}/events`));
    let connected = false;
    es.addEventListener('open', () => {
      if (connected) {
        dispatch({ type: 'CONSOLE_LOG', level: 'info', message: '[events] reconnected' });
      }
      connected = true;
    });
    // EventSource silently auto-reconnects on disconnect. Without this handler,
    // a backend that 500s the stream produces a stale LIVE/SCRUBBED pill (no
    // SET_HEAD updates arrive) with no signal to the user. Log once per outage.
    let loggedDisconnect = false;
    es.addEventListener('error', () => {
      if (es.readyState === EventSource.CLOSED) {
        if (!loggedDisconnect) {
          dispatch({ type: 'CONSOLE_LOG', level: 'error', message: '[events] stream closed — head pill may be stale' });
          loggedDisconnect = true;
        }
      } else if (!loggedDisconnect) {
        dispatch({ type: 'CONSOLE_LOG', level: 'error', message: '[events] connection lost — retrying' });
        loggedDisconnect = true;
      }
    });
    es.addEventListener('task', (e) => {
      const ev = JSON.parse(e.data);
      dispatch({ type: 'SET_TASK', op: ev.op, status: ev.status, message: ev.message || '' });
      const level = ev.status === 'error' ? 'error' as const : 'info' as const;
      const repo = ev.repo ? `${ev.repo}/` : '';
      dispatch({ type: 'CONSOLE_LOG', level, message: `[${repo}${ev.op}] ${ev.message || ev.status}` });
      // Refresh head when a task completes.
      if (ev.status === 'done' || ev.status === 'error') {
        api.status(state.repo, state.branch)
          .then(s => dispatch({ type: 'SET_HEAD', head: s.head }))
          .catch(err => dispatch({ type: 'CONSOLE_LOG', level: 'error', message: `[status] refresh failed: ${String(err)}` }));
      }
    });
    es.addEventListener('status', (e) => {
      const s = JSON.parse(e.data);
      if (s.head) dispatch({ type: 'SET_HEAD', head: s.head });
    });
    const handleRemoteEvent = (e: MessageEvent) => {
      const ev = JSON.parse(e.data);
      if (ev.error) {
        dispatch({ type: 'SET_REMOTE_ERROR', error: ev.error });
        dispatch({ type: 'CONSOLE_LOG', level: 'error', message: `[remote] ${ev.error}` });
        return;
      }
      dispatch({ type: 'SET_REMOTE_ERROR', error: '' });
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
          dispatch({ type: 'CONSOLE_LOG', level: 'info', message: `[remote] ${parts.join(', ')}` });
        }
      }
    };
    es.addEventListener('sync_ok', handleRemoteEvent);
    es.addEventListener('sync_error', handleRemoteEvent);
    es.addEventListener('push_ok', handleRemoteEvent);
    es.addEventListener('push_error', handleRemoteEvent);
    return () => es.close();
  }, [state.repo, state.branch]);

  // Keyboard shortcuts
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const tag = (document.activeElement as HTMLElement)?.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

      if (e.key === '/') {
        e.preventDefault();
        document.getElementById('filter-input')?.focus();
        return;
      }
      if (e.key === 'Escape') {
        // While scrubbed, Escape returns to now (the read-only excursion is the
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

  // Transient amber notice (e.g. "fact was retracted — returned to now").
  // Auto-clears after ~6s; the effect re-arms whenever a new notice is set.
  useEffect(() => {
    if (!state.notice) return;
    const id = setTimeout(() => dispatch({ type: 'CLEAR_NOTICE' }), 6000);
    return () => clearTimeout(id);
  }, [state.notice]);

  if (reposLoaded && repos.length === 0) {
    return (
      <div data-testid="no-repos" style={{ display: 'flex', flexDirection: 'column', gap: 8, alignItems: 'center', justifyContent: 'center', height: '100vh', width: '100vw', background: '#141414', color: '#888', fontFamily: 'system-ui, sans-serif' }}>
        <div>No repositories found.</div>
        <div style={{ fontSize: 12, color: '#666' }}>Create one with <code style={{ color: '#7c9' }}>knomit init</code>, then reload.</div>
      </div>
    );
  }

  if (!state.branch) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100vh', width: '100vw', background: '#141414', color: '#888', fontFamily: 'system-ui, sans-serif' }}>
        Loading…
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif', overflow: 'hidden' }}>
      <TopBar state={state} repos={repos} dispatch={dispatch} onManageRepos={() => setRepoMgrOpen(true)} leftWidth={leftPanelWidth} />
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
      <ErrorBoundary label="The repo manager hit an error" onReset={() => setRepoMgrOpen(false)}>
        <RepoManager
          open={repoMgrOpen}
          repos={repos}
          currentRepo={state.repo}
          readOnly={isReadOnly(state)}
          onClose={() => setRepoMgrOpen(false)}
          onChanged={() => {
            api.repos().then(list => {
              setRepos(list);
              // If the active repo was archived/removed, switch to a remaining
              // one (prefer trunk) so the app never points at a gone repo.
              if (list.length && !list.some(r => r.name === state.repo)) {
                const next = list.find(r => r.name === 'trunk') ?? list[0];
                dispatch({ type: 'SET_REPO', repo: next.name });
              }
            }).catch(() => {});
          }}
        />
      </ErrorBoundary>

      {/* Unified now/history surface: a rotating LeftPanel (Library ⇄ timeline
          nav), a trail-aware FilterBar, the fact RightPanel, and — when a fact
          is open — the EdgesRail connections column. Time-travel (scrub/hop/
          return-to-now) routes through `tt` so the same layout serves live and
          scrubbed reads. */}
      <div style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
          <div style={{ width: leftPanelWidth, flexShrink: 0, overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
            <LeftPanel state={state} dispatch={dispatch} navigate={navigate} onScrub={tt.scrub} onOpenFileAt={tt.openFileAt} />
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
                column runs clean to the splitter. When scrubbed it swaps to the
                trail breadcrumb. */}
            <FilterBar
              state={state}
              dispatch={dispatch}
              onJumpTrail={(i) => {
                const c = selectTrail(state)[i];
                if (c) dispatch({ type: 'APPLY_NAV', view: 'library', factPath: c.factPath, asOf: c.asOf });
              }}
              onReturnToNow={tt.returnToNow}
            />
            <div style={{ flex: 1, minHeight: 0, display: 'flex', overflow: 'hidden' }}>
              <div style={{ flex: 1, minWidth: 0, overflow: 'hidden' }}>
                <RightPanel
                  state={state}
                  dispatch={dispatch}
                  onScrub={tt.scrub}
                  onHopRef={(p) => tt.hopEdge(p, liveEdgeAnchor)}
                />
              </div>
              {state.factPath && (
                <EdgesRail
                  repo={state.repo}
                  branch={state.branch}
                  factPath={state.factPath}
                  anchorCommit={liveEdgeAnchor}
                  scrubbed={!isLive(state)}
                  onHop={tt.hopEdge}
                />
              )}
            </div>
          </div>
        </div>
        <Console state={state} dispatch={dispatch} />
      </div>

    </div>
  );
}
