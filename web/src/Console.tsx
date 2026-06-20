import { useEffect, useRef, useCallback } from 'react';
import type { AppState, Action, AsOf } from './state';
import { selectTrail } from './state';
import { ChevronUpIcon, ChevronDownIcon } from './icons';

interface Props {
  state: AppState;
  dispatch: React.Dispatch<Action>;
}

function formatTime(ts: number): string {
  const d = new Date(ts);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

interface StatusFooterProps {
  asOf: AsOf;
  info: number;
  errors: number;
  task: { op: string; message: string } | null;
  onExpand: () => void;
  appState: AppState;
}

function pillContent(asOf: AsOf): { color: string; label: string; descriptor: string; glow: boolean } {
  switch (asOf.mode) {
    case 'live':
      return { color: '#7c9', label: 'LIVE', descriptor: 'HEAD', glow: true };
    case 'scrubbed':
      return { color: '#e5a23c', label: 'SCRUBBED', descriptor: asOf.commit.slice(0, 7), glow: false };
    case 'diff':
      return { color: '#e5a23c', label: 'DIFF', descriptor: `${asOf.from.slice(0, 7)}..${asOf.to.slice(0, 7)}`, glow: false };
  }
}

function Kbd({ children }: { children: string }) {
  return (
    <span style={{
      color: '#a0a0a8', background: '#16161b', padding: '0 4px',
      borderRadius: 2, border: '1px solid #1f1f26',
      fontFamily: 'JetBrains Mono, ui-monospace, monospace', fontSize: 10,
    }}>{children}</span>
  );
}

function StatusFooter({ asOf, info, errors, task, onExpand, appState }: StatusFooterProps) {
  const p = pillContent(asOf);
  const trail = selectTrail(appState);
  const trailHops = trail.length - 1; // number of hops (N)

  return (
    <div
      data-testid="console"
      onClick={onExpand}
      style={{
        height: 26, background: '#0b0b0d', borderTop: '1px solid #1f1f26',
        display: 'flex', alignItems: 'center', padding: '0 14px', gap: 10,
        flexShrink: 0, cursor: 'pointer', userSelect: 'none',
        fontFamily: 'Inter, system-ui, sans-serif', fontSize: 11,
      }}
    >
      <span style={{
        flex: '0 0 auto', display: 'flex', alignItems: 'center', gap: 8,
      }}>
        <span style={{
          width: 6, height: 6, borderRadius: '50%', background: p.color,
          boxShadow: p.glow ? `0 0 6px ${p.color}` : 'none',
        }}/>
        <span style={{
          color: p.color, letterSpacing: 1.1, fontWeight: 600,
          fontFamily: 'JetBrains Mono, ui-monospace, monospace', fontSize: 10,
        }}>{p.label}</span>
        <span style={{ color: '#a0a0a8', fontFamily: 'JetBrains Mono, ui-monospace, monospace', fontSize: 10 }}>
          {p.descriptor}
        </span>
        {asOf.mode === 'scrubbed' && (
          <span style={{ color: '#a0a0a8', fontFamily: 'JetBrains Mono, ui-monospace, monospace', fontSize: 10 }}>
            · read-only
          </span>
        )}
        {asOf.mode === 'scrubbed' && trailHops >= 1 && (
          <span style={{ color: '#e5a23c', fontFamily: 'JetBrains Mono, ui-monospace, monospace', fontSize: 10 }}>
            trail {trailHops} deep
          </span>
        )}
      </span>

      <span style={{ color: '#1f1f26', flex: '0 0 auto' }}>│</span>

      <span style={{
        flex: '1 1 auto', minWidth: 0, display: 'flex', alignItems: 'center', gap: 10,
        overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
      }}>
        <span style={{ color: '#5a5a65' }}>Console</span>
        <span style={{ color: '#a0a0a8' }}>{info}</span>
        {errors > 0 && <span style={{ color: '#f88' }}>{errors} err</span>}
        {task && (
          <span style={{
            color: '#8af',
            fontFamily: 'JetBrains Mono, ui-monospace, monospace',
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            minWidth: 0,
          }}>[{task.op}] {task.message}</span>
        )}
      </span>

      <span style={{
        flex: '0 0 auto', display: 'flex', alignItems: 'center', gap: 8,
        fontFamily: 'JetBrains Mono, ui-monospace, monospace', fontSize: 10, color: '#5a5a65',
      }}>
        <Kbd>h</Kbd> now
      </span>

      <ChevronUpIcon color="#5a5a65" size={13} />
    </div>
  );
}

export function Console({ state, dispatch }: Props) {
  const { consoleEntries, consoleOpen, consoleHeight } = state;
  const listRef      = useRef<HTMLDivElement>(null);
  const dragRef      = useRef<{ startY: number; startH: number } | null>(null);
  const heightRef    = useRef(consoleHeight);
  heightRef.current  = consoleHeight;

  const infoCount  = consoleEntries.filter(e => e.level === 'info').length;
  const errorCount = consoleEntries.filter(e => e.level === 'error').length;

  // Auto-scroll to bottom on new entries
  useEffect(() => {
    if (consoleOpen && listRef.current) {
      listRef.current.scrollTop = listRef.current.scrollHeight;
    }
  }, [consoleEntries.length, consoleOpen]);

  // Drag resize
  const onMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    dragRef.current = { startY: e.clientY, startH: heightRef.current };

    const onMove = (ev: MouseEvent) => {
      if (!dragRef.current) return;
      const delta = dragRef.current.startY - ev.clientY;
      dispatch({ type: 'CONSOLE_SET_HEIGHT', height: dragRef.current.startH + delta });
    };
    const onUp = () => {
      dragRef.current = null;
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
    };
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  }, [dispatch]);

  // Pick the highest-priority active task for status display.
  let activeTask: { op: string; status: string; message: string } | null = null;
  for (const [op, t] of Object.entries(state.tasks)) {
    if (t.status === 'idle') continue;
    if (!activeTask || t.status === 'running' || (t.status === 'error' && activeTask.status !== 'running')) {
      activeTask = { op, ...t };
    }
  }

  // Collapsed bar — delegated to StatusFooter
  if (!consoleOpen) {
    return (
      <StatusFooter
        asOf={state.asOf}
        info={infoCount}
        errors={errorCount}
        task={activeTask ? { op: activeTask.op, message: activeTask.message } : null}
        onExpand={() => dispatch({ type: 'CONSOLE_TOGGLE' })}
        appState={state}
      />
    );
  }

  // Expanded console
  return (
    <div data-testid="console" style={{ flexShrink: 0, display: 'flex', flexDirection: 'column', borderTop: '1px solid #222' }}>
      <div
        onMouseDown={onMouseDown}
        style={{
          height: 4, background: '#1a1a1a', cursor: 'ns-resize',
          borderBottom: '1px solid #222',
        }}
      />
      <div
        style={{
          height: 24, background: '#0d0d0d',
          display: 'flex', alignItems: 'center', padding: '0 12px', gap: 10,
          userSelect: 'none', flexShrink: 0,
        }}
      >
        <span style={{ color: '#888', fontSize: 11, fontWeight: 'bold' }}>Console</span>
        <span style={{ color: '#888', fontSize: 11 }}>{infoCount}</span>
        {errorCount > 0 && <span style={{ color: '#c66', fontSize: 11 }}>{errorCount} err</span>}
        <div style={{ flex: 1 }} />
        <span
          data-testid="console-toggle"
          onClick={() => dispatch({ type: 'CONSOLE_TOGGLE' })}
          style={{ color: '#666', cursor: 'pointer', display: 'flex', alignItems: 'center' }}
        ><ChevronDownIcon color="#666" size={13} /></span>
      </div>
      <div
        ref={listRef}
        style={{
          height: consoleHeight, background: '#111', overflowY: 'auto', overflowX: 'hidden',
          fontFamily: 'monospace', fontSize: 11, lineHeight: '18px', padding: '2px 0',
        }}
      >
        {consoleEntries.length === 0 && (
          <div style={{ color: '#444', padding: '4px 12px' }}>No messages yet.</div>
        )}
        {consoleEntries.map(entry => (
          <div
            key={entry.id}
            style={{
              padding: '0 12px', whiteSpace: 'pre-wrap', wordBreak: 'break-word',
              color: entry.level === 'error' ? '#c66' : '#999',
            }}
          >
            <span style={{ color: '#555' }}>{formatTime(entry.time)}</span>{' '}
            {entry.message}
          </div>
        ))}
      </div>
    </div>
  );
}
