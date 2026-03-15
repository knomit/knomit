import { useReducer, useEffect } from 'react';
import { reducer, init } from './state';
import { api } from './api';
import { TopBar } from './TopBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { Console } from './Console';
import './App.css';

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);

  // Load initial status
  useEffect(() => {
    api.status().then(s => dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled, ontologyRoot: s.ontology_root })).catch(() => {});
  }, []);

  // SSE for task and status events
  useEffect(() => {
    const es = new EventSource('/api/v1/events');
    es.addEventListener('task', (e) => {
      const ev = JSON.parse(e.data);
      dispatch({ type: 'SET_TASK', op: ev.op, status: ev.status, message: ev.message || '' });
      const level = ev.status === 'error' ? 'error' as const : 'info' as const;
      dispatch({ type: 'CONSOLE_LOG', level, message: `[${ev.op}] ${ev.message || ev.status}` });
      // Refresh head when a task completes — the SSE status event may be
      // buffered by the Vite proxy, so fetch it explicitly.
      if (ev.status === 'done' || ev.status === 'error') {
        api.status().then(s => dispatch({ type: 'SET_HEAD', head: s.head })).catch(() => {});
      }
    });
    es.addEventListener('status', (e) => {
      const s = JSON.parse(e.data);
      if (s.head) dispatch({ type: 'SET_HEAD', head: s.head });
    });
    return () => es.close();
  }, []);

  // Build breadcrumb segments from currentPath
  const pathParts = state.currentPath.split('/').filter(Boolean);
  const breadcrumbs = pathParts.map((seg, i) => ({
    label: seg,
    path: pathParts.slice(0, i + 1).join('/'),
  }));

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif', overflow: 'hidden' }}>
      <TopBar state={state} />

      {/* Breadcrumb path bar + action buttons */}
      <div style={{ height: 30, background: '#111', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 8px', gap: 2, flexShrink: 0, overflow: 'hidden' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 2, flex: 1, overflow: 'hidden', minWidth: 0 }}>
          <span style={{ color: '#444', fontSize: 12, flexShrink: 0, marginRight: 2 }}>⟩</span>
          {breadcrumbs.map((crumb, i) => (
            <span key={crumb.path} style={{ display: 'flex', alignItems: 'center', gap: 2, minWidth: 0, overflow: 'hidden' }}>
              {i > 0 && <span style={{ color: '#444', fontSize: 12, flexShrink: 0 }}>/</span>}
              <span
                onClick={() => dispatch({ type: 'NAVIGATE', path: crumb.path })}
                style={{
                  color: i === breadcrumbs.length - 1 ? '#ccc' : '#666',
                  fontSize: 12,
                  cursor: 'pointer',
                  whiteSpace: 'nowrap',
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  padding: '1px 4px',
                  borderRadius: 3,
                }}
                onMouseEnter={e => (e.currentTarget.style.color = '#eee')}
                onMouseLeave={e => (e.currentTarget.style.color = i === breadcrumbs.length - 1 ? '#ccc' : '#666')}
              >
                {crumb.label}
              </span>
            </span>
          ))}
        </div>
      </div>

      <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
        <div style={{ width: '35%', minWidth: 180, maxWidth: '50%', borderRight: '1px solid #222', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
          <LeftPanel state={state} dispatch={dispatch} />
        </div>
        <div style={{ flex: 1, overflow: 'hidden', minWidth: 0 }}>
          <RightPanel state={state} dispatch={dispatch} />
        </div>
      </div>
      <Console state={state} dispatch={dispatch} />
    </div>
  );
}
