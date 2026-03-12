import type { Dispatch } from 'react';
import type { AppState, Action } from './state';
import { api } from './api';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function TopBar({ state, dispatch }: Props) {
  const handleSync = async () => {
    dispatch({ type: 'SET_SYNCING', value: true });
    try {
      const result = await api.sync();
      if (result.commit) dispatch({ type: 'SET_STATUS', head: result.commit, embeddingsEnabled: state.embeddingsEnabled });
    } catch (e) {
      // ignore
    } finally {
      dispatch({ type: 'SET_SYNCING', value: false });
    }
  };

  return (
    <div style={{ height: 40, background: '#111', borderBottom: '1px solid #333', display: 'flex', alignItems: 'center', padding: '0 16px', gap: 12, flexShrink: 0 }}>
      <span style={{ color: '#7c9', fontWeight: 'bold', fontSize: 15 }}>knomit</span>
      <div style={{ flex: 1 }} />
      <button onClick={handleSync} disabled={state.syncing}
        style={{ background: 'transparent', color: state.syncing ? '#555' : '#666', border: 'none', padding: '2px 8px', borderRadius: 4, cursor: state.syncing ? 'default' : 'pointer', fontSize: 12 }}>
        {state.syncing ? '⟳ syncing…' : '⟳ sync'}
      </button>
      <span
        title={state.embeddingsEnabled ? 'Embeddings on' : 'Embeddings off'}
        style={{ color: state.embeddingsEnabled ? '#8c8' : '#555', fontSize: 12, fontFamily: 'monospace', fontWeight: 'bold', userSelect: 'none' }}>
        e
      </span>
    </div>
  );
}
