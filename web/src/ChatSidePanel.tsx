import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties, Dispatch, MouseEvent as ReactMouseEvent, ReactNode } from 'react';
import { api } from './api';
import { ChatCommitView } from './ChatCommitView';
import type { CommitMemo } from './ChatCommitView';
import { FactView } from './FactView';
import { useFactEdges } from './useFactEdges';
import type { Action, FactReading } from './state';
import { ChatResultPanel } from './ChatResultPanel';
import { FacetSplitter } from './FacetColumns';
import type { ChatCall } from './chatApi';
import type { RepoRef } from './chatFactLinks';
import { bindTarget, isBind, toolCallLabel, toolCallOf } from './chatState';
import type { ToolCall } from './chatState';
import { crumbOf, shownEntry } from './chatPanelState';
import type { BrowseFactRequest, PanelAction, PanelState } from './chatPanelState';
import { PANEL_CLOSE_LABEL, PANEL_KEY_CLOSE } from './chatCopy';

// The mockup's icon (mockup.html, `I`): a 13px stroke.
const svg = (size: number, children: React.ReactNode) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{children}</svg>
);
const ICON_CLOSE = svg(13, <><line x1="6" y1="6" x2="18" y2="18" /><line x1="18" y1="6" x2="6" y2="18" /></>);

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
 * A fact in the panel is Browse's fact view, the one component (FactView). All
 * this does is say what it reads (that fact on that branch, at the commit if there
 * is one) and where its controls go. A control that navigates lands in Browse, and
 * Browse's Back returns to the chat. The panel's only differences are the × in the
 * band and no retract button.
 */
function PanelFact({ repo, branch, path, commit, close, onBrowse, repos, repoNames, serverReadOnly }: {
  repo: string;
  branch: string;
  path: string;
  commit?: string;
  close: ReactNode;
  onBrowse?: (req: BrowseFactRequest) => void;
  repos?: RepoRef[];
  repoNames?: Record<string, string>;
  serverReadOnly: boolean;
}) {
  const [branchWritable, setBranchWritable] = useState(false);
  useEffect(() => {
    let cancelled = false;
    api.status(repo, branch).then(s => { if (!cancelled) setBranchWritable(!!s.writable); }).catch(() => { /* stays read-only */ });
    return () => { cancelled = true; };
  }, [repo, branch]);
  const repoReadOnly = repos?.find(r => r.name === repo)?.mode === 'subscribe';
  // headCommit stays empty: a live read with no head asks for the branch's tip.
  const reading = useMemo<FactReading>(() => ({
    context: { kind: 'repo', repo }, lens: null, lensWriteId: null, repo, branch, factPath: path, factSource: null,
    asOf: commit ? { mode: 'history', commit } : { mode: 'live' }, headCommit: '',
    serverReadOnly, repoReadOnly, branchWritable, factPanelRequest: null,
  }), [repo, branch, path, commit, serverReadOnly, repoReadOnly, branchWritable]);
  const edges = useFactEdges(reading);
  const send = (req: Omit<BrowseFactRequest, 'repo' | 'branch' | 'path'>) => {
    onBrowse?.({ repo, branch, path, ...(commit ? { commit } : {}), ...req });
  };
  const hop = (to: string, pinned: string) => onBrowse?.({ repo, branch, path: to, ...(pinned ? { commit: pinned } : {}) });
  // Of what the view dispatches only a filter chip means anything outside Browse's
  // state: it goes with the reader. The rest is Browse's bookkeeping for its open fact.
  const dispatch = (a: Action) => { if (a.type === 'ADD_FILTER') send({ filter: a.chip }); };
  return (
    <FactView
      reading={reading}
      dispatch={dispatch}
      // The version opens Browse's history of this fact. Browse's walker opens it at
      // the newest version; a fact read at a commit stays at that commit.
      onScrub={newest => send({ commit: commit ?? newest })}
      onHopRef={hop}
      onHopEdge={hop}
      onOpenPanel={panel => send({ panel })}
      repoNames={repoNames}
      incoming={edges.incoming}
      outgoing={edges.outgoing}
      edgesError={edges.error}
      refCommits={edges.refCommits}
      bandAction={close}
      hideRetract
      materializeInUrl={false}
    />
  );
}

