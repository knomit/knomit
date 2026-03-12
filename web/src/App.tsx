import { useReducer, useEffect, useState, useRef, useCallback } from 'react';
import { reducer, init } from './state';
import { api } from './api';
import { TopBar } from './TopBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { StatusBar } from './StatusBar';
import './App.css';

function IconBtn({ title, onClick, disabled, children }: { title: string; onClick: () => void; disabled?: boolean; children: React.ReactNode }) {
  return (
    <button
      title={title}
      onClick={onClick}
      disabled={disabled}
      style={{ background: 'transparent', border: 'none', outline: 'none', cursor: disabled ? 'default' : 'pointer', padding: '2px 8px', borderRadius: 3, fontSize: 22, display: 'flex', alignItems: 'center', color: disabled ? '#444' : '#888', transition: 'color 0.15s, background 0.15s' }}
      onMouseEnter={e => { if (!disabled) e.currentTarget.style.color = '#7c9'; }}
      onMouseLeave={e => { if (!disabled) { e.currentTarget.style.color = '#888'; e.currentTarget.style.background = 'transparent'; } }}
      onMouseDown={e => { if (!disabled) e.currentTarget.style.background = 'rgba(119,204,153,0.1)'; }}
      onMouseUp={e => { if (!disabled) e.currentTarget.style.background = 'transparent'; }}
    >
      {children}
    </button>
  );
}

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);
  const [synthesizing, setSynthesizing] = useState(false);
  const statusTimer = useRef<ReturnType<typeof setTimeout>>();

  const showStatus = useCallback((msg: string, ms = 4000) => {
    dispatch({ type: 'SET_STATUS_MESSAGE', message: msg });
    clearTimeout(statusTimer.current);
    statusTimer.current = setTimeout(() => dispatch({ type: 'SET_STATUS_MESSAGE', message: '' }), ms);
  }, []);

  // Load initial status
  useEffect(() => {
    api.status().then(s => dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled })).catch(() => {});
  }, []);

  const handleSync = async () => {
    dispatch({ type: 'SET_SYNCING', value: true });
    showStatus('Syncing…');
    try {
      const result = await api.sync();
      if (result.commit) dispatch({ type: 'SET_STATUS', head: result.commit, branch: state.branch, embeddingsEnabled: state.embeddingsEnabled });
      showStatus(result.message || result.status || 'Sync complete');
    } catch (e) {
      showStatus(`Sync failed: ${e}`);
    } finally {
      dispatch({ type: 'SET_SYNCING', value: false });
    }
  };

  const handleSynthesize = async () => {
    setSynthesizing(true);
    showStatus('Synthesizing…');
    try {
      const result = await api.synthesize();
      showStatus(result.status || 'Synthesis started');
    } catch (e) {
      showStatus(`Synthesis failed: ${e}`);
    } finally {
      setSynthesizing(false);
    }
  };

  const handleReset = () => {
    dispatch({ type: 'NAVIGATE', path: 'know' });
    dispatch({ type: 'CLEAR_SEARCH' });
  };

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
        {/* Path-scoped action */}
        <IconBtn title="Synthesize" onClick={handleSynthesize} disabled={synthesizing}>⚗</IconBtn>
        <div style={{ width: 1, height: 16, background: '#333', flexShrink: 0 }} />
        {/* Branch-scoped actions */}
        <IconBtn title="Reset to root" onClick={handleReset}>⌂</IconBtn>
        <IconBtn title="Sync" onClick={handleSync} disabled={state.syncing}>⟳</IconBtn>
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
