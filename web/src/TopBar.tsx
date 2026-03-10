import { Fragment } from 'react';
import type { Dispatch } from 'react';
import type { AppState, Action } from './state';
import { api } from './api';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function TopBar({ state, dispatch }: Props) {
  const breadcrumbs = state.currentPath.split('/');

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
    <div style={{ height: 48, background: '#111', borderBottom: '1px solid #333', display: 'flex', alignItems: 'center', padding: '0 16px', gap: 16, flexShrink: 0 }}>
      <span style={{ color: '#7c9', fontWeight: 'bold', fontSize: 16 }}>knomit</span>
      <div style={{ display: 'flex', gap: 4, alignItems: 'center', flex: 1 }}>
        {breadcrumbs.map((seg, i) => (
          <Fragment key={i}>
            {i > 0 && <span style={{ color: '#444' }}>/</span>}
            <span
              onClick={() => dispatch({ type: 'NAVIGATE', path: breadcrumbs.slice(0, i + 1).join('/') })}
              style={{ color: i === breadcrumbs.length - 1 ? '#ddd' : '#888', cursor: 'pointer', fontSize: 13, padding: '2px 4px', borderRadius: 4 }}>
              {seg}
            </span>
          </Fragment>
        ))}
      </div>
      <button onClick={handleSync} disabled={state.syncing}
        style={{ background: state.syncing ? '#333' : '#2a3a2a', color: state.syncing ? '#666' : '#8c8', border: '1px solid #333', padding: '4px 12px', borderRadius: 4, cursor: state.syncing ? 'default' : 'pointer', fontSize: 12 }}>
        {state.syncing ? '⟳ Syncing…' : '⟳ Sync'}
      </button>
      {state.headCommit && (
        <code style={{ color: '#555', fontSize: 11 }}>{state.headCommit.slice(0, 7)}</code>
      )}
    </div>
  );
}
