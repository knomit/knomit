// The chat page's visible copy, in one place so it can be reviewed.
//
// VERBATIM: wording the plan and the design give. Do not change it on your own
// ruling.
// APPROVED: wording the plan and design did not give, approved by the user on
// 2026-11-05. Do not change it on your own ruling either.
// New wording that is not yet approved is flagged in the report that adds it
// and needs the user's approval before it ships. (None at present.)
// The server's own sentences (the price-tier warning, the reasoning-truncated
// message, a refusal) are NOT here: the page shows them as the server sends
// them.

// ── VERBATIM (design "Chat" > Rendering, and Approvals) ──

/** The header of a group of consecutive tool calls. */
export const TOOL_GROUP_HEADER = 'Used Knomit';
export const APPROVAL_ALLOW = 'Allow';
export const APPROVAL_DECLINE = 'Decline';
export const APPROVAL_ALLOW_SESSION = 'Allow for session';
/** The two tabs of a chat target (the user's answer to Q1) live in
 *  TopBar's FACET_LABEL: 'Chat' and 'Settings'. */

// ── APPROVED (2026-11-05) ──

/** N14. The approval card's target when the server could not resolve the
 *  call's repo or lens handle (the card would otherwise show an empty target). */
export const APPROVAL_TARGET_UNRESOLVED = 'target not resolved';

/** The start form, when no provider offers a usable model. */
export const CHAT_NO_MODELS = 'No models are available to chat with.';

/** The message box and its two buttons. */
export const CHAT_INPUT_PLACEHOLDER = 'Message';
export const CHAT_SEND = 'Send';
export const CHAT_STOP = 'Stop';

/** The chat's Settings body when nothing has been authorized for the session. */
export const CHAT_NO_GRANTS = 'Nothing has been allowed for this session.';

/** The chat Settings line for the model's context window. `how` is what the
 *  server reports: "config", "memory", or "trained" (derived, and the model's
 *  own trained length was the smaller). */
export const CHAT_CONTEXT_HOW: Record<string, string> = {
  config: 'set in config',
  memory: "limited to fit this machine's memory",
  trained: "the model's trained length",
};
export function chatContextLine(window: number, how: string): string {
  const h = CHAT_CONTEXT_HOW[how];
  return h ? `Context window ${window} tokens, ${h}` : `Context window ${window} tokens`;
}

/** Shown when Ollama reports part of the model running on the CPU. */
export function chatSpillWarning(window: number): string {
  return `This model is partly running on CPU at a context of ${window} tokens. Set num_ctx for this provider in knomit.toml to a smaller value.`;
}

/** The result panel, while a tool result is being fetched. */
export const CHAT_RESULT_LOADING = 'Loading…';

/** The button on a Settings grant row that takes the grant back. */
export const CHAT_REVOKE_GRANT = 'Revoke';

/** The detail of the error shown when a send gets no answer in time
 *  (`<url> → <this>`, the same shape as every other request error). */
export const CHAT_SEND_TIMEOUT = 'no answer';

// ── APPROVED (2026-11-06) ──
// Chat UX v2, PR 1 (design 2026-11-05-chat-ux-v2, section 2). Approved by the
// user on 2026-11-06, with the singular forms ("1 step", "read 1 fact",
// "used 1 tool", "Session (1 turn)") and the timeline verbs below.

/** The activity line's words. ONE function words the whole summary: the pieces
 *  are joined with " · ". `strong` marks the parts drawn bold (the target and
 *  the counts). A tool with no wording here counts toward "used N tools". */
export interface SummarySeg { text: string; strong?: boolean }
export interface SummaryCall { kind: 'bind' | 'query' | 'read' | null; target: string }

