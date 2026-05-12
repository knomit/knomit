import { useReducer, useEffect, useState } from 'react';
import { reducer, init, isReadOnly, isLive } from './state';
import { api } from './api';
import { useNavigationManager } from './useNavigationManager';
import { bootstrapStatusWithRetry } from './bootstrap';
import type { RepoInfo } from './api';
import { TopBar } from './TopBar';
import { Breadcrumb } from './Breadcrumb';
import { FilterBar } from './FilterBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { Console } from './Console';
import { ConnectRemoteModal } from './ConnectRemoteModal';
import { ExplainView } from './ExplainView';
import './App.css';

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);
  const { navigate } = useNavigationManager(state, dispatch);
  const [repos, setRepos] = useState<RepoInfo[]>([]);
  const [showOrigin, setShowOrigin] = useState(false);
  const [explainEntry, setExplainEntry] = useState<{ path: string; commit: string | null } | null>(null);

  // Fetch repos list on mount.
  useEffect(() => {
    api.repos().then(setRepos).catch(() => {});
  }, []);

  // Load status when repo changes (also fires on mount). Bootstrap fetches the
  // agent branch then the branch root for full status, retrying with
  // exponential backoff: a single transient hiccup (dev proxy first-request
  // hang, brief network blip, backend just-restarted) would otherwise leave
  // the page stuck on "Loading…" because this effect only re-fires when
  // state.repo changes. Each failed attempt is logged to the Console so a
  // permanently broken backend is visible instead of silent.
  useEffect(() => {
    let cancelled = false;
    bootstrapStatusWithRetry({
      repo: state.repo,
      initialBranch: state.branch,
      getAgentBranch: api.getAgentBranch,
      getStatus: api.status,
      onSuccess: (s) => {
        dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled, ontologyRoot: s.ontology_root });
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

  // SSE for task and status events — reconnects when repo/branch changes.
  useEffect(() => {
    if (!state.branch) return; // wait until branch is known from status bootstrap
    const es = new EventSource(`/api/v1/repos/${state.repo}/branches/${state.branch.replaceAll('/', ':')}/events`);
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
      } else {
        dispatch({ type: 'SET_REMOTE_ERROR', error: '' });
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
        dispatch({ type: 'CLEAR_FILTERS' });
        return;
      }
      if (e.key === 'Backspace' || e.key === 'Delete') {
        e.preventDefault();
        dispatch({ type: 'NAV_BACK' });
        return;
      }
      if (e.key === 'h') {
        e.preventDefault();
        if (!isLive(state)) dispatch({ type: 'SET_AS_OF', asOf: { mode: 'live' } });
        return;
      }
      if (e.key === '1') { e.preventDefault(); navigate({ view: 'tree' }); return; }
      if (e.key === '2') { e.preventDefault(); navigate({ view: 'chrono' }); return; }
      if (e.key === '3') { e.preventDefault(); navigate({ view: 'history' }); return; }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [navigate, state]);

  if (!state.branch) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '100vh', width: '100vw', background: '#141414', color: '#888', fontFamily: 'system-ui, sans-serif' }}>
        Loading…
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif', overflow: 'hidden' }}>
      <TopBar state={state} repos={repos} dispatch={dispatch} onSettingsClick={() => setShowOrigin(true)} />
      {explainEntry ? (
        <div style={{ flex: 1, minHeight: 0, overflow: 'hidden' }}>
          <ExplainView
            repo={state.repo}
            branch={state.branch}
            initialEntry={explainEntry}
            onClose={() => setExplainEntry(null)}
          />
        </div>
      ) : (
        <>
          <Breadcrumb state={state} dispatch={dispatch} navigate={navigate} />
          <FilterBar state={state} dispatch={dispatch} />

          <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
            <div style={{ width: '35%', minWidth: 180, maxWidth: '50%', borderRight: '1px solid #222', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
              <LeftPanel state={state} dispatch={dispatch} navigate={navigate} />
            </div>
            <div style={{ flex: 1, overflow: 'hidden', minWidth: 0 }}>
              <RightPanel state={state} dispatch={dispatch} navigate={navigate} onExplain={(path, commit) => setExplainEntry({ path, commit })} />
            </div>
          </div>
          <Console state={state} dispatch={dispatch} />
        </>
      )}
      {showOrigin && !isReadOnly(state) && <ConnectRemoteModal repo={state.repo} onClose={() => setShowOrigin(false)} />}
    </div>
  );
}
