import { useEffect, useRef } from 'react';
import type { AppState } from './state';

interface Props {
  state: AppState;
  dispatch: (action: { type: 'SET_TASK'; op: string; status: 'idle' | 'running' | 'done' | 'error'; message: string }) => void;
}

export function StatusBar({ state, dispatch }: Props) {
  const clearTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Single pass: pick the highest-priority non-idle task (running > error > done).
  let taskOp: string | null = null;
  let taskState: { status: string; message: string } | null = null;
  for (const [op, t] of Object.entries(state.tasks)) {
    if (t.status === 'idle') continue;
    if (!taskState || t.status === 'running' || (t.status === 'error' && taskState.status !== 'running')) {
      taskOp = op;
      taskState = t;
    }
  }

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
    </div>
  );
}
