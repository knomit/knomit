import { useEffect, useRef, useCallback } from 'react';
import type { AppState, Action, AsOf } from './state';
import { selectTrail } from './state';
import { ChevronUpIcon, ChevronDownIcon, BroadcastIcon, HistoryIcon, CompareIcon } from './icons';
import type { ComponentType } from 'react';

interface Props {
  state: AppState;
  dispatch: React.Dispatch<Action>;
  /** Running server build version (full string), or null until it resolves. */
  version?: string | null;
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
  version?: string | null;
}

type PillIcon = ComponentType<{ color: string; size?: number }>;

// The mode label is now a theme-colored glyph (broadcast = live HEAD, clock =
// history, git-compare = diff); `label` survives as the icon's accessible name.
function pillContent(asOf: AsOf): { color: string; label: string; Icon: PillIcon; descriptor: string; glow: boolean } {
  switch (asOf.mode) {
    case 'live':
      return { color: '#7c9', label: 'LIVE', Icon: BroadcastIcon, descriptor: 'HEAD', glow: true };
    case 'history':
      return { color: '#e5a23c', label: 'HISTORY', Icon: HistoryIcon, descriptor: asOf.commit.slice(0, 7), glow: false };
    case 'diff':
      return { color: '#e5a23c', label: 'DIFF', Icon: CompareIcon, descriptor: `${asOf.from.slice(0, 7)}..${asOf.to.slice(0, 7)}`, glow: false };
  }
}

// Muted build-version tag. Lives inline in the console chrome (collapsed bar
// and expanded header) rather than as a floating overlay, so it can never
// collide with the bar's controls.
function VersionTag({ version }: { version?: string | null }) {
  if (!version) return null;
  return (
    <span
      data-testid="version-badge"
      title="knomit build version"
      style={{
        color: '#5a5a65', fontFamily: 'var(--k-font-mono)', fontSize: 10,
        whiteSpace: 'nowrap', userSelect: 'none',
      }}
    >
      v{version}
    </span>
  );
}

function Kbd({ children }: { children: string }) {
  return (
    <span style={{
      color: '#a0a0a8', background: '#16161b', padding: '0 4px',
      borderRadius: 2, border: '1px solid #1f1f26',
      fontFamily: 'var(--k-font-mono)', fontSize: 10,
    }}>{children}</span>
  );
}

function StatusFooter({ asOf, info, errors, task, onExpand, appState, version }: StatusFooterProps) {
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
        fontFamily: 'var(--k-font-body)', fontSize: 11,
      }}
    >
      <span style={{
        flex: '0 0 auto', display: 'flex', alignItems: 'center', gap: 8,
      }}>
        <span style={{
          width: 6, height: 6, borderRadius: '50%', background: p.color,
          boxShadow: p.glow ? `0 0 6px ${p.color}` : 'none',
        }}/>
        <span
          data-testid="console-mode"
          role="img"
          aria-label={p.label}
          title={p.label}
          style={{ display: 'inline-flex', alignItems: 'center' }}
        ><p.Icon color={p.color} size={12} /></span>
        <span style={{ color: '#a0a0a8', fontFamily: 'var(--k-font-mono)', fontSize: 10 }}>
          {p.descriptor}
        </span>
        {asOf.mode === 'history' && (
          <span style={{ color: '#a0a0a8', fontFamily: 'var(--k-font-mono)', fontSize: 10 }}>
            · read-only
          </span>
        )}
        {asOf.mode === 'history' && trailHops >= 1 && (
          <span style={{ color: '#e5a23c', fontFamily: 'var(--k-font-mono)', fontSize: 10 }}>
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
            fontFamily: 'var(--k-font-mono)',
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            minWidth: 0,
          }}>[{task.op}] {task.message}</span>
        )}
      </span>

      <VersionTag version={version} />

      <span style={{
        flex: '0 0 auto', display: 'flex', alignItems: 'center', gap: 8,
        fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#5a5a65',
      }}>
        <Kbd>h</Kbd> now
      </span>

      <ChevronUpIcon color="#5a5a65" size={13} />
    </div>
  );
}

export function Console({ state, dispatch, version }: Props) {
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
        version={version}
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
        <VersionTag version={version} />
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
          fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: '18px', padding: '2px 0',
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
