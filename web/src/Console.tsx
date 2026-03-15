import { useEffect, useRef, useCallback } from 'react';
import type { AppState, Action } from './state';

interface Props {
  state: AppState;
  dispatch: React.Dispatch<Action>;
}

function formatTime(ts: number): string {
  const d = new Date(ts);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

export function Console({ state, dispatch }: Props) {
  const { consoleEntries, consoleOpen, consoleHeight } = state;
  const listRef = useRef<HTMLDivElement>(null);
  const dragRef = useRef<{ startY: number; startH: number } | null>(null);

  const infoCount = consoleEntries.filter(e => e.level === 'info').length;
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
    dragRef.current = { startY: e.clientY, startH: consoleHeight };

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
  }, [consoleHeight, dispatch]);

  // Collapsed bar
  if (!consoleOpen) {
    return (
      <div
        onClick={() => dispatch({ type: 'CONSOLE_TOGGLE' })}
        style={{
          height: 24, background: '#0d0d0d', borderTop: '1px solid #222',
          display: 'flex', alignItems: 'center', padding: '0 12px', flexShrink: 0,
          cursor: 'pointer', userSelect: 'none', gap: 10,
        }}
      >
        <span style={{ color: '#666', fontSize: 11 }}>Console</span>
        <span style={{ color: '#888', fontSize: 11 }}>{infoCount}</span>
        {errorCount > 0 && <span style={{ color: '#c66', fontSize: 11 }}>{errorCount} err</span>}
        <div style={{ flex: 1 }} />
        <span style={{ color: '#666', fontSize: 13 }}>&#x25B2;</span>
      </div>
    );
  }

  // Expanded console
  return (
    <div style={{ flexShrink: 0, display: 'flex', flexDirection: 'column', borderTop: '1px solid #222' }}>
      {/* Drag handle */}
      <div
        onMouseDown={onMouseDown}
        style={{
          height: 4, background: '#1a1a1a', cursor: 'ns-resize',
          borderBottom: '1px solid #222',
        }}
      />
      {/* Header */}
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
          onClick={() => dispatch({ type: 'CONSOLE_TOGGLE' })}
          style={{ color: '#666', fontSize: 13, cursor: 'pointer' }}
        >&#x25BC;</span>
      </div>
      {/* Log entries */}
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
