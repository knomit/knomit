import { useCallback, useEffect, useRef, useState } from 'react';
import type { CSSProperties, Dispatch, MouseEvent as ReactMouseEvent } from 'react';
import { ChatFactView } from './ChatFactView';
import { ChatResultPanel } from './ChatResultPanel';
import { FacetSplitter } from './FacetColumns';
import type { ChatCall } from './chatApi';
import { bindTarget, toolCallLabel, toolCallOf, toolKind } from './chatState';
import type { ToolCall } from './chatState';
import { crumbOf, shownEntry } from './chatPanelState';
import type { PanelAction, PanelState } from './chatPanelState';
import {
  PANEL_BACK_LABEL, PANEL_CLOSE_LABEL, PANEL_FORWARD_LABEL, PANEL_KEY_BACK, PANEL_KEY_CLOSE, PANEL_OPEN_IN_BROWSE, panelOpened,
} from './chatCopy';

// The mockup's icons (mockup.html, `I`): 13px strokes for the header, 11px for the external-link mark.
const svg = (size: number, children: React.ReactNode) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{children}</svg>
);
const ICON_BACK = svg(13, <polyline points="15 18 9 12 15 6" />);
const ICON_FORWARD = svg(13, <polyline points="9 18 15 12 9 6" />);
const ICON_CLOSE = svg(13, <><line x1="6" y1="6" x2="18" y2="18" /><line x1="18" y1="6" x2="6" y2="18" /></>);
const ICON_EXTERNAL = svg(11, <><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" /><polyline points="15 3 21 3 21 9" /><line x1="10" y1="14" x2="21" y2="3" /></>);

/** The footer's "Esc close" / "⌘[ back": the key drawn as a keycap, then its word. */
const keyHint = (hint: string) => {
  const i = hint.indexOf(' ');
  return <><span style={keycap}>{hint.slice(0, i)}</span><span>{hint.slice(i)}<span style={dot}> · </span></span></>;
};

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
export function ChatSidePanel({ chatId, panel, dispatch, running, width, onSplitterDown, onOpenInBrowse, boundFor, callFor }: {
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
  /** The transcript's record of a call, which its timeline row is drawn from:
   *  the panel's header is that row, and the breadcrumb names it the same way. */
  callFor?: (callId: string) => ToolCall | undefined;
}) {
  const root = useRef<HTMLDivElement>(null);
  const entry = shownEntry(panel);
  // What a loaded result says about itself: its breadcrumb and where Browse would open it.
  const [calls, setCalls] = useState<Record<string, { crumb: string[]; browse?: { repo: string; path: string } }>>({});
  const boundForRef = useRef(boundFor);
  const callForRef = useRef(callFor);
  useEffect(() => { boundForRef.current = boundFor; callForRef.current = callFor; });
  const onCall = useCallback((callId: string, call: ChatCall) => {
    // A materialize names its fact as `file` (a path, or kb://<id>/<path>); other
    // tools use `path`. The target is what the call names, else the binding in
    // force when it ran (the handle in `binding` is opaque, so it is not read).
    const file = arg(call, 'file', 'path');
    const path = file.replace(/^kb:\/\/[^/]+\//, '');
    // A bind names its own target (bindTarget's rule: lens, repo, name, target), and it is that, not the binding in
    // force BEFORE it ran, that the call belongs to. Other calls name a repo or lens, else the binding in force.
    const own = toolKind(call.name) === 'bind' ? bindTarget(toolCallOf(callId, call)) : arg(call, 'repo', 'lens', 'target');
    const repo = own || boundForRef.current?.(callId) || '';
    // The mockup's "repo / path / dir" when the call names a file; otherwise the repo it ran in, then the call,
    // labelled as its row is (toolCallLabel).
    const label = toolCallLabel(callForRef.current?.(callId) ?? toolCallOf(callId, call));
    const crumb = path ? crumbOf(repo || label, path) : [repo, label].filter(Boolean);
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
            disabled={!canBack} onClick={() => dispatch({ type: 'back' })} style={iconBtn(canBack)}>{ICON_BACK}</button>
          <button type="button" data-testid="chat-panel-forward" aria-label={PANEL_FORWARD_LABEL} title={PANEL_FORWARD_LABEL}
            disabled={!canForward} onClick={() => dispatch({ type: 'forward' })} style={iconBtn(canForward)}>{ICON_FORWARD}</button>
          <div data-testid="chat-panel-crumb" style={crumbBox}>
            {crumb.map((c, i) => (
              <span key={i} style={{ whiteSpace: 'nowrap' }}>{i > 0 && ' / '}{c}</span>
            ))}
          </div>
          {browse && onOpenInBrowse && (
            <button type="button" data-testid="chat-panel-browse" onClick={() => onOpenInBrowse(browse.repo, browse.path)} style={browseBtn}>
              {PANEL_OPEN_IN_BROWSE}{ICON_EXTERNAL}
            </button>
          )}
          <button type="button" data-testid="chat-panel-close" aria-label={PANEL_CLOSE_LABEL} title={PANEL_CLOSE_LABEL}
            onClick={() => dispatch({ type: 'close' })} style={iconBtn(true)}>{ICON_CLOSE}</button>
        </div>
        <div style={{ flex: 1, minHeight: 0, overflow: 'hidden' }}>
          {entry.kind === 'result'
            ? <ChatResultPanel chatId={chatId} callId={entry.callId} row={callFor?.(entry.callId)} onCall={onCall} />
            : <ChatFactView repo={entry.repo} branch={entry.branch} path={entry.path} commit={entry.commit} />}
        </div>
        <div data-testid="chat-panel-footer" style={footer}>
          {keyHint(PANEL_KEY_CLOSE)}{keyHint(PANEL_KEY_BACK)}
          <span style={{ marginLeft: 'auto' }}>{panelOpened(panel.index + 1, panel.entries.length)}</span>
        </div>
      </div>
    </>
  );
}

// Sizes and colours are the mockup's .sbar / .ib / .bc / .obtn / .kbdh (mockup.html, side panel).
const header: CSSProperties = {
  height: 40, flexShrink: 0, display: 'flex', alignItems: 'center', gap: 4, padding: '0 8px',
  borderBottom: '1px solid #1e1e1e', boxSizing: 'border-box',
};
const iconBtn = (on: boolean): CSSProperties => ({
  background: 'none', border: 'none', padding: 0, width: 26, height: 26, borderRadius: 5, flexShrink: 0,
  display: 'flex', alignItems: 'center', justifyContent: 'center',
  color: on ? '#888' : '#3a3a3a', cursor: on ? 'pointer' : 'default',
});
const crumbBox: CSSProperties = {
  flex: 1, minWidth: 0, marginLeft: 4, overflow: 'hidden', whiteSpace: 'nowrap', textOverflow: 'ellipsis',
  fontSize: 11.5, color: '#777',
};
const browseBtn: CSSProperties = {
  flexShrink: 0, background: 'none', border: '1px solid #333', borderRadius: 5, padding: '3px 9px',
  fontSize: 11.5, color: '#ccc', cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 5, whiteSpace: 'nowrap',
};
const footer: CSSProperties = {
  flexShrink: 0, display: 'flex', alignItems: 'center', gap: 10, padding: '8px 14px', boxSizing: 'border-box',
  borderTop: '1px solid #1e1e1e', fontSize: 10.5, color: '#555',
};
const keycap: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 9.5, color: '#555', border: '1px solid #2a2a2a', borderRadius: 3, padding: '0 4px',
};
// The " · " separators stay in the text (it reads the same to a screen reader) but are zero-width and INSIDE the word's own
// flex item, so they add no flex gap: the footer's items are exactly the mockup's (keycap, word, keycap, word, count).
const dot: CSSProperties = { display: 'inline-block', width: 0, overflow: 'hidden', whiteSpace: 'pre' };
