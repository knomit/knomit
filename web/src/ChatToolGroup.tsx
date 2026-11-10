import { useState } from 'react';
import type { CSSProperties } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import type { ChatApproval, ApprovalDecision } from './chatApi';
import type { ToolCall } from './chatState';
import { toolCallLabel, isBind, callTarget, boundAfter, formatDuration, wallDurationMs } from './chatState';
import {
  APPROVAL_ALLOW, APPROVAL_DECLINE, APPROVAL_ALLOW_SESSION, APPROVAL_TARGET_UNRESOLVED,
  activitySummary, activitySteps, resultCount,
} from './chatCopy';
import type { SummarySeg } from './chatCopy';

/** The compact JSON of a call's arguments, bounded so a large call stays a card. */
function argsText(args: unknown): string {
  const t = JSON.stringify(args);
  if (!t || t === '{}' || t === 'null') return '';
  return t.length > 600 ? `${t.slice(0, 600)}…` : t;
}

const AMBER = '#e8c27a';

/** The knomit mark, in the 16px tile the activity line starts with. */
export function KnomitMark({ size = 16 }: { size?: number }) {
  return (
    <svg aria-hidden="true" width={size} height={size} viewBox="0 0 80 80" style={{ display: 'block', flexShrink: 0 }}>
      <rect x="12" y="12" width="56" height="56" rx="10" transform="rotate(45 40 40)" fill="#7c9" />
      <line x1="30" y1="24" x2="30" y2="56" stroke="#111" strokeWidth="4" strokeLinecap="round" />
      <path d="M30 40 Q38 36 50 24" stroke="#111" strokeWidth="4" fill="none" strokeLinecap="round" />
      <path d="M30 40 Q38 44 50 56" stroke="#111" strokeWidth="4" fill="none" strokeLinecap="round" />
    </svg>
  );
}

const segs = (list: SummarySeg[]) => list.map((s, i) => (s.strong ? <strong key={i} style={{ color: '#bbb', fontWeight: 500 }}>{s.text}</strong> : <span key={i}>{s.text}</span>));

/**
 * ChatToolGroup is a run of consecutive tool calls drawn as ONE ACTIVITY LINE:
 * the knomit mark, a summary worded from the calls (chatCopy.activitySummary),
 * the step count, and a chevron. Collapsed by default. Expanded it is a
 * timeline: the model's interstitial text (`lead`) and one row per call. A row
 * that has finished can be selected: the page then fetches that call's result
 * and shows it in the result panel. While the turn runs, the collapsed line
 * is drawn amber. It has no Stop of its own: the composer's Stop (and Esc)
 * is the one way to stop a turn. A call waiting for the person
 * carries its approval card inside its row, and holds its group open, since a
 * card nobody can see would stall the turn.
 */
