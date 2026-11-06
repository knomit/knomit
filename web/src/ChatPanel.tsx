import { useEffect, useRef, useState } from 'react';
import type { CSSProperties, KeyboardEvent } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { ChatToolGroup } from './ChatToolGroup';
import { ChatTurnFooter } from './ChatTurnFooter';
import { ContextMeter } from './ChatPill';
import { chatPlugins, chatComponents } from './chatTableBars';
import { boundAfter, compactCount, sessionCost } from './chatState';
import type { ChatState, ChatTurn, ToolCall } from './chatState';
import type { ApprovalDecision } from './chatApi';
import {
  CHAT_INPUT_PLACEHOLDER, CHAT_REPLY_PLACEHOLDER, CHAT_SEND, CHAT_STOP, COMPOSER_HINT, CONTEXT_LABEL,
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

/** The size of the chat's context: tokens used by the latest prompt, and the
 *  model's window (0 until it is known). */
export interface ChatContextUse { used: number; window: number }

/** The column the transcript and the composer share. */
const COLUMN: CSSProperties = { width: 740, maxWidth: 'calc(100% - 48px)', margin: '0 auto' };

/**
 * ChatPanel is the conversation, in a centered column: each turn's message (a
 * faint strip), its activity line, its final text as markdown, and a cost
 * footer; the server's price-tier warning; and the rounded box to write in.
 */
export function ChatPanel({ state, selectedId, actionError, context, onSend, onStop, onSelect, onAnswer }: {
  state: ChatState;
  selectedId: string | null;
  /** The last failed send, cancel or answer. */
  actionError: string;
  context: ChatContextUse;
  onSend: (text: string) => void;
  onStop: () => void;
  onSelect: (call: ToolCall) => void;
  onAnswer: (callId: string, decision: ApprovalDecision) => void;
}) {
  const [draft, setDraft] = useState('');
  const scroller = useRef<HTMLDivElement>(null);
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
    <div data-testid="chat-panel" style={panel}>
      <div ref={scroller} data-testid="chat-transcript" style={transcript}>
        <div style={COLUMN}>
          {state.turns.map((t, ti) => {
            const live = state.running && ti === state.turns.length - 1;
            const blocks = blocksOf(t);
            const lastBlock = blocks[blocks.length - 1];
            const answer = blocks.filter(b => b.kind === 'text').map(b => (b as { text: string }).text).join('\n\n');
            return (
              <div key={ti} data-testid={`chat-turn-${ti}`} style={turnBox(ti > 0)}>
                {t.user && (
                  <div data-testid="chat-user" className="k-chat-user k-prose">
                    <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{t.user}</ReactMarkdown>
                  </div>
                )}
                {blocks.map(b => {
                  if (b.kind === 'text') {
                    return (
                      <div key={b.key} data-testid="chat-text" className="k-prose k-chat-answer">
                        <ReactMarkdown remarkPlugins={chatPlugins} components={chatComponents}>{b.text}</ReactMarkdown>
                        {live && b === lastBlock && <span data-testid="chat-caret" className="k-chat-caret" aria-hidden="true" />}
                      </div>
                    );
                  }
                  return (
                    <ChatToolGroup key={b.key} lead={b.lead} calls={b.calls} approvals={state.approvals} bound={boundBefore.get(`${ti}:${b.key}`) ?? ''}
                      running={live} onStop={onStop} selectedId={selectedId} onSelect={onSelect} onAnswer={onAnswer} />
                  );
                })}
                {t.warning && <div data-testid="chat-warning" style={warningBox}>{t.warning}</div>}
                {t.error && <div role="alert" data-testid="chat-turn-error" style={errorBox}>{t.error}</div>}
                {t.usage && !live && (
                  <ChatTurnFooter usage={t.usage} costUsd={t.costUsd} answer={answer}
                    sessionCostUsd={total} sessionTurns={state.turns.length} />
                )}
              </div>
            );
          })}
          {actionError && <div role="alert" data-testid="chat-action-error" style={errorBox}>{actionError}</div>}
        </div>
      </div>
      <div style={{ ...COLUMN, flexShrink: 0, paddingBottom: 14 }}>
        <div style={composer}>
          <textarea
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
            <span data-testid="chat-context" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
              <span>{CONTEXT_LABEL}</span>
              <ContextMeter used={context.used} window={context.window} />
              <span data-testid="chat-context-text">
                {compactCount(context.used)}{context.window > 0 ? ` / ${compactCount(context.window)}` : ''}
              </span>
            </span>
            <span style={{ color: '#5a5a5a' }}>{COMPOSER_HINT}</span>
            <span style={{ flex: 1 }} />
            {state.running
              ? <button type="button" data-testid="chat-stop" onClick={onStop} style={stopBtn}>{CHAT_STOP}</button>
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
      </div>
    </div>
  );
}

const panel: CSSProperties = { display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 };
const transcript: CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '4px 0' };
const turnBox = (sep: boolean): CSSProperties => ({ padding: '18px 0 14px', ...(sep ? { borderTop: '1px solid #1c1c1c' } : {}) });
const errorBox: CSSProperties = { background: '#2b1c1c', color: '#e0a0a0', border: '1px solid #3a2a2a', borderRadius: 4, padding: '6px 10px', fontSize: 12.5, margin: '6px 0' };
const warningBox: CSSProperties = { background: '#2a200e', color: '#f5c47a', border: '1px solid #a36a18', borderRadius: 4, padding: '6px 10px', fontSize: 12.5, margin: '6px 0' };
const composer: CSSProperties = { background: '#171717', border: '1px solid #2e2e2e', borderRadius: 12, padding: '10px 12px 8px' };
const input: CSSProperties = {
  display: 'block', width: '100%', resize: 'none', background: 'transparent', color: '#ddd', border: 'none',
  padding: '2px 2px 8px', fontSize: 13.5, fontFamily: 'inherit', outline: 'none',
};
const composerRow: CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 12, fontSize: 11, fontFamily: 'var(--k-font-mono)', color: '#6a6a6a', minHeight: 28,
};
const sendBtn = (active: boolean): CSSProperties => ({
  width: 28, height: 28, borderRadius: '50%', display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  background: active ? '#e8c27a' : '#222', color: active ? '#1a1405' : '#666', border: 'none', padding: 0,
  cursor: active ? 'pointer' : 'default', flexShrink: 0,
});
const stopBtn: CSSProperties = {
  background: '#1d4ed8', color: '#fff', border: '1px solid #1d4ed8', borderRadius: 4,
  padding: '7px 14px', fontSize: 13, cursor: 'pointer', flexShrink: 0,
};
