import { useEffect, useRef } from 'react';
import type { AppState } from './state';

interface Props {
  state: AppState;
  dispatch: (action: { type: 'SET_TASK'; op: string; status: 'idle' | 'running' | 'done' | 'error'; message: string }) => void;
}

export function StatusBar({ state, dispatch }: Props) {
  const clearTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const activeTask = Object.entries(state.tasks).find(([, t]) => t.status === 'running');
  const errorTask = Object.entries(state.tasks).find(([, t]) => t.status === 'error');
  const doneTask = Object.entries(state.tasks).find(([, t]) => t.status === 'done');

  const displayTask = activeTask || errorTask || doneTask;
  const [taskOp, taskState] = displayTask || [null, null];

  useEffect(() => {
    clearTimeout(clearTimer.current);
    if (taskState?.status === 'done' && taskOp) {
      clearTimer.current = setTimeout(() => {
        dispatch({ type: 'SET_TASK', op: taskOp, status: 'idle', message: '' });
      }, 4000);
    }
    return () => clearTimeout(clearTimer.current);
  }, [taskOp, taskState?.status]);

  const taskMsg = taskState?.message || '';
  const taskColor = taskState?.status === 'done' ? '#8c8' : taskState?.status === 'error' ? '#c66' : '#888';

  const msg = taskMsg || state.statusMessage || (state.loading ? 'Loading\u2026' : '');
  const color = taskMsg ? taskColor : '#888';

  return (
    <div style={{ height: 24, background: '#0d0d0d', borderTop: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 12px', flexShrink: 0, gap: 8 }}>
      {msg && <span style={{ color, fontSize: 11, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{msg}</span>}
      <div style={{ flex: 1 }} />
      <span
        title={state.embeddingsEnabled ? 'Embeddings on' : 'Embeddings off'}
        style={{ color: state.embeddingsEnabled ? '#8c8' : '#555', fontSize: 11, fontFamily: 'monospace', fontWeight: 'bold', userSelect: 'none' }}>
        e
      </span>
    </div>
  );
}