export function ChatToolGroup({ lead, calls, approvals, selectedId, bound = '', running = false, onSelect, onAnswer }: {
  lead: string[];
  calls: ToolCall[];
  approvals: Record<string, ChatApproval>;
  selectedId: string | null;
  /** The repo or lens the chat was bound to before this group. */
  bound?: string;
  /** The turn this group belongs to is still running. */
  running?: boolean;
  onSelect: (call: ToolCall) => void;
  onAnswer: (callId: string, decision: ApprovalDecision) => void;
}) {
  const [opened, setOpened] = useState(false);
  const waiting = calls.some(c => approvals[c.id]);
  const open = opened || waiting;
  const live = running && calls.some(c => c.state === 'running');

  // Each call's target: its own, else the binding in force where it ran.
  const targets: string[] = [];
  let inForce = bound;
  for (const c of calls) {
    targets.push(callTarget(c, inForce));
    inForce = boundAfter([c], inForce);
  }
  const summary = activitySummary(calls.map((c, i) => ({ title: toolCallLabel(c), target: targets[i], bind: isBind(c.name), failed: c.isError })));
  const wall = wallDurationMs(calls);

  return (
    <div data-testid="chat-tool-group" data-open={open} data-live={live || undefined} style={{ margin: '0 0 10px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, width: 'max-content', maxWidth: '100%' }}>
        <button type="button" data-testid="chat-tool-group-toggle" aria-expanded={open}
          onClick={() => setOpened(o => !o)} style={header}>
          <span style={tile}><KnomitMark /></span>
          {summary.length > 0 && (
            // Nothing to say (a group of failed binds only): no empty item, so no double gap.
            // While the turn runs the same line is drawn amber: it never words the step itself.
            <span data-testid="chat-activity-summary" style={{ color: live ? AMBER : '#8a8a8a', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis' }}>
              {summary.map((piece, i) => (
                <span key={i}>{i > 0 && ' · '}{segs(piece)}</span>
              ))}
            </span>
          )}
          <span data-testid="chat-activity-steps" style={steps}>
            {activitySteps(calls.length)}{wall > 0 && ` · ${formatDuration(wall)}`}
          </span>
          <span aria-hidden="true" style={{ color: '#555', fontSize: 11, transform: open ? 'rotate(90deg)' : 'none', display: 'inline-block' }}>›</span>
        </button>
      </div>
      {open && (
        <div data-testid="chat-timeline" style={timeline}>
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
              <div key={c.id} data-testid={`chat-tool-row-${c.id}`} data-state={c.state} data-error={c.isError || undefined} style={{ position: 'relative' }}>
                <ToolCallDot call={c} testId={`chat-tool-dot-${c.id}`} onTimeline />
                <button type="button" data-testid={`chat-tool-select-${c.id}`} disabled={!canOpen}
                  aria-current={selected ? 'true' : undefined} onClick={() => onSelect(c)} style={rowBtn(selected)}>
                  <ToolCallLine call={c} testPrefix="chat-tool" />
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

/** A call's state mark: amber while it runs, green when it succeeded, red when it
 *  failed. On the timeline it sits on the rail; elsewhere it is inline. */
function ToolCallDot({ call, testId, onTimeline = false }: { call: ToolCall; testId?: string; onTimeline?: boolean }) {
  return <span aria-hidden="true" data-call-part="dot" data-testid={testId} style={dot(call.state, call.isError, onTimeline)} />;
}

/** What a call's row says: "<title> · <main argument>" (toolCallLabel and the
 *  main argument the server sent), then its size and time, in the row's
 *  colours (red for a failed call: the colour says it failed, no word does). */
function ToolCallLine({ call: c, testPrefix }: { call: ToolCall; testPrefix: string }) {
  const arg = c.keyArg;
  return (
    <span data-call-part="line" style={line(c.isError)}>
      <span style={words}>
        <span data-call-part="label" style={{ color: c.state === 'running' ? AMBER : undefined, flexShrink: 0 }}>{toolCallLabel(c)}</span>
        {arg && <span data-call-part="sep" aria-hidden="true" style={sep}>·</span>}
        {arg && <span data-call-part="arg" data-testid={`${testPrefix}-arg-${c.id}`} style={argMono}>{arg}</span>}
      </span>
      {(c.summary || c.durationMs) && (
        <span data-testid={`${testPrefix}-meta-${c.id}`} style={metaMono}>
          {c.summary && <span data-testid={`${testPrefix}-size-${c.id}`}>{resultCount(c.summary.count)}</span>}
          {c.durationMs ? <span data-testid={`${testPrefix}-time-${c.id}`}>{formatDuration(c.durationMs)}</span> : null}
        </span>
      )}
    </span>
  );
}

/**
 * ToolCallHead is a call's timeline row drawn as a heading: the same dot and
 * the same line (ToolCallDot, ToolCallLine), so the side panel that shows a
 * call's result is headed by the very row that was clicked, failed or not.
 * `style` places it (the side panel's top row sets its own spacing).
 */
export function ToolCallHead({ call, testId, style }: { call: ToolCall; testId?: string; style?: CSSProperties }) {
  return (
    <div data-testid={testId} data-state={call.state} data-error={call.isError || undefined} style={style ? { ...headRow, ...style } : headRow}>
      <ToolCallDot call={call} />
      <ToolCallLine call={call} testPrefix="chat-result-call" />
    </div>
  );
}

/** The approval inside a tool row: what is asked, of what, and the three
 *  answers. "Allow for session" is offered only for a call the server can
 *  grant (an unknown tool or an unresolved handle cannot be).
 *  One grey card for every approval (chat review, 2026-11-09, option B1): a
 *  destructive one is marked by its action word alone, drawn amber; the
 *  answers are one line of text links, Allow in green, split by a muted dot. */
function ApprovalCard({ approval, onAnswer }: { approval: ChatApproval; onAnswer: (d: ApprovalDecision) => void }) {
  const target = approval.target || APPROVAL_TARGET_UNRESOLVED;
  const args = argsText(approval.arguments);
  return (
    <div data-testid={`chat-approval-${approval.call_id}`} data-destructive={approval.destructive || undefined} style={card}>
      <div style={{ fontSize: 12, color: '#ccc' }}>
        <span data-testid="chat-approval-action" style={{ color: approval.destructive ? AMBER : '#ccc' }}>{approval.action}</span>
        <span style={{ color: '#777' }}>{' · '}</span>
        <span data-testid="chat-approval-target" title={approval.target_kind || undefined}>{target}</span>
      </div>
      {args && <pre data-testid="chat-approval-args" style={argsPre}>{args}</pre>}
      <div data-testid="chat-approval-answers" style={answers}>
        <button type="button" data-testid="chat-approval-allow" className="k-approval-answer k-approval-answer--allow" onClick={() => onAnswer('allow')}>{APPROVAL_ALLOW}</button>
        <span aria-hidden="true" style={answerSep}>·</span>
        <button type="button" data-testid="chat-approval-decline" className="k-approval-answer" onClick={() => onAnswer('decline')}>{APPROVAL_DECLINE}</button>
        {approval.grantable && (
          <>
            <span aria-hidden="true" style={answerSep}>·</span>
            <button type="button" data-testid="chat-approval-allow-session" className="k-approval-answer" onClick={() => onAnswer('allow_session')}>{APPROVAL_ALLOW_SESSION}</button>
          </>
        )}
      </div>
    </div>
  );
}

// The mockup's .actl: 12px #8a8a8a, 26px tall, as wide as its content, one line.
const header: CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 8, minWidth: 0, maxWidth: '100%', background: 'transparent', border: 'none',
  color: '#8a8a8a', fontSize: 12, fontWeight: 400, padding: '5px 9px 5px 7px', cursor: 'pointer', textAlign: 'left', fontFamily: 'inherit',
  borderRadius: 6, whiteSpace: 'nowrap',
};
const tile: CSSProperties = {
  width: 16, height: 16, borderRadius: 4, display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  background: '#1a231d', flexShrink: 0,
};
const steps: CSSProperties = { color: '#5a5a5a', fontSize: 10.5, fontWeight: 400, fontFamily: 'var(--k-font-mono)', flexShrink: 0 };
const timeline: CSSProperties = { marginLeft: 14, paddingLeft: 14, borderLeft: '1px solid #262626', padding: '2px 0 2px 14px' };
const interstitial: CSSProperties = { color: '#888', fontStyle: 'italic', fontSize: 12.5, lineHeight: 1.5, margin: '4px 0 6px' };
const dot = (state: 'running' | 'done', isError: boolean, onTimeline: boolean): CSSProperties => ({
  ...(onTimeline ? { position: 'absolute', left: -19, top: 11 } : { flexShrink: 0, alignSelf: 'center' }),
  width: 7, height: 7, borderRadius: '50%', boxSizing: 'border-box',
  ...(state === 'running'
    ? { background: AMBER, boxShadow: '0 0 0 3px rgba(232,194,122,.18)' }
    : { background: isError ? '#e0a0a0' : 'rgba(119,204,153,.25)', border: `1px solid ${isError ? '#e0a0a0' : '#4a8a66'}` }),
});
const rowBtn = (selected: boolean): CSSProperties => ({
  display: 'flex', width: '100%', textAlign: 'left', background: selected ? '#1b1f2a' : 'transparent',
  border: 'none', borderRadius: 4, padding: '4px 8px', fontFamily: 'inherit', cursor: 'pointer',
});
// The row's text: label, argument and meta on one baseline, red for a failed call.
const line = (isError: boolean): CSSProperties => ({
  display: 'flex', alignItems: 'baseline', gap: 10, flex: 1, minWidth: 0, fontSize: 12.5, fontWeight: 400, lineHeight: 'normal',
  color: isError ? '#e0a0a0' : '#c8c8c8',
});
// The heading: the row's dot beside the row's line, with the rail's gap (19px from dot to text, less the 7px dot).
const headRow: CSSProperties = { display: 'flex', alignItems: 'center', gap: 12, padding: '4px 0', margin: '0 0 12px' };
// The title, the separator and the main argument: closer together than the meta.
const words: CSSProperties = { display: 'flex', alignItems: 'baseline', gap: 6, minWidth: 0 };
const sep: CSSProperties = { color: '#666', flexShrink: 0 };
const argMono: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#9aa4b2', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};
const metaMono: CSSProperties = {
  marginLeft: 'auto', display: 'inline-flex', gap: 10, flexShrink: 0, fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#666',
};
// One card for every approval: a destructive one differs only by its amber action word.
const card: CSSProperties = {
  margin: '2px 8px 6px', padding: '8px 10px', borderRadius: 4, background: '#1a1a1a', border: '1px solid #2a2a2a',
};
const argsPre: CSSProperties = {
  margin: '6px 0 0', fontSize: 11, color: '#999', whiteSpace: 'pre-wrap', wordBreak: 'break-all',
  fontFamily: 'var(--k-font-mono)', maxHeight: 120, overflow: 'auto',
};
// The answers: one line of text links (.k-approval-answer in App.css draws them,
// with their hover and focus), split by a muted dot the screen reader skips.
const answers: CSSProperties = { display: 'flex', alignItems: 'baseline', gap: 2, marginTop: 6, fontSize: 12 };
const answerSep: CSSProperties = { color: '#444' };
