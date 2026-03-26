import { useReducer, useEffect, useState } from 'react';
import { reducer, init } from './state';
import { api } from './api';
import { useNavigationManager } from './useNavigationManager';
import type { RepoInfo } from './api';
import { TopBar } from './TopBar';
import { Breadcrumb } from './Breadcrumb';
import { FilterBar } from './FilterBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { Console } from './Console';
import { ConnectRemoteModal } from './ConnectRemoteModal';
import './App.css';

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);
  const { navigate } = useNavigationManager(state, dispatch);
  const [repos, setRepos] = useState<RepoInfo[]>([]);
  const [showOrigin, setShowOrigin] = useState(false);

  // Fetch repos list on mount.
  useEffect(() => {
    api.repos().then(setRepos).catch(() => {});
  }, []);

  // Load status when repo changes (also fires on mount).
  useEffect(() => {
    api.status(state.repo).then(s =>
      dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled, ontologyRoot: s.ontology_root })
    ).catch(() => {});
  }, [state.repo]);

  // SSE for task and status events — reconnects when repo changes.
  useEffect(() => {
    const es = new EventSource(`/api/v1/${state.repo}/events`);
    es.addEventListener('task', (e) => {
      const ev = JSON.parse(e.data);
      dispatch({ type: 'SET_TASK', op: ev.op, status: ev.status, message: ev.message || '' });
      const level = ev.status === 'error' ? 'error' as const : 'info' as const;
      const repo = ev.repo ? `${ev.repo}/` : '';
      dispatch({ type: 'CONSOLE_LOG', level, message: `[${repo}${ev.op}] ${ev.message || ev.status}` });
      // Refresh head when a task completes.
      if (ev.status === 'done' || ev.status === 'error') {
        api.status(state.repo).then(s => dispatch({ type: 'SET_HEAD', head: s.head })).catch(() => {});
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
  }, [state.repo]);

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
      if (e.key === '1') { e.preventDefault(); navigate({ view: 'tree' }); return; }
      if (e.key === '2') { e.preventDefault(); navigate({ view: 'chrono' }); return; }
      if (e.key === '3') { e.preventDefault(); navigate({ view: 'history' }); return; }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [navigate]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif', overflow: 'hidden' }}>
      <TopBar state={state} repos={repos} dispatch={dispatch} onSettingsClick={() => setShowOrigin(true)} />
      <Breadcrumb state={state} dispatch={dispatch} navigate={navigate} />
      <FilterBar state={state} dispatch={dispatch} />

      <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
        <div style={{ width: '35%', minWidth: 180, maxWidth: '50%', borderRight: '1px solid #222', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
          <LeftPanel state={state} dispatch={dispatch} navigate={navigate} />
        </div>
        <div style={{ flex: 1, overflow: 'hidden', minWidth: 0 }}>
          <RightPanel state={state} dispatch={dispatch} navigate={navigate} />
        </div>
      </div>
      <Console state={state} dispatch={dispatch} />
      {showOrigin && <ConnectRemoteModal repo={state.repo} onClose={() => setShowOrigin(false)} />}
    </div>
  );
}
