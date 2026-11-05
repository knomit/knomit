import { useState } from 'react';
import type { CSSProperties } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import type { ChatApproval, ApprovalDecision } from './chatApi';
import type { ToolCall } from './chatState';
import { toolRowLabel } from './chatState';
import {
  TOOL_GROUP_HEADER, APPROVAL_ALLOW, APPROVAL_DECLINE, APPROVAL_ALLOW_SESSION, APPROVAL_TARGET_UNRESOLVED,
} from './chatCopy';

/** The compact JSON of a call's arguments, bounded so a large call stays a card. */
function argsText(args: unknown): string {
  const t = JSON.stringify(args);
  if (!t || t === '{}' || t === 'null') return '';
  return t.length > 600 ? `${t.slice(0, 600)}…` : t;
}

/**
 * ChatToolGroup is a run of consecutive tool calls under one header, collapsed
 * by default. Expanded it shows the model's interstitial text (`lead`) and one
 * row per call. A row that has finished can be selected: the page then fetches
 * that call's result and shows it in the result panel. A call waiting for the
 * person carries its approval card inside its row, and holds its group open,
 * since a card nobody can see would stall the turn.
 */
export function ChatToolGroup({ lead, calls, approvals, selectedId, onSelect, onAnswer }: {
  lead: string[];
  calls: ToolCall[];
  approvals: Record<string, ChatApproval>;
  selectedId: string | null;
  onSelect: (call: ToolCall) => void;
  onAnswer: (callId: string, decision: ApprovalDecision) => void;
}) {
  const [opened, setOpened] = useState(false);
  const waiting = calls.some(c => approvals[c.id]);
  const open = opened || waiting;
  return (
    <div data-testid="chat-tool-group" data-open={open} style={group}>
      <button type="button" data-testid="chat-tool-group-toggle" aria-expanded={open}
        onClick={() => setOpened(o => !o)} style={header}>
        <span aria-hidden="true" style={{ display: 'inline-block', width: 12 }}>{open ? '▾' : '▸'}</span>
        <span>{TOOL_GROUP_HEADER}</span>
      </button>
      {open && (
        <div style={{ padding: '4px 10px 8px' }}>
          {lead.map((t, i) => (
            <div key={i} className="k-prose" style={interstitial}>
              <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{t}</ReactMarkdown>
            </div>
          ))}
          {calls.map(c => {
            const ap = approvals[c.id];
            const selected = selectedId === c.id;
            const canOpen = c.state === 'done';
            return (
              <div key={c.id} data-testid={`chat-tool-row-${c.id}`} data-state={c.state} data-error={c.isError || undefined}>
                <button type="button" data-testid={`chat-tool-select-${c.id}`} disabled={!canOpen}
                  aria-pressed={selected} onClick={() => onSelect(c)} style={rowBtn(selected, c.isError)}>
                  <span>{toolRowLabel(c.name)}</span>
                  {canOpen && <span aria-hidden="true">{' ▸'}</span>}
                </button>
                {ap && <ApprovalCard approval={ap} onAnswer={d => onAnswer(c.id, d)} />}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

/** The approval inside a tool row: what is asked, of what, and the three
 *  answers. "Allow for session" is offered only for a call the server can
 *  grant (an unknown tool or an unresolved handle cannot be). */
function ApprovalCard({ approval, onAnswer }: { approval: ChatApproval; onAnswer: (d: ApprovalDecision) => void }) {
  const target = approval.target || APPROVAL_TARGET_UNRESOLVED;
  const args = argsText(approval.arguments);
  return (
    <div data-testid={`chat-approval-${approval.call_id}`} data-destructive={approval.destructive || undefined} style={card(approval.destructive)}>
      <div style={{ fontSize: 12, color: '#ccc' }}>
        <span data-testid="chat-approval-action">{approval.action}</span>
        <span style={{ color: '#777' }}>{' · '}</span>
        <span data-testid="chat-approval-target" title={approval.target_kind || undefined}>{target}</span>
      </div>
      {args && <pre data-testid="chat-approval-args" style={argsPre}>{args}</pre>}
      <div style={{ display: 'flex', gap: 6, marginTop: 6 }}>
        <button type="button" data-testid="chat-approval-allow" onClick={() => onAnswer('allow')} style={btn}>{APPROVAL_ALLOW}</button>
        <button type="button" data-testid="chat-approval-decline" onClick={() => onAnswer('decline')} style={btn}>{APPROVAL_DECLINE}</button>
        {approval.grantable && (
          <button type="button" data-testid="chat-approval-allow-session" onClick={() => onAnswer('allow_session')} style={btn}>{APPROVAL_ALLOW_SESSION}</button>
        )}
      </div>
    </div>
  );
}

const group: CSSProperties = { border: '1px solid #262626', borderRadius: 6, margin: '6px 0', background: '#121212' };
const header: CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 4, width: '100%', background: 'transparent', border: 'none',
  color: '#aaa', fontSize: 12, padding: '6px 10px', cursor: 'pointer', textAlign: 'left', fontFamily: 'inherit',
};
const interstitial: CSSProperties = { color: '#aaa', fontSize: 13, lineHeight: 1.5, margin: '2px 0 6px' };
const rowBtn = (selected: boolean, isError: boolean): CSSProperties => ({
  display: 'block', width: '100%', textAlign: 'left', background: selected ? '#1d2230' : 'transparent',
  border: 'none', borderRadius: 4, padding: '4px 8px', fontSize: 12.5, fontFamily: 'inherit',
  color: isError ? '#e0a0a0' : '#bbb', cursor: 'pointer',
});
const card = (destructive: boolean): CSSProperties => ({
  margin: '2px 8px 6px', padding: '8px 10px', borderRadius: 4,
  background: destructive ? '#2b1c1c' : '#1a1a1a', border: `1px solid ${destructive ? '#5c2a2a' : '#2a2a2a'}`,
});
const argsPre: CSSProperties = {
  margin: '6px 0 0', fontSize: 11, color: '#999', whiteSpace: 'pre-wrap', wordBreak: 'break-all',
  fontFamily: 'var(--k-font-mono)', maxHeight: 120, overflow: 'auto',
};
const btn: CSSProperties = {
  background: '#222', color: '#ddd', border: '1px solid #333', borderRadius: 4,
  padding: '4px 10px', fontSize: 12, cursor: 'pointer', fontFamily: 'inherit',
};
