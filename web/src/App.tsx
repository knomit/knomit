import { Fragment, useReducer, useEffect } from 'react';
import { reducer, init } from './state';
import { api } from './api';
import { TopBar } from './TopBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { StatusBar } from './StatusBar';
import './App.css';

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);

  // Load initial status
  useEffect(() => {
    api.status().then(s => dispatch({ type: 'SET_STATUS', head: s.head, embeddingsEnabled: s.embeddings_enabled })).catch(() => {});
  }, []);

  const breadcrumbs = state.currentPath.split('/');

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif' }}>
      <TopBar state={state} dispatch={dispatch} />

      {/* Path bar: current path left, commit hash right */}
      <div style={{ height: 32, background: '#111', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 16px', flexShrink: 0 }}>
        <div style={{ display: 'flex', gap: 4, alignItems: 'center', flex: 1 }}>
          {breadcrumbs.map((seg, i) => (
            <Fragment key={i}>
              {i > 0 && <span style={{ color: '#333' }}>/</span>}
              <span
                onClick={() => dispatch({ type: 'NAVIGATE', path: breadcrumbs.slice(0, i + 1).join('/') })}
                style={{ color: i === breadcrumbs.length - 1 ? '#ccc' : '#666', cursor: 'pointer', fontSize: 12, padding: '1px 3px', borderRadius: 3 }}>
                {seg}
              </span>
            </Fragment>
          ))}
        </div>
        {state.headCommit && (
          <code style={{ color: '#444', fontSize: 11 }}>{state.headCommit.slice(0, 7)}</code>
        )}
      </div>

      <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
        <div style={{ width: '35%', minWidth: 200, borderRight: '1px solid #222', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
          <LeftPanel state={state} dispatch={dispatch} />
        </div>
        <div style={{ flex: 1, overflow: 'hidden' }}>
          <RightPanel state={state} dispatch={dispatch} />
        </div>
      </div>
      <StatusBar state={state} />
    </div>
  );
}
