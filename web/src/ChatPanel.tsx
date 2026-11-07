import { useEffect, useRef, useState } from 'react';
import type { CSSProperties, KeyboardEvent } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { ChatToolGroup } from './ChatToolGroup';
import { ChatTurnFooter } from './ChatTurnFooter';
import { ContextMeter } from './ChatPill';
import { TurnNotices, NoticeLine } from './ChatNotice';
import { SendingLine, ThinkingPlaceholder, StoppedMark } from './ChatWaiting';
import { chatPlugins, chatLivePlugins, chatComponents } from './chatTableBars';
import { FactLinkContext, useTurnFacts, remarkFactLinks, turnPrefer } from './chatFactLinks';
import type { RepoRef, ResolvedFact } from './chatFactLinks';
import { boundAfter, compactCount, sessionCost } from './chatState';
import type { ChatState, ChatTurn, ToolCall } from './chatState';
import type { ApprovalDecision } from './chatApi';
import {
  CHAT_CONTINUE_MESSAGE, CHAT_INPUT_PLACEHOLDER, CHAT_REPLY_PLACEHOLDER, CHAT_SEND, CHAT_STOP, CHAT_STOP_KEY, COMPOSER_HINT, CONTEXT_LABEL,
  NOT_SENT, NOT_SENT_RETRY,
} from './chatCopy';

/** How a turn is drawn: its work (runs of tool calls, each with the text that
 *  led into it) and the final text below. Text before a group is interstitial
 *  and goes inside the next group; text after the last group is the answer. */
type Block = { kind: 'group'; lead: string[]; calls: ToolCall[]; key: string } | { kind: 'text'; text: string; key: string };

function blocksOf(turn: ChatTurn): Block[] {
  const out: Block[] = [];
  let lead: string[] = [];
  turn.parts.forEach((p, i) => {
    if (p.kind === 'text') { lead.push(p.text); return; }
    out.push({ kind: 'group', lead, calls: p.calls, key: `g${i}` });
    lead = [];
  });
  lead.forEach((t, i) => out.push({ kind: 'text', text: t, key: `t${i}` }));
  return out;
}

/** A turn's answer: its text blocks, in order. */
const answerOf = (blocks: Block[]): string => blocks.filter(b => b.kind === 'text').map(b => (b as { text: string }).text).join('\n\n');

const NO_REPOS: RepoRef[] = [];

/** The size of the chat's context: tokens used by the latest prompt, and the
 *  model's window (0 until it is known). */
export interface ChatContextUse { used: number; window: number }

/** The column the transcript and the composer share. */
const COLUMN: CSSProperties = { width: 740, maxWidth: 'calc(100% - 48px)', margin: '0 auto' };
/** The transcript hangs from the composer: a short one sits at the bottom of its box (the auto top margin
 *  collapses to nothing once it is taller than the box, so it scrolls as before). */
const TRANSCRIPT_COLUMN: CSSProperties = { ...COLUMN, margin: 'auto auto 0' };

/**
 * ChatPanel is the conversation, in a centered column: each turn's message (a
 * faint strip), its activity line, its final text as markdown, and a cost
 * footer; the server's price-tier warning; and the rounded box to write in.
 */
