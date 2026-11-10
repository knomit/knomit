import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties, Dispatch, MouseEvent as ReactMouseEvent, ReactNode, RefObject } from 'react';
import { api } from './api';
import { ChatCommitView } from './ChatCommitView';
import type { CommitMemo } from './ChatCommitView';
import { FactView } from './FactView';
import { useFactEdges } from './useFactEdges';
import type { Action, FactReading } from './state';
import { ChatResultPanel } from './ChatResultPanel';
import { FacetSplitter } from './FacetColumns';
import type { ChatCall } from './chatApi';
import type { RepoRef, ResolvedFact } from './chatFactLinks';
import { toolCallOf } from './chatState';
import type { ToolCall } from './chatState';
import { shownEntry, typingIn } from './chatPanelState';
import type { BrowseFactRequest, PanelAction, PanelState } from './chatPanelState';
import { ToolCallHead } from './ChatToolGroup';
import type { JsonViewHandle } from './JsonView';
import { FIND_KEY, FIND_LABEL, PANEL_CLOSE_LABEL } from './chatCopy';

// The mockup's icon (mockup.html, `I`): a 13px stroke.
const svg = (size: number, children: React.ReactNode) => (
  <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{children}</svg>
);
const ICON_CLOSE = svg(13, <><line x1="6" y1="6" x2="18" y2="18" /><line x1="18" y1="6" x2="6" y2="18" /></>);
const ICON_FIND = svg(12, <><circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" /></>);

function isShown(el: HTMLElement | null): boolean {
  for (let n = el; n; n = n.parentElement) if (getComputedStyle(n).display === 'none') return false;
  return true;
}

/**
 * A fact in the panel is Browse's fact view, the one component (FactView). All
 * this does is say what it reads (that fact on that branch, at the commit if there
 * is one) and where its controls go. A control that navigates lands in Browse, and
 * Browse's Back returns to the chat. The panel's only differences are the × in the
 * view's top-right corner and no retract button.
 */
function PanelFact({ repo, branch, path, commit, close, onBrowse, repos, repoNames, serverReadOnly, onScreen, runningRef }: {
  repo: string;
  branch: string;
  path: string;
  commit?: string;
  close: ReactNode;
  onBrowse?: (req: BrowseFactRequest) => void;
  repos?: RepoRef[];
  repoNames?: Record<string, string>;
  serverReadOnly: boolean;
  onScreen: boolean;
  runningRef: RefObject<boolean>;
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
    serverReadOnly, repoReadOnly, branchWritable,
  }), [repo, branch, path, commit, serverReadOnly, repoReadOnly, branchWritable]);
  const edges = useFactEdges(reading);
  const send = (req: Omit<BrowseFactRequest, 'repo' | 'branch' | 'path'>) => {
    onBrowse?.({ repo, branch, path, ...(commit ? { commit } : {}), ...req });
  };
  const hop = (to: string, pinned: string) => onBrowse?.({ repo, branch, path: to, ...(pinned ? { commit: pinned } : {}) });
  // Of what the view dispatches only a filter chip and a motif pivot mean anything
  // outside Browse's state: they go with the reader. The rest is Browse's
  // bookkeeping for its open fact.
  const dispatch = (a: Action) => {
    if (a.type === 'ADD_FILTER') send({ filter: a.chip });
    else if (a.type === 'PIVOT_MOTIF') send({ motif: a.motif });
  };
  return (
    <FactView
      reading={reading}
      dispatch={dispatch}
      // The version opens Browse's history of this fact. Browse's walker opens it at
      // the newest version; a fact read at a commit stays at that commit.
      onScrub={newest => send({ commit: commit ?? newest })}
      onHopRef={hop}
      // Cited by, cites and a motif open their menu here, in the panel. Picking
      // from it is what goes to Browse: an edge hops to that fact (onHopEdge), a
      // motif's "Open all N carriers" pivots there (PIVOT_MOTIF above).
      onHopEdge={hop}
      repoNames={repoNames}
      incoming={edges.incoming}
      outgoing={edges.outgoing}
      edgesError={edges.error}
      refCommits={edges.refCommits}
      cornerAction={close}
      hideRetract
      materializeInUrl={false}
      materializeFocus={{ hidden: !onScreen, yieldsEsc: () => runningRef.current }}
    />
  );
}

