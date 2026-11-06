import { useCallback, useEffect, useRef, useState } from 'react';
import type { CSSProperties, Dispatch, MouseEvent as ReactMouseEvent } from 'react';
import { ChatFactView } from './ChatFactView';
import { ChatResultPanel } from './ChatResultPanel';
import { FacetSplitter } from './FacetColumns';
import type { ChatCall } from './chatApi';
import { toolRowLabel } from './chatState';
import { crumbOf, shownEntry } from './chatPanelState';
import type { PanelAction, PanelState } from './chatPanelState';
import {
  PANEL_BACK_LABEL, PANEL_CLOSE_LABEL, PANEL_FORWARD_LABEL, PANEL_OPEN_IN_BROWSE, panelFooter,
} from './chatCopy';

/** A string argument of a tool call, or ''. */
const arg = (call: ChatCall, ...names: string[]): string => {
  const a = (call.arguments ?? {}) as Record<string, unknown>;
  for (const n of names) if (typeof a[n] === 'string' && a[n]) return a[n] as string;
  return '';
};

function isShown(el: HTMLElement | null): boolean {
  for (let n = el; n; n = n.parentElement) if (getComputedStyle(n).display === 'none') return false;
  return true;
}

/** Whether the key went to something the person types into. */
function typingIn(el: Element | null): boolean {
  if (!el) return false;
  const tag = el.tagName;
  return tag === 'TEXTAREA' || tag === 'INPUT' || tag === 'SELECT' || (el as HTMLElement).isContentEditable === true;
}

/**
 * ChatSidePanel is the 470px panel on the right of a chat: a fact or a tool
 * result, a 40px header (back, forward, breadcrumb, Open in Browse, close) and
 * a footer. It is mounted only while open, so it costs nothing before a step is
 * clicked; the chat column beside it narrows to make room.
 *
 * KEYS (precedence).
 *   Esc while a turn is RUNNING stops the turn (ChatPanel) and never closes the
 *   panel: Stop is the one that must not be missed, so the panel yields to it
 *   wherever the focus is. Esc with no turn running closes the panel, unless
 *   the focus is in the composer (or any field), where Esc is not ours. A key
 *   something else already took (defaultPrevented) is left alone, as is a chat
 *   that is not on screen.
 *   Cmd+[ (Ctrl+[ too) goes back, from anywhere, including the composer; the
 *   browser's own history shortcut is suppressed.
 */
