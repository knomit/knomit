// The chat transcript as the page keeps it: a reducer over the server's events
// (internal/edge/web/api/chat/chat.go), and the pure wording helpers.
//
// A TURN is one user message and everything the assistant did for it. Its
// parts are in the order they happened: text, and GROUPS of consecutive tool
// calls (the page draws each group under one "Used Knomit" header).
//
// THE SNAPSHOT IS AUTHORITATIVE. GET /chats/{id} carries every turn whole: its
// parts, summed usage and cost, price warning, and how it ended. `ready` (the
// first event of every connect) and every turn end make the page read it, and
// the reducer replaces every turn it lists, verbatim; nothing is patched on.
// Live events only fill in what happens after the snapshot was cut.
//
// Live events are routed by TURN INDEX. The server puts the view index of a
// turn's user item (`turn`) on every event of the turn and on the 202 that
// answers a send, and every snapshot turn knows its own (`at`). An event goes to
// the turn with its index; nothing is guessed from order.
//
// The one thing the page holds beyond a snapshot is the turn the person just
// sent (`local`). Its index is not known until the 202 (or the first event of
// the turn, whichever arrives first): an event for an index the page has no
// turn for belongs to that local turn when one exists (it is the server's next
// turn), and is how the page learns the index. With no local turn it is a turn
// the page does not know (started elsewhere): the page asks for a snapshot
// (`staleSeq`). The server appends a turn to its view BEFORE it answers the
// POST, so a snapshot started after the POST was answered 2xx lists it; until
// then the local turn is kept after the snapshot's turns. A POST that fails
// takes its local turn out outright.
import { durationMinutes, durationMs, durationSeconds } from './chatCopy';
import type { ChatApproval, ChatCallSummary, ChatNotice, ChatSnapshot, ChatViewItem } from './chatApi';

/** The loader tool's name (llm.LoadToolsName): its calls get their own row. */
export const LOADER_TOOL = 'load_tools';

export interface ToolCall {
  id: string; name: string; state: 'running' | 'done'; isError: boolean;
  /** The call's arguments: in the snapshot always, and on a live `running` event. */
  args?: unknown;
  /** How long the tool ran, in ms, and its result in one number: on a finished
   *  call (the done event and the snapshot); absent for a call that did not run. */
  durationMs?: number;
  summary?: ChatCallSummary;
  /** When the call began, and once it is done when it ended: Unix ms, the server's clock. */
  startedMs?: number;
  endedMs?: number;
  /** The tools a call to the loader loaded (only on that call). */
  loaded?: string[];
}
export type ChatPart = { kind: 'text'; text: string } | { kind: 'group'; calls: ToolCall[] };
export interface TurnUsage { input: number; cacheRead: number; cacheWrite: number; output: number }
export interface ChatTurn {
  /** The person's message; empty for a turn the page joined mid-way. */
  user: string;
  /** The view index of this turn's user item on the server, which live events
   *  name; unknown (undefined) for a local turn until its 202 or first event. */
  at?: number;
  parts: ChatPart[];
  /** Summed over the turn's requests; null until the first usage event. */
  usage: TurnUsage | null;
  /** Estimated USD; null when no request of the turn was priced (no price card). */
  costUsd: number | null;
  /** The loop's message when the turn ended in an error (a refusal, a limit). */
  error: string | null;
  /** The server's price-tier warning for this turn, verbatim. */
  warning: string | null;
  /** The same warning's numbers, for the page's own line. */
  warningInfo: ChatNotice | null;
  /** The numbers behind `error` when it is a context-full end. */
  errorInfo: ChatNotice | null;
  /** The person stopped this turn (the server marks it cancelled). */
  stopped: boolean;
  /** The reasoning summary streamed so far, shown under the placeholder until
   *  the first tool call or text arrives. Not in the snapshot. */
  thinking: string;
  /** When the server accepted the send (ms, the page's clock), for the wait
   *  counter. Unknown (undefined) for a turn the page joined mid-way. */
  acceptedAt?: number;
  /** The person sent this message from here and no snapshot started after the
   *  server accepted it has listed it yet. `accepted` is true once the server
   *  is known to have the turn (the POST answered 2xx, or an event named it). */
  local?: { sendId: number; accepted: boolean };
}