/**
 * ChatSidePanel is the 470px panel on the right of a chat: a fact, a commit or
 * a tool result, and a footer. A commit or a tool result has a 40px header
 * (breadcrumb, close). A fact has its band instead, which holds the close. It is
 * mounted only while open, so it costs nothing before a step is clicked; the chat
 * column beside it narrows to make room.
 *
 * There is no history here: the reader picks from the chat. From a fact opened out
 * of a commit, the way back to the commit is its chip in the answer.
 *
 * ESC while a turn is RUNNING stops the turn (ChatPanel) and never closes the
 * panel: Stop is the one that must not be missed, so the panel yields to it
 * wherever the focus is. Esc with no turn running closes the panel, unless the
 * focus is in the composer (or any field), where Esc is not ours. A key something
 * else already took (defaultPrevented) is left alone, as is a chat that is not on
 * screen.
 */
export function ChatSidePanel({ chatId, panel, dispatch, running, width, onSplitterDown, onBrowse, repos, repoNames, serverReadOnly = false, boundFor, callFor }: {
  chatId: string;
  panel: PanelState;
  dispatch: Dispatch<PanelAction>;
  /** A turn is running: Esc belongs to Stop. */
  running: boolean;
  width: number;
  onSplitterDown: (e: ReactMouseEvent) => void;
  /** Send the reader to Browse on a fact. Absent where the page cannot navigate. */
  onBrowse?: (req: BrowseFactRequest) => void;
  /** What the fact view reads from the page, as Browse does. */
  repos?: RepoRef[];
  repoNames?: Record<string, string>;
  serverReadOnly?: boolean;
  /** The repo or lens the chat was bound to when a call ran (a result with no
   *  target of its own belongs to it). */
  boundFor?: (callId: string) => string;
  /** The transcript's record of a call, which its timeline row is drawn from:
   *  the panel's header is that row, and the breadcrumb names it the same way. */
  callFor?: (callId: string) => ToolCall | undefined;
}) {
  const root = useRef<HTMLDivElement>(null);
  const entry = shownEntry(panel);
  // What a loaded result says about itself: its breadcrumb.
  const [calls, setCalls] = useState<Record<string, { crumb: string[] }>>({});
  const boundForRef = useRef(boundFor);
  const callForRef = useRef(callFor);
  useEffect(() => { boundForRef.current = boundFor; callForRef.current = callFor; });
  // What each commit view loaded and where it was scrolled to, so its chip in the answer returns to the same place in a long list.
  const [commitMemo] = useState<CommitMemo>(() => new Map());
  const onCall = useCallback((callId: string, call: ChatCall) => {
    // A materialize names its fact as `file` (a path, or kb://<id>/<path>); other
    // tools use `path`. The target is what the call names, else the binding in
    // force when it ran (the handle in `binding` is opaque, so it is not read).
    const file = arg(call, 'file', 'path');
    const path = file.replace(/^kb:\/\/[^/]+\//, '');
    // A bind names its own target (bindTarget's rule: lens, repo, name, target), and it is that, not the binding in
    // force BEFORE it ran, that the call belongs to. Other calls name a repo or lens, else the binding in force.
    const own = isBind(call.name) ? bindTarget(toolCallOf(callId, call)) : arg(call, 'repo', 'lens', 'target');
    const repo = own || boundForRef.current?.(callId) || '';
    // The mockup's "repo / path / dir" when the call names a file; otherwise the repo it ran in, then the call,
    // labelled as its row is (toolCallLabel).
    const label = toolCallLabel(callForRef.current?.(callId) ?? toolCallOf(callId, call));
    const crumb = path ? crumbOf(repo || label, path) : [repo, label].filter(Boolean);
    setCalls(c => ({ ...c, [callId]: { crumb } }));
  }, []);

  const runningRef = useRef(running);
  useEffect(() => { runningRef.current = running; });
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = root.current;
      if (!el || !isShown(el)) return;
      if (e.key !== 'Escape' || e.defaultPrevented || runningRef.current) return;
      if (typingIn(document.activeElement)) return;
      dispatch({ type: 'close' });
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [dispatch]);

  if (!entry) return null;
  const close = (
    <button type="button" data-testid="chat-panel-close" aria-label={PANEL_CLOSE_LABEL} title={PANEL_CLOSE_LABEL}
      onClick={() => dispatch({ type: 'close' })} style={iconBtn}>{ICON_CLOSE}</button>
  );
  const crumb = entry.kind === 'commit' ? [entry.repo, entry.commit.slice(0, 7)] : entry.kind === 'result' ? calls[entry.callId]?.crumb ?? [] : [];

  return (
    <>
      <FacetSplitter onMouseDown={onSplitterDown} testId="chat-panel-splitter" />
      <div ref={root} data-testid="chat-side-panel" className="k-chat-result" style={{ ['--k-panel-w' as string]: `${width}px` }}>
        {entry.kind !== 'fact' && (
          <div data-testid="chat-panel-header" style={header}>
            <div data-testid="chat-panel-crumb" style={crumbBox}>
              {crumb.map((c, i) => (
                <span key={i} style={{ whiteSpace: 'nowrap' }}>{i > 0 && ' / '}{c}</span>
              ))}
            </div>
            {close}
          </div>
        )}
        <div style={{ flex: 1, minHeight: 0, overflow: 'hidden' }}>
          {entry.kind === 'result'
            ? <ChatResultPanel chatId={chatId} callId={entry.callId} row={callFor?.(entry.callId)} onCall={onCall} />
            : entry.kind === 'commit'
              ? <ChatCommitView key={`${entry.repo}\0${entry.branch}\0${entry.commit}`} repo={entry.repo} branch={entry.branch} commit={entry.commit} memo={commitMemo} onOpenFact={path => dispatch({
                  type: 'open',
                  entry: { kind: 'fact', repo: entry.repo, branch: entry.branch, path, commit: entry.commit },
                })} />
              : <PanelFact key={`${entry.repo}\0${entry.branch}\0${entry.path}\0${entry.commit ?? ''}`} repo={entry.repo} branch={entry.branch} path={entry.path}
                  commit={entry.commit} close={close} onBrowse={onBrowse} repos={repos} repoNames={repoNames} serverReadOnly={serverReadOnly} />}
        </div>
        <div data-testid="chat-panel-footer" style={footer}>
          <span style={keycap}>{PANEL_KEY_CLOSE.slice(0, PANEL_KEY_CLOSE.indexOf(' '))}</span>
          <span>{PANEL_KEY_CLOSE.slice(PANEL_KEY_CLOSE.indexOf(' '))}</span>
        </div>
      </div>
    </>
  );
}

// Sizes and colours are the mockup's .sbar / .ib / .bc / .kbdh (mockup.html, side panel).
const header: CSSProperties = {
  height: 40, flexShrink: 0, display: 'flex', alignItems: 'center', gap: 4, padding: '0 8px',
  borderBottom: '1px solid #1e1e1e', boxSizing: 'border-box',
};
const iconBtn: CSSProperties = {
  background: 'none', border: 'none', padding: 0, width: 26, height: 26, borderRadius: 5, flexShrink: 0,
  display: 'flex', alignItems: 'center', justifyContent: 'center',
  color: '#888', cursor: 'pointer',
};
const crumbBox: CSSProperties = {
  flex: 1, minWidth: 0, marginLeft: 4, overflow: 'hidden', whiteSpace: 'nowrap', textOverflow: 'ellipsis',
  fontSize: 11.5, color: '#777',
};
const footer: CSSProperties = {
  flexShrink: 0, display: 'flex', alignItems: 'center', gap: 10, padding: '8px 14px', boxSizing: 'border-box',
  borderTop: '1px solid #1e1e1e', fontSize: 10.5, color: '#555',
};
const keycap: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 9.5, color: '#555', border: '1px solid #2a2a2a', borderRadius: 3, padding: '0 4px',
};
