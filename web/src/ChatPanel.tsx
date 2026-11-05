import { useEffect, useRef, useState } from 'react';
import type { CSSProperties, KeyboardEvent } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { ChatToolGroup } from './ChatToolGroup';
import { usageLine } from './chatState';
import type { ChatState, ChatTurn, ToolCall } from './chatState';
import type { ApprovalDecision } from './chatApi';
import { CHAT_INPUT_PLACEHOLDER_DRAFT, CHAT_SEND_DRAFT, CHAT_STOP_DRAFT } from './chatCopy';

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

/**
 * ChatPanel is the conversation: each turn's message, its tool groups, its
 * final text as markdown, and its usage and estimated cost; the server's
 * price-tier warning; and the box to write in. It sits where Browse's results
 * list sits.
 */
export function ChatPanel({ state, selectedId, actionError, onSend, onStop, onSelect, onAnswer }: {
  state: ChatState;
  selectedId: string | null;
  /** The last failed send, cancel or answer. */
  actionError: string;
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

  return (
    <div data-testid="chat-panel" style={panel}>
      <div ref={scroller} data-testid="chat-transcript" style={transcript}>
        {state.turns.map((t, ti) => (
          <div key={ti} data-testid={`chat-turn-${ti}`} style={{ marginBottom: 18 }}>
            {t.user && <div data-testid="chat-user" style={userMsg}>{t.user}</div>}
            {blocksOf(t).map(b => b.kind === 'group'
              ? <ChatToolGroup key={b.key} lead={b.lead} calls={b.calls} approvals={state.approvals}
                  selectedId={selectedId} onSelect={onSelect} onAnswer={onAnswer} />
              : (
                <div key={b.key} data-testid="chat-text" className="k-prose" style={assistantText}>
                  <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{b.text}</ReactMarkdown>
                </div>
              ))}
            {t.error && <div role="alert" data-testid="chat-turn-error" style={errorBox}>{t.error}</div>}
            {t.usage && <div data-testid="chat-usage" style={usage}>{usageLine(t.usage, t.costUsd)}</div>}
          </div>
        ))}
        {state.warning && <div data-testid="chat-warning" style={warningBox}>{state.warning}</div>}
        {actionError && <div role="alert" data-testid="chat-action-error" style={errorBox}>{actionError}</div>}
      </div>
      <div style={composer}>
        <textarea
          data-testid="chat-input"
          value={draft}
          rows={2}
          placeholder={CHAT_INPUT_PLACEHOLDER_DRAFT}
          aria-label={CHAT_INPUT_PLACEHOLDER_DRAFT}
          onChange={e => setDraft(e.target.value)}
          onKeyDown={onKey}
          style={input}
        />
        {state.running
          ? <button type="button" data-testid="chat-stop" onClick={onStop} style={sendBtn(false)}>{CHAT_STOP_DRAFT}</button>
          : <button type="button" data-testid="chat-send" disabled={!draft.trim()} onClick={submit} style={sendBtn(!draft.trim())}>{CHAT_SEND_DRAFT}</button>}
      </div>
    </div>
  );
}

const panel: CSSProperties = { display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 };
const transcript: CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '4px 2px' };
const userMsg: CSSProperties = {
  background: '#1a1f2b', color: '#dde4f2', borderRadius: 6, padding: '8px 12px', fontSize: 13.5,
  whiteSpace: 'pre-wrap', marginBottom: 8,
};
const assistantText: CSSProperties = { color: '#ccc', lineHeight: 1.6, fontSize: 14, margin: '6px 0' };
const usage: CSSProperties = { color: '#777', fontSize: 11, fontFamily: 'var(--k-font-mono)', marginTop: 4 };
const errorBox: CSSProperties = { background: '#2b1c1c', color: '#e0a0a0', border: '1px solid #3a2a2a', borderRadius: 4, padding: '6px 10px', fontSize: 12.5, margin: '6px 0' };
const warningBox: CSSProperties = { background: '#2a200e', color: '#f5c47a', border: '1px solid #a36a18', borderRadius: 4, padding: '6px 10px', fontSize: 12.5, margin: '6px 0' };
const composer: CSSProperties = { display: 'flex', gap: 8, alignItems: 'flex-end', paddingTop: 8, borderTop: '1px solid #222', flexShrink: 0 };
const input: CSSProperties = {
  flex: 1, minWidth: 0, resize: 'none', background: '#151515', color: '#ddd', border: '1px solid #2a2a2a',
  borderRadius: 4, padding: '8px 10px', fontSize: 13.5, fontFamily: 'inherit', outline: 'none',
};
const sendBtn = (disabled: boolean): CSSProperties => ({
  background: disabled ? 'transparent' : '#1d4ed8', color: disabled ? '#666' : '#fff', border: '1px solid ' + (disabled ? '#2a2a2a' : '#1d4ed8'),
  borderRadius: 4, padding: '7px 14px', fontSize: 13, cursor: disabled ? 'default' : 'pointer', flexShrink: 0,
});