export function ChatSidePanel({ chatId, panel, dispatch, running, width, onSplitterDown, onOpenInBrowse, boundFor }: {
  chatId: string;
  panel: PanelState;
  dispatch: Dispatch<PanelAction>;
  /** A turn is running: Esc belongs to Stop. */
  running: boolean;
  width: number;
  onSplitterDown: (e: ReactMouseEvent) => void;
  /** Open this fact in Browse. Absent where the page cannot navigate. */
  onOpenInBrowse?: (repo: string, path: string) => void;
  /** The repo or lens the chat was bound to when a call ran (a result with no
   *  target of its own belongs to it). */
  boundFor?: (callId: string) => string;
}) {
  const root = useRef<HTMLDivElement>(null);
  const entry = shownEntry(panel);
  // What a loaded result says about itself: its breadcrumb and where Browse would open it.
  const [calls, setCalls] = useState<Record<string, { crumb: string[]; browse?: { repo: string; path: string } }>>({});
  const boundForRef = useRef(boundFor);
  useEffect(() => { boundForRef.current = boundFor; });
  const onCall = useCallback((callId: string, call: ChatCall) => {
    // A materialize names its fact as `file` (a path, or kb://<id>/<path>); other
    // tools use `path`. The target is what the call names, else the binding in
    // force when it ran (the handle in `binding` is opaque, so it is not read).
    const file = arg(call, 'file', 'path');
    const path = file.replace(/^kb:\/\/[^/]+\//, '');
    const repo = arg(call, 'repo', 'lens', 'target') || boundForRef.current?.(callId) || '';
    const crumb = repo || path ? crumbOf(repo || toolRowLabel(call.name), path) : [toolRowLabel(call.name)];
    const browse = call.name === 'knomit_materialize' && !call.is_error && repo && path ? { repo, path: file } : undefined;
    setCalls(c => ({ ...c, [callId]: { crumb, ...(browse ? { browse } : {}) } }));
  }, []);

  const runningRef = useRef(running);
  useEffect(() => { runningRef.current = running; });
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = root.current;
      if (!el || !isShown(el)) return;
      if (e.key === 'Escape') {
        if (e.defaultPrevented || runningRef.current) return;
        if (typingIn(document.activeElement)) return;
        dispatch({ type: 'close' });
      } else if (e.key === '[' && (e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey) {
        e.preventDefault();
        dispatch({ type: 'back' });
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [dispatch]);

  if (!entry) return null;
  const known = entry.kind === 'result' ? calls[entry.callId] : undefined;
  const crumb = entry.kind === 'fact' ? crumbOf(entry.repo, entry.path) : known?.crumb ?? [];
  const browse = entry.kind === 'fact' ? { repo: entry.repo, path: entry.path } : known?.browse;
  const canBack = panel.index > 0;
  const canForward = panel.index < panel.entries.length - 1;

  return (
    <>
      <FacetSplitter onMouseDown={onSplitterDown} testId="chat-panel-splitter" />
      <div ref={root} data-testid="chat-side-panel" className="k-chat-result" style={{ ['--k-panel-w' as string]: `${width}px` }}>
        <div data-testid="chat-panel-header" style={header}>
          <button type="button" data-testid="chat-panel-back" aria-label={PANEL_BACK_LABEL} title={PANEL_BACK_LABEL}
            disabled={!canBack} onClick={() => dispatch({ type: 'back' })} style={iconBtn(canBack)}>‹</button>
          <button type="button" data-testid="chat-panel-forward" aria-label={PANEL_FORWARD_LABEL} title={PANEL_FORWARD_LABEL}
            disabled={!canForward} onClick={() => dispatch({ type: 'forward' })} style={iconBtn(canForward)}>›</button>
          <div data-testid="chat-panel-crumb" style={crumbBox}>
            {crumb.map((c, i) => (
              <span key={i} style={{ whiteSpace: 'nowrap' }}>{i > 0 && <span style={{ color: '#444' }}> / </span>}{c}</span>
            ))}
          </div>
          {browse && onOpenInBrowse && (
            <button type="button" data-testid="chat-panel-browse" onClick={() => onOpenInBrowse(browse.repo, browse.path)} style={browseBtn}>
              {PANEL_OPEN_IN_BROWSE}
            </button>
          )}
          <button type="button" data-testid="chat-panel-close" aria-label={PANEL_CLOSE_LABEL} title={PANEL_CLOSE_LABEL}
            onClick={() => dispatch({ type: 'close' })} style={iconBtn(true)}>×</button>
        </div>
        <div style={{ flex: 1, minHeight: 0, overflow: 'hidden' }}>
          {entry.kind === 'result'
            ? <ChatResultPanel chatId={chatId} callId={entry.callId} onCall={onCall} />
            : <ChatFactView repo={entry.repo} branch={entry.branch} path={entry.path} commit={entry.commit} />}
        </div>
        <div data-testid="chat-panel-footer" style={footer}>{panelFooter(panel.index + 1, panel.entries.length)}</div>
      </div>
    </>
  );
}

const header: CSSProperties = {
  height: 40, flexShrink: 0, display: 'flex', alignItems: 'center', gap: 6, padding: '0 10px',
  borderBottom: '1px solid #222', boxSizing: 'border-box',
};
const iconBtn = (on: boolean): CSSProperties => ({
  background: 'none', border: 'none', padding: '0 6px', fontSize: 18, lineHeight: '24px', borderRadius: 4,
  color: on ? '#bbb' : '#444', cursor: on ? 'pointer' : 'default', flexShrink: 0,
});
const crumbBox: CSSProperties = {
  flex: 1, minWidth: 0, display: 'flex', gap: 0, overflow: 'hidden', fontSize: 11.5, color: '#8a8a8a',
  fontFamily: 'var(--k-font-mono)',
};
const browseBtn: CSSProperties = {
  flexShrink: 0, background: 'none', border: '1px solid #2e2e2e', borderRadius: 6, padding: '3px 9px',
  fontSize: 11.5, color: '#b8c4d6', cursor: 'pointer',
};
const footer: CSSProperties = {
  minHeight: 28, flexShrink: 0, display: 'flex', alignItems: 'center', padding: '4px 14px', boxSizing: 'border-box',
  borderTop: '1px solid #222', fontSize: 10.5, color: '#666', fontFamily: 'var(--k-font-mono)',
};