export interface ChatState {
  turns: ChatTurn[];
  running: boolean;
  /** Tool calls waiting for the person, by call id. */
  approvals: Record<string, ChatApproval>;
  ended: boolean;
  /** The highest usage `seq` the page has counted (or the snapshot includes):
   *  the server numbers a chat's requests, so an event at or below it is a
   *  repeat of one already counted. */
  lastUsageSeq: number;
  /** Counts the times a live event named a turn the page has no turn for and
   *  no local turn could take: the page reads a snapshot for each. */
  staleSeq: number;
  /** A snapshot has been applied. */
  loaded: boolean;
  /** The size in tokens of the latest request's prompt (input + cache read +
   *  cache write): what the context meter shows as used. */
  ctxTokens: number;
  /** The slowest first-reply time the server has seen for this chat's model in
   *  its run, in ms; 0 when none has been observed (the page then never says
   *  "No response yet"). */
  expectedFirstTokenMs: number;
}

/** The `usage` event's payload, as the server names the fields. */
export interface UsagePayload { input: number; cache_read: number; cache_write: number; output: number }

export type ChatEvent =
  /** A snapshot arrived. `confirmed` lists the sends whose POST had answered
   *  2xx before the read started: the snapshot lists those turns. */
  | { kind: 'ready'; snapshot: ChatSnapshot; confirmed?: number[] }
  /** The person sent a message (the server's events do not echo it). */
  | { kind: 'user'; text: string; sendId: number }
  /** The POST answered 2xx: the server has the turn, at view index `turn`. */
  | { kind: 'send_accepted'; sendId: number; turn: number; at: number }
  /** The POST failed (non-2xx, a network error, no answer in time): the turn
   *  never started, so it is taken back out whatever it holds. */
  | { kind: 'send_failed'; sendId: number }
  | { kind: 'text_delta'; text: string; turn: number }
  | { kind: 'thinking_delta'; text: string; turn: number }
  | { kind: 'tool_call'; id: string; name: string; state: 'running' | 'done'; is_error?: boolean; arguments?: unknown; duration_ms?: number; summary?: ChatCallSummary; loaded?: string[]; started_ms?: number; ended_ms?: number; turn: number }
  | { kind: 'approval'; approval: ChatApproval }
  | { kind: 'approval_answered'; callId: string }
  | { kind: 'usage'; usage: UsagePayload; cost_usd?: number; seq?: number; turn: number }
  | { kind: 'warning'; message: string; notice?: ChatNotice; turn: number }
  | { kind: 'turn_done'; turn: number }
  /** The person pressed Stop: the turn is over here whatever the stream says. */
  | { kind: 'stopped' }
  | { kind: 'turn_error'; message: string; cancelled?: boolean; error_info?: ChatNotice; turn: number }
  | { kind: 'chat_end' };

export const initialChatState: ChatState = { turns: [], running: false, approvals: {}, ended: false, lastUsageSeq: 0, staleSeq: 0, loaded: false, ctxTokens: 0, expectedFirstTokenMs: 0 };

const newTurn = (user = ''): ChatTurn => ({ user, parts: [], usage: null, costUsd: null, error: null, warning: null, warningInfo: null, errorInfo: null, stopped: false, thinking: '' });

/** route finds the turn a live event names. A turn index the page has no turn
 *  for is the local unconfirmed turn's (the server's next turn), which learns
 *  its index from it; with no local turn it is stale: the page asks for a
 *  snapshot. */
