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
// user on 2026-11-06, with the singular forms ("1 step", "Session (1 turn)").
// The per-tool verbs ("Bound to", "searched", "read N facts", "used N tools",
// "Searching") were replaced the same day by the tools' own titles: the user's
// ruling that a tool's wording comes from its MCP metadata, never this file.

/** The activity line's words. ONE function words the whole summary, and it
 *  holds no word for any tool: every call is named by its tool's own title (the
 *  server sends annotations.title; a tool with none is named by its raw name).
 *  Calls are counted per (title, target) in first-appearance order and the
 *  pieces joined with " · ": "Bind demo-kb · Query 2× in knomit". `strong`
 *  marks the parts drawn bold (a bind's target and the counts). Approved by
 *  the user 2026-11-06 (tool titles); "×" and " in " are the connectors. */
export interface SummarySeg { text: string; strong?: boolean }
/** `title`: the call's title, else its tool name. `target`: for a bind, the repo
 *  or lens it names; for any other call, where it ran ("" when not known).
 *  `bind`: the call is a bind, whose target follows its title directly.
 *  `failed`: the call returned an error. A failed bind bound nothing, so it adds
 *  nothing to the line; a failed call of any other kind is counted like any other. */
export interface SummaryCall { title: string; target: string; bind?: boolean; failed?: boolean }