export function activitySummary(calls: SummaryCall[]): SummarySeg[][] {
  const pieces: SummarySeg[][] = [];
  const bound: string[] = [];
  const queries = new Map<string, number>();
  let reads = 0;
  let other = 0;
  for (const c of calls) {
    if (c.kind === 'bind') { if (!bound.includes(c.target)) bound.push(c.target); }
    else if (c.kind === 'query') queries.set(c.target, (queries.get(c.target) ?? 0) + 1);
    else if (c.kind === 'read') reads++;
    else other++;
  }
  for (const t of bound) pieces.push(t ? [{ text: 'Bound to ' }, { text: t, strong: true }] : [{ text: 'Bound' }]);
  for (const [t, n] of queries) {
    pieces.push([{ text: 'searched ' }, { text: `${n}×`, strong: true }, ...(t ? [{ text: ' in ' + t }] : [])]);
  }
  if (reads) pieces.push([{ text: 'read ' }, { text: String(reads), strong: true }, { text: reads === 1 ? ' fact' : ' facts' }]);
  if (other) pieces.push([{ text: 'used ' }, { text: String(other), strong: true }, { text: other === 1 ? ' tool' : ' tools' }]);
  // The line opens a sentence: capitalize the first word.
  const first = pieces[0]?.[0];
  if (first) first.text = first.text.charAt(0).toUpperCase() + first.text.slice(1);
  return pieces;
}

/** The running step of the collapsed line: Searching <t> for "<q>"… */
export function liveStep(target: string, query: string): SummarySeg[] {
  return [{ text: 'Searching ' }, ...(target ? [{ text: target, strong: true }, { text: ' ' }] : []), { text: `for "${query}"…` }];
}

/** The activity line's step count: "N steps · T" (the mockup words one as "1 step"). */
export const activitySteps = (n: number): string => `${n} ${n === 1 ? 'step' : 'steps'}`;

/** The turn footer: "<in> in · <out> out", and the cache meter's label. */
export const COST_IN = 'in';
export const COST_OUT = 'out';
export const cachedLabel = (pct: number): string => `${pct}% cached`;

/** The cost hover card. */
export const COST_ROW_INPUT = 'Input';
export const COST_ROW_CACHE_READ = 'Cache read';
export const COST_ROW_CACHE_WRITE = 'Cache write (5m)';
export const COST_ROW_OUTPUT = 'Output';
export const COST_THIS_TURN = 'This turn';
/** Shown for a cost when the model has no price card. */
export const COST_UNPRICED = '–';
export const costSession = (turns: number): string => `Session (${turns} ${turns === 1 ? 'turn' : 'turns'})`;

/** The verb of a timeline row for the tools that have wording; every other tool
 *  keeps toolRowLabel. A query that is still running reads "Searching". */
export function toolVerb(kind: 'bind' | 'query' | 'read' | null, running: boolean): string | null {
  switch (kind) {
    case 'bind': return 'Bound';
    case 'query': return running ? 'Searching' : 'Searched';
    case 'read': return 'Read';
    default: return null;
  }
}

/** The top-bar pill and the composer's meter row. */
export const PILL_SESSION = 'session';
export const CONTEXT_LABEL = 'context';
export const COMPOSER_HINT = 'Enter to send';
export const CHAT_REPLY_PLACEHOLDER = 'Reply…';

// ── APPROVED (2026-11-06) ──
// Chat UX v2, PR 4. The rail hover card's field labels, approved by the user
// exactly as in the mockup (R2), with "1 turn" for one.

export const RAIL_TIP_MODEL = 'Model';
export const RAIL_TIP_STARTED = 'Started';
export const RAIL_TIP_COST = 'Cost';
export const railTurns = (n: number): string => `${n} ${n === 1 ? 'turn' : 'turns'}`;

// ── PENDING APPROVAL ──
// Chat UX v2, PR 4 (design section 6, the rail). Not yet approved by the user.

/** Rail: the expander under the five most recent chats. VERBATIM from the
 *  design's copy table ("N more"). */
export const railMore = (n: number): string => `${n} more`;
/** Rail: the state in words, in a row's tooltip. VERBATIM from the design's
 *  copy table. Idle has no words in the design (the gap is listed in the PR
 *  report), so the tooltip shows none for it. */
export const RAIL_STATE_RUNNING = 'Running';
export const RAIL_STATE_ATTENTION = 'Waiting for your approval';
export const RAIL_STATE_NEW_ANSWER = 'New answer';
export const RAIL_STATE_FAILED = 'Failed';
/** Rail: the hover x and its confirmation when a turn is still running. The
 *  design gives no wording for these; they need a ruling. */
export const RAIL_END_CHAT = 'End chat';
export const RAIL_END_CONFIRM = 'End this chat? A turn is still running and will be stopped.';
export const RAIL_END_KEEP = 'Keep chat';