/**
 * ChatSidePanel is the 470px panel on the right of a chat: a fact, a commit or
 * a tool result, and a footer. A commit has a 40px header (breadcrumb, close); a
 * tool result's 36px header is the call's own timeline row (ToolCallHead: state
 * dot, label · main argument, size and time) beside the close. A fact has its
 * band instead, with the close in the panel's top-right corner, away from the
 * band's action row (history, materialize). The footer holds Find, which opens
 * the find bar of the JSON view on screen (a result's Result or Arguments). It is
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
export function ChatSidePanel({ chatId, panel, dispatch, running, width, onSplitterDown, onBrowse, repos, repoNames, serverReadOnly = false, onScreen = true, callFor }: {
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
  /** Whether the chat is on screen. It stays mounted behind Browse. */
  onScreen?: boolean;
  /** The transcript's record of a call, which its timeline row is drawn from:
   *  a result's header is that row. */
  callFor?: (callId: string) => ToolCall | undefined;
}) {
  const root = useRef<HTMLDivElement>(null);
  const entry = shownEntry(panel);
  // A loaded call, drawn as its row: the header of a call the transcript no longer holds.
  const [calls, setCalls] = useState<Record<string, ToolCall>>({});
  // The JSON view on screen, whose find bar the footer's Find opens.
  const [finder, setFinder] = useState<JsonViewHandle | null>(null);
  // What each commit view loaded and where it was scrolled to, so its chip in the answer returns to the same place in a long list.
  const [commitMemo] = useState<CommitMemo>(() => new Map());
  const onCall = useCallback((callId: string, call: ChatCall) => {
    setCalls(c => ({ ...c, [callId]: toolCallOf(callId, call) }));
  }, []);
  // A kb:// path in a result opens its fact here, as a fact link in the answer does.
  const onOpenFact = useCallback((f: ResolvedFact) => {
    dispatch({ type: 'open', entry: { kind: 'fact', repo: f.repo, branch: f.branch, path: f.path } });
  }, [dispatch]);

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
  const crumb = entry.kind === 'commit' ? [entry.repo, entry.commit.slice(0, 7)] : [];
  const head = entry.kind === 'result' ? callFor?.(entry.callId) ?? calls[entry.callId] : undefined;

  return (
    <>
      <FacetSplitter onMouseDown={onSplitterDown} testId="chat-panel-splitter" />
      <div ref={root} data-testid="chat-side-panel" className="k-chat-result" style={{ ['--k-panel-w' as string]: `${width}px` }}>
        {entry.kind === 'result' && (
          // The call's own row is the heading: no breadcrumb, and none in the body.
          <div data-testid="chat-panel-header" style={resultHeader}>
            <div style={{ flex: 1, minWidth: 0 }}>
              {head && <ToolCallHead call={head} testId="chat-result-title" style={headInRow} />}
            </div>
            {close}
          </div>
        )}
        {entry.kind === 'commit' && (
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
            ? <ChatResultPanel chatId={chatId} callId={entry.callId} onCall={onCall} onOpenFact={onOpenFact} onFinder={setFinder} />
            : entry.kind === 'commit'
              ? <ChatCommitView key={`${entry.repo}\0${entry.branch}\0${entry.commit}`} repo={entry.repo} branch={entry.branch} commit={entry.commit} memo={commitMemo} onOpenFact={path => dispatch({
                  type: 'open',
                  entry: { kind: 'fact', repo: entry.repo, branch: entry.branch, path, commit: entry.commit },
                })} />
              : <PanelFact key={`${entry.repo}\0${entry.branch}\0${entry.path}\0${entry.commit ?? ''}`} repo={entry.repo} branch={entry.branch} path={entry.path}
                  commit={entry.commit} close={close} onBrowse={onBrowse} repos={repos} repoNames={repoNames} serverReadOnly={serverReadOnly}
                  onScreen={onScreen} runningRef={runningRef} />}
        </div>
        <div data-testid="chat-panel-footer" style={footer}>
          {entry.kind === 'result' && finder && (
            <button type="button" data-testid="chat-panel-find" className="k-panel-find" onClick={() => finder.openFind()}>
              {ICON_FIND}{FIND_LABEL}<kbd style={keycap}>{FIND_KEY}</kbd>
            </button>
          )}
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
// A result's header: no rule under it (the tabs' underline already separates),
// and its dot on the body's 12px edge.
const resultHeader: CSSProperties = { ...header, height: 36, borderBottom: 'none', gap: 10, padding: '0 6px 0 12px' };
const headInRow: CSSProperties = { margin: 0, padding: 0 };
const crumbBox: CSSProperties = {
  flex: 1, minWidth: 0, marginLeft: 4, overflow: 'hidden', whiteSpace: 'nowrap', textOverflow: 'ellipsis',
  fontSize: 11.5, color: '#777',
};
const footer: CSSProperties = {
  flexShrink: 0, display: 'flex', alignItems: 'center', gap: 10, padding: '4px 12px', boxSizing: 'border-box',
  fontSize: 10.5, color: '#555',
};
const keycap: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 9.5, color: '#555', border: '1px solid #2a2a2a', borderRadius: 3, padding: '0 4px',
};