export function activitySummary(calls: SummaryCall[]): SummarySeg[][] {
  // A bind with no readable target is said alone only when no bind named one:
  // "Bind · Bind all" would say the same thing twice.
  const named = calls.some(c => c.bind && !c.failed && c.target);
  const groups: { title: string; target: string; bind: boolean; n: number }[] = [];
  for (const c of calls) {
    if (c.bind && (c.failed || (!c.target && named))) continue;
    const bind = !!c.bind;
    const g = groups.find(x => x.title === c.title && x.target === c.target && x.bind === bind);
    if (g) g.n++;
    else groups.push({ title: c.title, target: c.target, bind, n: 1 });
  }
  return groups.map(g => [
    { text: g.title },
    ...(g.bind && g.target ? [{ text: ' ' }, { text: g.target, strong: true }] : []),
    ...(g.n > 1 ? [{ text: ' ' }, { text: `${g.n}×`, strong: true }] : []),
    ...(!g.bind && g.target ? [{ text: ' in ' + g.target }] : []),
  ]);
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
export const COST_ROW_CACHE_WRITE = 'Cache write';
export const COST_ROW_OUTPUT = 'Output';
export const COST_THIS_TURN = 'This turn';
/** Shown for a cost when the model has no price card. */
export const COST_UNPRICED = '–';
/** The cost card's first line: "Turn N · <model>". */
export const costCardHeader = (turn: number, model: string): string => `Turn ${turn} · ${model}`;
export const costSession = (turns: number): string => `Session (${turns} ${turns === 1 ? 'turn' : 'turns'})`;

/** The top-bar pill and the composer's meter row. */
export const PILL_SESSION = 'session';
export const CONTEXT_LABEL = 'context';
export const COMPOSER_HINT = 'Enter to send';
export const CHAT_REPLY_PLACEHOLDER = 'Reply…';

// ── APPROVED (2026-11-06, PR 2) ──
// Chat UX v2, PR 2 (design sections 2 and 5). Approved by the user on
// 2026-11-06: the design's copy-table strings for Waiting, Stop and Notices,
// the counter "4s", the Esc keycap, "1 result" / "N results", "Not sent:
// <reason> (<status>)", the bare "nothing sent", the duration formats ("840ms",
// "1.9s", "22s", "1m 12s"), and the fixed Continue message.
// The server's own sentences ("Send withdrawn", "the send was stopped before
// the server accepted it") are in the Go handler, verbatim.

/** The fixed follow-up the Continue button sends after a stop. */
export const CHAT_CONTINUE_MESSAGE = 'Continue.';

/** Waiting for the reply: the unaccepted send, the placeholder, and the stall. */
export const CHAT_SENDING = 'Sending…';
export const CHAT_THINKING = 'Thinking…';
export const CHAT_NO_RESPONSE = 'No response yet';
/** The placeholder's seconds counter (the mockup words it "4s"; not in the copy table). */
export const waitCounter = (seconds: number): string => `${seconds}s`;

/** Stop. The keycap is the key's own name; the design draws it, the table does not list it. */
export const CHAT_STOP_KEY = 'Esc';
export const CHAT_STOPPED = 'Stopped by you';
export const CHAT_EDIT_RESEND = 'Edit and resend';
export const CHAT_CONTINUE = 'Continue';

/** Timeline rows: a query's result count (the mockup's "24 results"; the
 *  singular is not in the table). */
export const resultCount = (n: number): string => `${n} ${n === 1 ? 'result' : 'results'}`;

/** Notices. The price-tier line is `<n> of <m>` then the rest; the model is its display name. */
export const noticeOf = (n: string, m: string): string => `${n} of ${m}`;
export const noticeTierNext = (model: string, mult: number): string =>
  `${model}'s higher price tier is next${mult > 0 ? ` (${parseFloat(mult.toFixed(2))}×)` : ''}`;
export const NOTICE_NEW_CHAT = 'New chat';
export const NOTICE_FULL = 'Chat is full';
/** `<needed> > <window>, nothing sent`; with no known size, just `nothing sent`. */
export const noticeFullRest = (needed: string, window: string): string =>
  `${needed && window ? `${needed} > ${window}, ` : ''}nothing sent`;
export const NOT_SENT = 'Not sent';
export const NOT_SENT_RETRY = 'Retry';
/** Existing approved wording (App's boundary label for the new-chat draft), reused when starting a chat from a notice fails. */
export const NEW_CHAT_ERROR = 'New chat hit an error';

/** A tool's run time on a timeline row and in "N steps · T": 840ms, 1.9s, 22s, 1m 12s. */
export const durationMs = (ms: number): string => `${ms}ms`;
export const durationSeconds = (s: number): string => `${s}s`;
export const durationMinutes = (m: number, s: number): string => `${m}m ${s}s`;


// ── APPROVED (2026-11-06) ──
// Chat UX v2, PR 5 (design section 1, the start screen). Approved by the user
// on 2026-11-06: every section 1 string of the design's copy table, "free" on
// Ollama cards, the labels Context, Effort, Cost, Size, Keep alive, "none" and
// "cloud ·", and the formats "$x / MTok", "N tokens" and "N (num_ctx)". The
// strings the design gives are marked "design"; the rest are the mockup's
// wording (P1-new-chat.png).

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
/** Reused from approved PR 1 copy: "Input", "Output" and "Cache read" are
 *  COST_ROW_INPUT, COST_ROW_OUTPUT and COST_ROW_CACHE_READ. From the mockup: */
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

// ── APPROVED 2026-11-06 ──
// Side panel (chat UX v2, PR 3). The first six groups are the design's copy
// table ("Side panel", "Fact links") verbatim; the last three labels are GAPS:
// the design gives the back, forward and close buttons no accessible name, and
// these are the shortest words that name them. Approved by the user with the rest.

/** Header button. Design copy table, "Fact links" row. */
export const PANEL_OPEN_IN_BROWSE = 'Open in Browse';
/** The tool result's two tabs. Design copy table, "Side panel" row. */
export const PANEL_TAB_RESULT = 'Result';
export const PANEL_TAB_ARGUMENTS = 'Arguments';
/** The footer: "Esc close · ⌘[ back · N of M opened". */
export const PANEL_KEY_CLOSE = 'Esc close';
export const PANEL_KEY_BACK = '⌘[ back';
export const panelOpened = (n: number, m: number): string => `${n} of ${m} opened`;
export const panelFooter = (n: number, m: number): string => [PANEL_KEY_CLOSE, PANEL_KEY_BACK, panelOpened(n, m)].join(' · ');
// GAP (not in the design): accessible names for the header's icon buttons.
export const PANEL_BACK_LABEL = 'Back';
export const PANEL_FORWARD_LABEL = 'Forward';
export const PANEL_CLOSE_LABEL = 'Close';

// ── APPROVED (2026-11-06) ──
// Chat UX v2, PR 4 (design section 6, the rail). Approved by the user on
// 2026-11-06: the design's copy-table strings for the rail row, the hover card's
// field labels exactly as in the mockup (R2) with "1 turn" for one, and the
// wording for ending a chat.

/** The expander under the five most recent chats. */
export const railMore = (n: number): string => `${n} more`;
/** The state in words, in a row's tooltip. The design gives no words for idle,
 *  so the tooltip shows none for it. */
export const RAIL_STATE_RUNNING = 'Running';
export const RAIL_STATE_ATTENTION = 'Waiting for your approval';
export const RAIL_STATE_NEW_ANSWER = 'New answer';
export const RAIL_STATE_FAILED = 'Failed';
/** The tooltip's field labels. */
export const RAIL_TIP_MODEL = 'Model';
export const RAIL_TIP_STARTED = 'Started';
export const RAIL_TIP_COST = 'Cost';
export const railTurns = (n: number): string => `${n} ${n === 1 ? 'turn' : 'turns'}`;
/** The hover x, and its confirmation when a turn is still running. */
export const RAIL_END_CHAT = 'End chat';
export const RAIL_END_CONFIRM = 'End this chat? A turn is still running and will be stopped.';
export const RAIL_END_KEEP = 'Keep chat';