function route(s: ChatState, turn: number): { state: ChatState; index: number } {
  const known = s.turns.findIndex(t => t.at === turn);
  if (known >= 0) return { state: s, index: known };
  const local = s.turns.findIndex(t => t.local && !t.local.accepted && t.at === undefined);
  const beyond = s.turns.every(t => t.at === undefined || t.at < turn);
  if (local >= 0 && beyond) {
    const turns = s.turns.slice();
    turns[local] = { ...turns[local], at: turn, acceptedAt: turns[local].acceptedAt ?? Date.now(), local: { sendId: turns[local].local!.sendId, accepted: true } };
    return { state: { ...s, turns }, index: local };
  }
  return { state: { ...s, staleSeq: s.staleSeq + 1 }, index: -1 };
}

/** onTurn applies `edit` to the turn a live event names (see route). */
function onTurn(s: ChatState, turn: number, edit: (t: ChatTurn) => ChatTurn): ChatState {
  const r = route(s, turn);
  if (r.index < 0) return r.state;
  const turns = r.state.turns.slice();
  turns[r.index] = edit(turns[r.index]);
  return { ...r.state, turns };
}

/** Whether the turn at `index` is the latest one: only its end ends the chat's
 *  run (a late end from an older turn does not). */
const isLatest = (s: ChatState, index: number): boolean => index === s.turns.length - 1;

function addItem(turns: ChatTurn[], item: ChatViewItem, at: number): void {
  if (item.kind === 'user') {
    const t = newTurn(item.text ?? '');
    t.at = at;
    if (item.usage) t.usage = { input: item.usage.input, cacheRead: item.usage.cache_read, cacheWrite: item.usage.cache_write, output: item.usage.output };
    t.costUsd = item.cost_usd ?? null;
    t.warning = item.warning || null;
    t.warningInfo = item.warning_info ?? null;
    // A cancelled turn is the person's own Stop: no error, the page shows it as stopped.
    t.error = item.error && !item.cancelled ? item.error : null;
    t.errorInfo = t.error ? item.error_info ?? null : null;
    t.stopped = !!item.cancelled;
    turns.push(t);
    return;
  }
  if (!turns.length) turns.push(newTurn());
  const t = turns[turns.length - 1];
  if (item.kind === 'assistant') { t.parts.push({ kind: 'text', text: item.text ?? '' }); return; }
  const c: ToolCall = { id: item.id ?? '', name: item.name ?? '', state: item.state ?? 'done', isError: !!item.is_error, ...(item.arguments !== undefined ? { args: item.arguments } : {}),
    ...(item.duration_ms ? { durationMs: item.duration_ms } : {}), ...(item.summary ? { summary: item.summary } : {}), ...(item.loaded?.length ? { loaded: item.loaded } : {}),
    ...(item.started_ms ? { startedMs: item.started_ms } : {}), ...(item.ended_ms ? { endedMs: item.ended_ms } : {}) };
  const last = t.parts[t.parts.length - 1];
  if (last?.kind === 'group') last.calls.push(c); else t.parts.push({ kind: 'group', calls: [c] });
}

/** snapshotState is the state GET /chats/{id} describes, verbatim. Only the
 *  turns the person sent from here that the snapshot cannot list yet (their
 *  POST had not answered when the read started) are kept after it. */
export function snapshotState(snap: ChatSnapshot, prev: ChatState = initialChatState, confirmed: number[] = []): ChatState {
  const turns: ChatTurn[] = [];
  snap.items.forEach((it, at) => addItem(turns, it, at));
  // What only the page knows about a turn it already had: when it was accepted
  // (the wait counter) and the reasoning summary streamed so far.
  turns.forEach((t, i) => {
    const old = prev.turns.find(p => p.at !== undefined && p.at === t.at);
    if (!old) return;
    if (old.acceptedAt !== undefined) turns[i] = { ...turns[i], acceptedAt: old.acceptedAt };
    if (old.thinking && turns[i].parts.length === 0) turns[i] = { ...turns[i], thinking: old.thinking };
  });
  for (let i = turns.length; i < prev.turns.length; i++) {
    const t = prev.turns[i];
    if (t.local && !confirmed.includes(t.local.sendId)) turns.push(t);
  }
  const approvals: Record<string, ChatApproval> = {};
  for (const a of snap.pending_approvals ?? []) approvals[a.call_id] = a;
  // The snapshot's `prompt_tokens` is the last request's prompt, exactly. A live
  // usage event the snapshot does not include yet (its seq is past the
  // snapshot's usage_seq) is newer, so it is kept.
  const keepLive = prev.ctxTokens > 0 && prev.lastUsageSeq > (snap.usage_seq ?? 0);
  return {
    turns, running: snap.running, approvals, ended: false,
    lastUsageSeq: snap.usage_seq ?? prev.lastUsageSeq, staleSeq: prev.staleSeq, loaded: true,
    ctxTokens: keepLive ? prev.ctxTokens : (snap.prompt_tokens ?? 0),
    expectedFirstTokenMs: snap.expected_first_token_ms ?? prev.expectedFirstTokenMs,
  };
}