export function ChatPanel({ state, selectedId, actionError, notSent, landed, modelName, context, onSend, onStop, onSelect, onAnswer, onNewChat, factLinks }: {
  state: ChatState;
  selectedId: string | null;
  /** The last failed cancel or answer. */
  actionError: string;
  /** The last refused send: its text goes back in the composer, and the reason
   *  shows under the box with Retry. */
  notSent: { text: string; reason: string } | null;
  /** A send the person stopped that the server accepted anyway: its text, which was put back in the composer. */
  landed?: { text: string } | null;
  /** The chat model's display name, for the notices. */
  modelName: string;
  context: ChatContextUse;
  onSend: (text: string) => void;
  onStop: () => void;
  onSelect: (call: ToolCall) => void;
  onAnswer: (callId: string, decision: ApprovalDecision) => void;
  /** Start an empty chat with the same model (the notices' action). */
  onNewChat?: () => void;
  /** The answers' fact links: the repos a repo chip may name, the fact open in
   *  the side panel, and what a link does. */
  factLinks?: { repos: RepoRef[]; openKey: string | null; onOpen?: (f: ResolvedFact) => void; onBrowse?: (repo: string, path: string) => void };
}) {
  const [draft, setDraft] = useState('');
  // The finished turns' cited facts: one call for all not yet answered, each
  // looked for first in the repos its turn worked in.
  const turnFacts = useTurnFacts(
    state.turns.map((t, ti) => (state.running && ti === state.turns.length - 1 ? null : answerOf(blocksOf(t)))),
    state.turns.map((_, ti) => turnPrefer(state.turns, ti)),
  );
  const repos = factLinks?.repos ?? NO_REPOS;
  const scroller = useRef<HTMLDivElement>(null);
  const root = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLTextAreaElement>(null);
  const lastTurn = state.turns[state.turns.length - 1];
  const sending = !!lastTurn?.local && !lastTurn.local.accepted;

  // A refused send keeps its text: it goes back into the composer (in front of
  // anything typed since), with no change to the box's own look.
  const restored = useRef<unknown>(null);
  useEffect(() => {
    if (!notSent || restored.current === notSent) return;
    restored.current = notSent;
    setDraft(d => (d.trim() ? `${notSent.text}\n${d}` : notSent.text));
  }, [notSent]);

  // The turn the person stopped turned out to be in the transcript: its text
  // must not also wait in the composer (resending would duplicate it). Only the
  // restored text is taken out; anything typed since stays.
  const settled = useRef<unknown>(null);
  useEffect(() => {
    if (!landed || settled.current === landed) return;
    settled.current = landed;
    setDraft(d => (d === landed.text ? '' : d.startsWith(`${landed.text}\n`) ? d.slice(landed.text.length + 1) : d));
  }, [landed]);

  // Stop. A send still in flight is withdrawn, and its text comes back.
  const stop = () => {
    if (!state.running) return;
    if (sending && lastTurn) setDraft(d => (d.trim() ? `${lastTurn.user}\n${d}` : lastTurn.user));
    onStop();
  };
  const stopRef = useRef(stop);
  useEffect(() => { stopRef.current = stop; });
  // Esc anywhere in the chat stops it. The chat stays mounted when it is not
  // on screen, so only a visible chat answers, and not when something else
  // (a dialog, a menu) already took the key.
  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      const el = root.current;
      if (!el || !shown(el)) return;
      const active = document.activeElement;
      // The chat is its column and the side panel beside it (data-chat-scope).
      const scope = el.closest('[data-chat-scope]') ?? el;
      if (active && active !== document.body && !scope.contains(active)) return;
      stopRef.current();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
  useEffect(() => {
    const el = scroller.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [state.turns, state.approvals]);

  const submit = () => {
    const text = draft.trim();
    if (!text || state.running) return;
    setDraft('');
    onSend(text);
  };
  const editResend = (text: string) => { setDraft(text); box.current?.focus(); };
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submit(); }
  };

  // The target the chat is bound to, in force at each group (the last bind
  // before it), so a search line can say where it searched.
  const boundBefore = new Map<string, string>();
  {
    let bound = '';
    state.turns.forEach((t, ti) => blocksOf(t).forEach(b => {
      if (b.kind !== 'group') return;
      boundBefore.set(`${ti}:${b.key}`, bound);
      bound = boundAfter(b.calls, bound);
    }));
  }
  const total = sessionCost(state.turns);
  const hasText = !!draft.trim();

  return (
    <div ref={root} data-testid="chat-panel" style={panel}>
      <div ref={scroller} data-testid="chat-transcript" style={transcript}>
        <div style={TRANSCRIPT_COLUMN}>
          {state.turns.map((t, ti) => {
            const live = state.running && ti === state.turns.length - 1;
            const blocks = blocksOf(t);
            const lastBlock = blocks[blocks.length - 1];
            const answer = answerOf(blocks);
            const facts = turnFacts[ti];
            const factPlugin = remarkFactLinks(facts, repos);
            return (
              <FactLinkContext.Provider key={ti} value={{ facts, openKey: factLinks?.openKey ?? null, onOpen: factLinks?.onOpen, onBrowse: factLinks?.onBrowse }}>
              <div data-testid={`chat-turn-${ti}`} style={turnBox(ti > 0)}>
                {t.user && (
                  <div data-testid="chat-user" data-sending={t.local && !t.local.accepted ? true : undefined} className="k-chat-user k-prose"
                    style={t.local && !t.local.accepted ? sendingStrip : undefined}>
                    <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{t.user}</ReactMarkdown>
                  </div>
                )}
                {t.local && !t.local.accepted && <SendingLine />}
                {live && t.parts.length === 0 && !(t.local && !t.local.accepted) && (
                  <ThinkingPlaceholder since={t.acceptedAt} expectedMs={state.expectedFirstTokenMs} thinking={t.thinking} />
                )}
                {blocks.map(b => {
                  if (b.kind === 'text') {
                    return (
                      <div key={b.key} data-testid="chat-text" className="k-prose k-chat-answer">
                        <ReactMarkdown remarkPlugins={[...(live && b === lastBlock ? chatLivePlugins : chatPlugins), factPlugin]} components={chatComponents}>{b.text}</ReactMarkdown>
                      </div>
                    );
                  }
                  return (
                    <ChatToolGroup key={b.key} lead={b.lead} calls={b.calls} approvals={state.approvals} bound={boundBefore.get(`${ti}:${b.key}`) ?? ''}
                      running={live} onStop={onStop} selectedId={selectedId} onSelect={onSelect} onAnswer={onAnswer} />
                  );
                })}
                {t.stopped && !live && (
                  <StoppedMark latest={ti === state.turns.length - 1} onEdit={() => editResend(t.user)}
                    onContinue={() => { if (!state.running) onSend(CHAT_CONTINUE_MESSAGE); }} />
                )}
                <TurnNotices turn={t} modelName={modelName} onNewChat={onNewChat} />
                {t.usage && !live && (
                  <ChatTurnFooter usage={t.usage} costUsd={t.costUsd} answer={answer}
                    sessionCostUsd={total} sessionTurns={state.turns.length} />
                )}
              </div>
              </FactLinkContext.Provider>
            );
          })}
          {actionError && <div role="alert"><NoticeLine tone="blocked" testId="chat-action-error" lead={actionError} /></div>}
        </div>
      </div>
      <div style={{ ...COLUMN, flexShrink: 0, paddingBottom: 14 }}>
        <div style={composer}>
          <textarea
            ref={box}
            data-testid="chat-input"
            value={draft}
            rows={2}
            placeholder={state.turns.length > 0 ? CHAT_REPLY_PLACEHOLDER : CHAT_INPUT_PLACEHOLDER}
            aria-label={CHAT_INPUT_PLACEHOLDER}
            onChange={e => setDraft(e.target.value)}
            onKeyDown={onKey}
            style={input}
          />
          <div data-testid="chat-composer-row" style={composerRow}>
            <span style={leftGroup}>
              <span data-testid="chat-context" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                <span>{CONTEXT_LABEL}</span>
                <ContextMeter used={context.used} window={context.window} />
                <span data-testid="chat-context-text">
                  {compactCount(context.used)}{context.window > 0 ? ` / ${compactCount(context.window)}` : ''}
                </span>
              </span>
              <span style={{ color: '#5a5a5a' }}>{COMPOSER_HINT}</span>
              <span style={{ flex: 1 }} />
            </span>
            {state.running
              ? (
                <button type="button" data-testid="chat-stop" onClick={stop} style={stopBtn}>
                  <svg aria-hidden="true" width="10" height="10" viewBox="0 0 10 10"><rect width="10" height="10" rx="2" fill="currentColor" /></svg>
                  {CHAT_STOP}
                  <kbd style={keycap}>{CHAT_STOP_KEY}</kbd>
                </button>
              )
              : (
                <button type="button" data-testid="chat-send" aria-label={CHAT_SEND} title={CHAT_SEND} disabled={!hasText}
                  onClick={submit} style={sendBtn(hasText)}>
                  <svg aria-hidden="true" width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
                    <path d="M7 12V2M2.5 6.5 7 2l4.5 4.5" />
                  </svg>
                </button>
              )}
          </div>
        </div>
        {notSent && (
          <div data-testid="chat-not-sent" style={notSentLine}>
            <span aria-hidden="true" style={{ width: 6, height: 6, borderRadius: '50%', background: '#b0736c', flexShrink: 0 }} />
            <span title={notSent.reason}>{NOT_SENT}: {notSent.reason}</span>
            <button type="button" data-testid="chat-retry" onClick={submit} style={retryLink}>{NOT_SENT_RETRY}</button>
          </div>
        )}
      </div>
    </div>
  );
}

