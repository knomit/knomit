import { useReducer, useEffect } from 'react';
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
    api.status().then(s => dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled })).catch(() => {});
  }, []);

  // Show last two path segments with ellipsis prefix if deeper
  const pathParts = state.currentPath.split('/');
  const shortPath = pathParts.length > 2
    ? '…/' + pathParts.slice(-2).join('/')
    : state.currentPath;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif', overflow: 'hidden' }}>
      <TopBar state={state} dispatch={dispatch} />

      {/* Path bar: [repo][branch] .../path  commit */}
      <div style={{ height: 30, background: '#111', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 12px', gap: 8, flexShrink: 0, overflow: 'hidden' }}>
        <span style={{ color: '#7c9', fontSize: 11, background: '#1a2a1a', border: '1px solid #2a4a2a', borderRadius: 3, padding: '1px 6px', whiteSpace: 'nowrap' }}>knomit</span>
        {state.branch && (
          <span style={{ color: '#8af', fontSize: 11, background: '#1a1a2a', border: '1px solid #2a2a4a', borderRadius: 3, padding: '1px 6px', whiteSpace: 'nowrap' }}>{state.branch}</span>
        )}
        <span
          onClick={() => dispatch({ type: 'NAVIGATE', path: state.currentPath })}
          style={{ color: '#aaa', fontSize: 12, cursor: 'pointer', flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {shortPath}
        </span>
        {state.headCommit && (
          <code style={{ color: '#444', fontSize: 11, whiteSpace: 'nowrap' }}>{state.headCommit.slice(0, 7)}</code>
        )}
      </div>

      <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
        <div style={{ width: '35%', minWidth: 180, maxWidth: '50%', borderRight: '1px solid #222', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
          <LeftPanel state={state} dispatch={dispatch} />
        </div>
        <div style={{ flex: 1, overflow: 'hidden', minWidth: 0 }}>
          <RightPanel state={state} dispatch={dispatch} />
        </div>
      </div>
      <StatusBar state={state} />
    </div>
  );
}