export function chatReducer(s: ChatState, e: ChatEvent): ChatState {
  switch (e.kind) {
    case 'ready':
      return snapshotState(e.snapshot, s, e.confirmed);
    case 'user':
      return { ...s, turns: [...s.turns, { ...newTurn(e.text), local: { sendId: e.sendId, accepted: false } }], running: true };
    case 'send_accepted': {
      // The 202 names the turn's index. An early event may have told the page
      // already; then it is known and nothing changes.
      const turns = s.turns.map(t => t.local?.sendId === e.sendId
        ? { ...t, at: t.at ?? e.turn, acceptedAt: t.acceptedAt ?? e.at, local: { sendId: e.sendId, accepted: true } }
        : t);
      return { ...s, turns };
    }
    case 'send_failed': {
      // Out outright, whatever it holds: nothing is routed to a turn the
      // server never had.
      const turns = s.turns.filter(t => t.local?.sendId !== e.sendId);
      return turns.length === s.turns.length ? s : { ...s, turns, running: false };
    }
    case 'thinking_delta':
      return onTurn(s, e.turn, t => (t.parts.length ? t : { ...t, thinking: t.thinking + e.text }));
    case 'text_delta':
      return onTurn(s, e.turn, t => {
        const last = t.parts[t.parts.length - 1];
        const parts = last?.kind === 'text'
          ? [...t.parts.slice(0, -1), { kind: 'text' as const, text: last.text + e.text }]
          : [...t.parts, { kind: 'text' as const, text: e.text }];
        return { ...t, parts };
      });
    case 'tool_call':
      return onTurn(s, e.turn, t => {
        const existing = t.parts.some(p => p.kind === 'group' && p.calls.some(c => c.id === e.id));
        if (existing) {
          // A call only moves forward: a repeat of `running` after `done` (a
          // duplicate or a late frame) leaves it done.
          return {
            ...t,
            parts: t.parts.map(p => p.kind === 'group'
              ? {
                ...p,
                calls: p.calls.map(c => c.id !== e.id || c.state === 'done' ? c : {
                  ...c, state: e.state, isError: !!e.is_error,
                  ...(e.arguments !== undefined && c.args === undefined ? { args: e.arguments } : {}),
                  ...(e.duration_ms ? { durationMs: e.duration_ms } : {}), ...(e.summary ? { summary: e.summary } : {}),
                  ...(e.started_ms ? { startedMs: e.started_ms } : {}), ...(e.ended_ms ? { endedMs: e.ended_ms } : {}),
                  ...(e.loaded?.length ? { loaded: e.loaded } : {}),
                }),
              }
              : p),
          };
        }
        const c: ToolCall = {
          id: e.id, name: e.name, state: e.state, isError: !!e.is_error, ...(e.arguments !== undefined ? { args: e.arguments } : {}),
          ...(e.duration_ms ? { durationMs: e.duration_ms } : {}), ...(e.summary ? { summary: e.summary } : {}), ...(e.loaded?.length ? { loaded: e.loaded } : {}),
          ...(e.started_ms ? { startedMs: e.started_ms } : {}), ...(e.ended_ms ? { endedMs: e.ended_ms } : {}),
        };
        const last = t.parts[t.parts.length - 1];
        const parts = last?.kind === 'group'
          ? [...t.parts.slice(0, -1), { kind: 'group' as const, calls: [...last.calls, c] }]
          : [...t.parts, { kind: 'group' as const, calls: [c] }];
        return { ...t, parts };
      });
    case 'approval': {
      // The card shows in its call's row, so the turn must be one the page has
      // (route learns a local turn's index, or asks for a snapshot).
      const r = route(s, e.approval.turn);
      return { ...r.state, approvals: { ...r.state.approvals, [e.approval.call_id]: e.approval } };
    }
    case 'approval_answered': {
      const { [e.callId]: _gone, ...rest } = s.approvals;
      void _gone;
      return { ...s, approvals: rest };
    }
    case 'usage': {
      if (e.seq !== undefined && e.seq <= s.lastUsageSeq) return s;
      return onTurn({ ...s, lastUsageSeq: e.seq ?? s.lastUsageSeq, ctxTokens: e.usage.input + e.usage.cache_read + e.usage.cache_write }, e.turn, t => {
        const u = t.usage ?? { input: 0, cacheRead: 0, cacheWrite: 0, output: 0 };
        return {
          ...t,
          usage: {
            input: u.input + e.usage.input, cacheRead: u.cacheRead + e.usage.cache_read,
            cacheWrite: u.cacheWrite + e.usage.cache_write, output: u.output + e.usage.output,
          },
          costUsd: e.cost_usd === undefined ? t.costUsd : (t.costUsd ?? 0) + e.cost_usd,
        };
      });
    }
    case 'warning':
      return onTurn(s, e.turn, t => ({ ...t, warning: e.message, warningInfo: e.notice ?? t.warningInfo }));
    case 'stopped': {
      // The person pressed Stop: the running (latest) turn is over here, and is
      // drawn as stopped until the snapshot that follows says how it really ended.
      const turns = s.running && s.turns.length
        ? s.turns.map((t, i) => (i === s.turns.length - 1 ? { ...t, stopped: true } : t))
        : s.turns;
      return { ...s, turns, running: false, approvals: {} };
    }
    case 'turn_done':
    case 'turn_error': {
      const r = route(s, e.turn);
      let st = r.state;
      // A turn_error names its turn's failure; a cancelled one is the person's
      // own Stop and ends quietly.
      if (e.kind === 'turn_error' && r.index >= 0) {
        const turns = st.turns.slice();
        turns[r.index] = e.cancelled
          ? { ...turns[r.index], stopped: true }
          : { ...turns[r.index], error: e.message, errorInfo: e.error_info ?? null };
        st = { ...st, turns };
      }
      // Only the latest turn's end ends the run: a late end from an older
      // turn (the one just stopped) must not stop the one that follows.
      return r.index >= 0 && isLatest(st, r.index) ? { ...st, running: false, approvals: {} } : st;
    }
    case 'chat_end':
      return { ...s, running: false, ended: true };
  }
}

