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

// ── PENDING APPROVAL ──
// Chat UX v2, PR 5 (design section 1, the start screen). Awaiting the user's
// approval before it ships. The strings the design gives are marked "design";
// the rest are the mockup's wording (P1-new-chat.png) for what the design names
// but does not word, and need a ruling.

/** design: the heading and the line under it. */
export const START_HEADING = 'Start a chat';
export const START_INTRO = 'Pick a model, then ask. It can read every repo and lens you can; anything that writes asks you first.';
/** design: the composer. `name` is the selected model's display name. */
export const startPlaceholder = (name: string): string => `Ask ${name} about your knowledge…`;
export const START_HINT = 'Enter to start · Shift+Enter for a new line';
export const START_BUTTON = 'Start chat';
/** design: the reachability word beside a provider. */
export const PROVIDER_CONNECTED = 'connected';
export const PROVIDER_RUNNING = 'running';
/** design: the card's tags. "Price tier at 100K": the number is the model's own first tier. */
export const TAG_DEFAULT = 'Default';
export const TAG_TOOLS = 'Tools';
export const TAG_OLLAMA = 'Ollama';
export const TAG_NO_TOOLS = 'No tools';
export const tagPriceTier = (at: string): string => `Price tier at ${at}`;
/** design: the cost mark of a model that costs nothing. */
export const COST_FREE = 'free';
/** design, Suggestions. {repo} is a repo the reader has. */
export const SUGGEST_CHANGED = 'What changed in knomit-kb this week?';
export const SUGGEST_BIND = 'Bind to the all lens';
export const suggestReview = (repo: string): string => `Review open items in ${repo}`;
export const suggestContradictions = (repo: string): string => `Find contradictions in ${repo}`;

/** Reused from approved PR 1 copy: "Input", "Output" and "Cache read" are
 *  COST_ROW_INPUT, COST_ROW_OUTPUT and COST_ROW_CACHE_READ. NEW, from the mockup: */
export const STAT_CONTEXT = 'Context';
export const STAT_EFFORT = 'Effort';
export const STAT_COST = 'Cost';
export const STAT_SIZE = 'Size';
export const STAT_KEEP_ALIVE = 'Keep alive';
/** A price: "$0.10 / MTok". */
export const perMTok = (usd: string): string => `${usd} / MTok`;
/** An Ollama model's cost. */
export const COST_NONE = 'none';
/** A context window: "1M tokens", "32K (num_ctx)". */
export const contextTokens = (n: string): string => `${n} tokens`;
export const contextNumCtx = (n: string): string => `${n} (num_ctx)`;
/** The cloud provider's "where it runs" prefix: "cloud · ANTHROPIC_API_KEY". */
export const PROVIDER_CLOUD = 'cloud';