/** Whether an element is on screen: no ancestor is display:none (a chat that is
 *  not the one shown stays mounted, hidden). */
function shown(el: HTMLElement | null): boolean {
  for (let n = el; n; n = n.parentElement) if (getComputedStyle(n).display === 'none') return false;
  return true;
}

const panel: CSSProperties = { display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 };
const transcript: CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '4px 0', display: 'flex', flexDirection: 'column' };
const turnBox = (sep: boolean): CSSProperties => ({ padding: '18px 0 14px', ...(sep ? { borderTop: '1px solid #1c1c1c' } : {}) });
const composer: CSSProperties = { background: '#171717', border: '1px solid #2e2e2e', borderRadius: 12, padding: '10px 12px 8px' };
const input: CSSProperties = {
  display: 'block', width: '100%', resize: 'none', background: 'transparent', color: '#ddd', border: 'none',
  padding: '2px 2px 8px', fontSize: 13.5, fontFamily: 'inherit', outline: 'none',
};
// The Stop/Send button always stays on this row: the meter and the hint wrap
// inside their own group instead of pushing it down.
const leftGroup: CSSProperties = { whiteSpace: 'nowrap', display: 'flex', alignItems: 'center', flexWrap: 'wrap', rowGap: 2, columnGap: 12, flex: 1, minWidth: 0 };
const composerRow: CSSProperties = {
  display: 'flex', alignItems: 'center', flexWrap: 'nowrap', columnGap: 12,
  fontSize: 11, fontFamily: 'var(--k-font-mono)', color: '#6a6a6a', minHeight: 28,
};
const sendBtn = (active: boolean): CSSProperties => ({
  width: 28, height: 28, borderRadius: '50%', display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  background: active ? '#e8c27a' : '#222', color: active ? '#1a1405' : '#666', border: 'none', padding: 0,
  cursor: active ? 'pointer' : 'default', flexShrink: 0,
});
const stopBtn: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 7, height: 28, padding: '0 11px 0 9px', borderRadius: 8, flexShrink: 0,
  background: '#2b1c1c', color: '#f3c0c0', border: '1px solid #5c2a2a', fontFamily: 'inherit', fontSize: 12, fontWeight: 500, cursor: 'pointer',
};
const keycap: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 9.5, color: '#a07a7a', border: '1px solid #5c2a2a', borderRadius: 3, padding: '0 4px', fontWeight: 400,
};
const sendingStrip: CSSProperties = { opacity: 0.6, borderLeftStyle: 'dashed', background: '#161718' };
const notSentLine: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, margin: '7px 0 0', padding: '0 4px', fontSize: 11, color: '#8a7a78' };
const retryLink: CSSProperties = {
  background: 'transparent', border: 'none', padding: 0, font: 'inherit', color: '#c8c8c8', cursor: 'pointer',
  textDecoration: 'underline', textDecorationColor: '#4a4a4a', textUnderlineOffset: 3,
};