/** toolRowLabel words a tool call's row: knomit_repos -> "knomit: Knomit repos". */
export function toolRowLabel(tool: string): string {
  const rest = tool.startsWith('knomit_') ? tool.slice('knomit_'.length) : '';
  return rest ? `knomit: Knomit ${rest.replaceAll('_', ' ')}` : tool;
}

/** A knomit_materialize document puts `<a id="f-..."></a>` on its own line
 *  before each fact so a document can link to it. The chat panel renders
 *  markdown without raw HTML, so the tag would show as literal text; the panel
 *  has no in-page links to point at it, so the line is dropped. */
const ANCHOR_LINE = /^[ \t]*<a\s+(?:id|name)="[^"]*"\s*><\/a>[ \t]*\r?$/;
const FENCE_LINE = /^ {0,3}(```+|~~~+)/;

/** stripAnchorLines drops anchor lines, except inside a fenced code block,
 *  where the tag is the fact's own content (an example of markup). */
function stripAnchorLines(md: string): string {
  let fence: string | null = null;
  const out: string[] = [];
  for (const line of md.split('\n')) {
    const m = FENCE_LINE.exec(line);
    if (m) {
      const mark = m[1][0];
      if (fence === null) fence = mark;
      else if (fence === mark) fence = null;
    } else if (fence === null && ANCHOR_LINE.test(line)) {
      continue;
    }
    out.push(line);
  }
  return out.join('\n');
}

/** The markdown a knomit_materialize result stands for: its document `text`
 *  (audience "agent", the real result shape: a JSON object whose `text` is the
 *  document, beside metadata such as root, anchor, total and cursor), or its
 *  facts (audience "human") as a title and a body each. Anchor lines are
 *  dropped. Anything else is shown as it came. */
export function materializedMarkdown(result: string): string {
  try {
    const v = JSON.parse(result) as { text?: unknown; facts?: unknown };
    if (typeof v.text === 'string') return stripAnchorLines(v.text);
    if (Array.isArray(v.facts)) {
      return v.facts.map((f: { title?: string; body?: string }) =>
        `${f.title ? `# ${f.title}\n\n` : ''}${stripAnchorLines(f.body ?? '')}`).join('\n\n---\n\n');
    }
  } catch { /* not JSON: the text itself */ }
  return result;
}

/** formatCost words an estimated cost in USD. */
export function formatCost(usd: number): string {
  if (usd > 0 && usd < 0.0001) return '<$0.0001';
  return usd >= 1 ? `$${usd.toFixed(2)}` : `$${usd.toFixed(4)}`;
}

/** usageLine is a turn's usage: the four counts, then the estimated cost, or
 *  "cost unknown" when the model has no price card. */
export function usageLine(u: TurnUsage, costUsd: number | null): string {
  return [
    `input ${u.input}`, `cache read ${u.cacheRead}`, `cache write ${u.cacheWrite}`, `output ${u.output}`,
    costUsd === null ? 'cost unknown' : formatCost(costUsd),
  ].join(' · ');
}

/** promptTokens is the size of a request's prompt: uncached input plus the
 *  cached parts. */
export const promptTokens = (u: TurnUsage): number => u.input + u.cacheRead + u.cacheWrite;

/** cachedPercent is the share of a turn's prompt served from cache:
 *  cache_read / (input + cache_read + cache_write), rounded; 0 for no prompt. */
export function cachedPercent(u: TurnUsage): number {
  const total = promptTokens(u);
  return total === 0 ? 0 : Math.round((u.cacheRead / total) * 100);
}

/** compactCount words a token count compactly: 950, 2.4K, 68K, 1.1M. */
export function compactCount(n: number): string {
  const f = (v: number, unit: string) => `${v >= 100 ? Math.round(v) : Math.round(v * 10) / 10}${unit}`;
  if (n >= 1_000_000) return f(n / 1_000_000, 'M');
  if (n >= 1_000) return f(n / 1_000, 'K');
  return String(n);
}

/** The cost mark of a model, from its input price per MTok: `$` under $1, `$$`
 *  under $3, otherwise `$$$`. */
export function costTier(inputPerMTok: number): 1 | 2 | 3 {
  return inputPerMTok < 1 ? 1 : inputPerMTok < 3 ? 2 : 3;
}

/** sessionCost sums the turns' estimated costs; null when no turn was priced. */
export function sessionCost(turns: ChatTurn[]): number | null {
  const priced = turns.filter(t => t.costUsd !== null);
  return priced.length ? priced.reduce((a, t) => a + (t.costUsd ?? 0), 0) : null;
}

const argOf = (args: unknown, keys: string[]): string => {
  if (!args || typeof args !== 'object') return '';
  for (const k of keys) {
    const v = (args as Record<string, unknown>)[k];
    if (typeof v === 'string' && v) return v;
  }
  return '';
};

/** The tools the activity line has wording for. */
export type ToolKind = 'bind' | 'query' | 'read';
export function toolKind(name: string): ToolKind | null {
  switch (name) {
    case 'knomit_bind': return 'bind';
    case 'knomit_query': return 'query';
    case 'knomit_materialize': case 'knomit_explain': return 'read';
    default: return null;
  }
}

/** bindTarget is the repo or lens a knomit_bind call names. */
export const bindTarget = (c: ToolCall): string => argOf(c.args, ['lens', 'repo', 'name', 'target']);

/** The target a call acts on: what its own arguments name, else the target of
 *  the binding in force (`bound`). Empty when neither is known. */
export const callTarget = (c: ToolCall, bound: string): string =>
  (toolKind(c.name) === 'bind' ? bindTarget(c) : argOf(c.args, ['lens', 'repo'])) || bound;

/** The query text of a knomit_query call. */
export const queryText = (c: ToolCall): string => argOf(c.args, ['text']);

/** keyArg is the one argument a timeline row shows in mono: the query text,
 *  the binding's target, or what a read names. Empty when the call has none
 *  (a live call whose arguments have not arrived). */
export function keyArg(c: ToolCall): string {
  switch (toolKind(c.name)) {
    case 'bind': return bindTarget(c);
    case 'query': return queryText(c);
    case 'read': return argOf(c.args, ['file', 'path', 'handle', 'root']);
    default: return '';
  }
}

/** The target in force after `calls`: the last bind's, starting from `bound`. */
export function boundAfter(calls: ToolCall[], bound: string): string {
  let b = bound;
  for (const c of calls) if (toolKind(c.name) === 'bind') b = bindTarget(c) || b;
  return b;
}

/** The target the chat was bound to when call `callId` ran: the last bind
 *  before it, across every turn. Empty when none, or the call is unknown. */
export function boundBeforeCall(turns: ChatTurn[], callId: string): string {
  let bound = '';
  for (const t of turns) for (const part of t.parts) {
    if (part.kind !== 'group') continue;
    for (const c of part.calls) {
      if (c.id === callId) return bound;
      bound = boundAfter([c], bound);
    }
  }
  return '';
}

/** A duration in ms, worded compactly: 840ms, 1.9s, 22s, 1m 12s. */
export function formatDuration(ms: number): string {
  if (ms < 1000) return durationMs(Math.max(1, Math.round(ms)));
  const s = ms / 1000;
  if (s < 10) return durationSeconds(Math.round(s * 10) / 10);
  if (s < 60) return durationSeconds(Math.round(s));
  const m = Math.floor(s / 60);
  return durationMinutes(m, Math.round(s - m * 60));
}

/** The WALL time of a group of calls: the first start to the last end, on the
 *  server's clock. It is not the sum of the tools' own durations: calls that
 *  overlap would count twice, and the gaps where the model thought between them
 *  would not count at all. 0 while any call is still running or has no
 *  timestamps (a server that sends none), so the line then shows no time. */
export function wallDurationMs(calls: ToolCall[]): number {
  if (!calls.length || calls.some(c => !c.startedMs || !c.endedMs)) return 0;
  return Math.max(...calls.map(c => c.endedMs!)) - Math.min(...calls.map(c => c.startedMs!));
}

/** A token count to three significant digits, for a notice: 95K, 100K, 1.02M. */
export function noticeCount(n: number): string {
  const f = (v: number, unit: string) => `${parseFloat(v.toPrecision(3))}${unit}`;
  if (n >= 1_000_000) return f(n / 1_000_000, 'M');
  if (n >= 1_000) return f(n / 1_000, 'K');
  return String(n);
}

/** The reason a request was refused, from the error the api layer threw
 *  (`<url> → <status> <detail>`): the detail, which is the server's sentence. */
export function refusalReason(message: string): string {
  const m = /^\S+ → (?:\d{3} )?(.*)$/s.exec(message);
  return (m ? m[1] : message).trim();
}
