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
    api.status().then(s => dispatch({ type: 'SET_STATUS', head: s.head, embeddingsEnabled: s.embeddings_enabled })).catch(() => {});
  }, []);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif' }}>
      <TopBar state={state} dispatch={dispatch} />
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
