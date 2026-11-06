import { useState } from 'react';
import type { CSSProperties } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import type { ChatApproval, ApprovalDecision } from './chatApi';
import type { ToolCall } from './chatState';
import { toolRowLabel, toolKind, callTarget, boundAfter, keyArg, queryText, formatDuration, wallDurationMs } from './chatState';
import {
  APPROVAL_ALLOW, APPROVAL_DECLINE, APPROVAL_ALLOW_SESSION, APPROVAL_TARGET_UNRESOLVED, CHAT_STOP,
  activitySummary, activitySteps, liveStep, toolVerb, resultCount,
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
 * shows the live step in amber with a Stop pill. A call waiting for the person
 * carries its approval card inside its row, and holds its group open, since a
 * card nobody can see would stall the turn.
 */
export function ChatToolGroup({ lead, calls, approvals, selectedId, bound = '', running = false, onStop, onSelect, onAnswer }: {
  lead: string[];
  calls: ToolCall[];
  approvals: Record<string, ChatApproval>;
  selectedId: string | null;
  /** The repo or lens the chat was bound to before this group. */
  bound?: string;
  /** The turn this group belongs to is still running. */
  running?: boolean;
  onStop?: () => void;
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
  const summary = activitySummary(calls.map((c, i) => ({ kind: toolKind(c.name), target: targets[i] })));
  const wall = wallDurationMs(calls);
  const runningIdx = calls.findIndex(c => c.state === 'running');
  const runningCall = runningIdx >= 0 ? calls[runningIdx] : null;
  const liveText = live && runningCall && toolKind(runningCall.name) === 'query' && queryText(runningCall)
    ? liveStep(targets[runningIdx], queryText(runningCall)) : null;

  return (
    <div data-testid="chat-tool-group" data-open={open} data-live={live || undefined} style={{ margin: '0 0 10px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, width: 'max-content', maxWidth: '100%' }}>
        <button type="button" data-testid="chat-tool-group-toggle" aria-expanded={open}
          onClick={() => setOpened(o => !o)} style={header}>
          <span style={tile}><KnomitMark /></span>
          {liveText
            ? <span data-testid="chat-activity-live" style={{ color: AMBER }}>{segs(liveText)}</span>
            : (
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
        {live && onStop && (
          <button type="button" data-testid="chat-activity-stop" onClick={onStop} style={stopPill}>
            <span aria-hidden="true" style={{ width: 8, height: 8, background: '#f3c0c0', borderRadius: 1, display: 'inline-block' }} />
            {CHAT_STOP}
          </button>
        )}
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
            const arg = keyArg(c);
            return (
              <div key={c.id} data-testid={`chat-tool-row-${c.id}`} data-state={c.state} data-error={c.isError || undefined} style={{ position: 'relative' }}>
                <span aria-hidden="true" data-testid={`chat-tool-dot-${c.id}`} style={dot(c.state, c.isError)} />
                <button type="button" data-testid={`chat-tool-select-${c.id}`} disabled={!canOpen}
                  aria-pressed={selected} onClick={() => onSelect(c)} style={rowBtn(selected, c.isError)}>
                  <span style={{ color: c.state === 'running' ? AMBER : undefined }}>{toolVerb(toolKind(c.name), c.state === 'running') ?? toolRowLabel(c.name)}</span>
                  {arg && <span data-testid={`chat-tool-arg-${c.id}`} style={argMono}>{arg}</span>}
                  {(c.summary || c.durationMs) && (
                    <span data-testid={`chat-tool-meta-${c.id}`} style={metaMono}>
                      {c.summary && toolKind(c.name) === 'query' && <span data-testid={`chat-tool-size-${c.id}`}>{resultCount(c.summary.count)}</span>}
                      {c.durationMs ? <span data-testid={`chat-tool-time-${c.id}`}>{formatDuration(c.durationMs)}</span> : null}
                    </span>
                  )}
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
const stopPill: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 6, background: '#2b1c1c', color: '#f3c0c0', border: '1px solid #5c2a2a',
  borderRadius: 999, padding: '2px 10px', fontSize: 12, cursor: 'pointer', fontFamily: 'inherit',
};
const timeline: CSSProperties = { marginLeft: 14, paddingLeft: 14, borderLeft: '1px solid #262626', padding: '2px 0 2px 14px' };
const interstitial: CSSProperties = { color: '#888', fontStyle: 'italic', fontSize: 12.5, lineHeight: 1.5, margin: '4px 0 6px' };
const dot = (state: 'running' | 'done', isError: boolean): CSSProperties => ({
  position: 'absolute', left: -19, top: 11, width: 7, height: 7, borderRadius: '50%', boxSizing: 'border-box',
  ...(state === 'running'
    ? { background: AMBER, boxShadow: '0 0 0 3px rgba(232,194,122,.18)' }
    : { background: isError ? '#e0a0a0' : 'rgba(119,204,153,.25)', border: `1px solid ${isError ? '#e0a0a0' : '#4a8a66'}` }),
});
const rowBtn = (selected: boolean, isError: boolean): CSSProperties => ({
  display: 'flex', alignItems: 'baseline', gap: 10, width: '100%', textAlign: 'left', background: selected ? '#1b1f2a' : 'transparent',
  border: 'none', borderRadius: 4, padding: '4px 8px', fontSize: 12.5, fontFamily: 'inherit',
  color: isError ? '#e0a0a0' : '#c8c8c8', cursor: 'pointer',
});
const argMono: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#9aa4b2', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};
const metaMono: CSSProperties = {
  marginLeft: 'auto', display: 'inline-flex', gap: 10, flexShrink: 0, fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#666',
};
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
