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
import type { ChatApproval, ChatSnapshot, ChatViewItem } from './chatApi';

export interface ToolCall { id: string; name: string; state: 'running' | 'done'; isError: boolean }
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
  | { kind: 'send_accepted'; sendId: number; turn: number }
  /** The POST failed (non-2xx, a network error, no answer in time): the turn
   *  never started, so it is taken back out whatever it holds. */
  | { kind: 'send_failed'; sendId: number }
  | { kind: 'text_delta'; text: string; turn: number }
  | { kind: 'tool_call'; id: string; name: string; state: 'running' | 'done'; is_error?: boolean; turn: number }
  | { kind: 'approval'; approval: ChatApproval }
  | { kind: 'approval_answered'; callId: string }
  | { kind: 'usage'; usage: UsagePayload; cost_usd?: number; seq?: number; turn: number }
  | { kind: 'warning'; message: string; turn: number }
  | { kind: 'turn_done'; turn: number }
  /** The person pressed Stop: the turn is over here whatever the stream says. */
  | { kind: 'stopped' }
  | { kind: 'turn_error'; message: string; cancelled?: boolean; turn: number }
  | { kind: 'chat_end' };

export const initialChatState: ChatState = { turns: [], running: false, approvals: {}, ended: false, lastUsageSeq: 0, staleSeq: 0, loaded: false };

const newTurn = (user = ''): ChatTurn => ({ user, parts: [], usage: null, costUsd: null, error: null, warning: null });

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
    turns[local] = { ...turns[local], at: turn, local: { sendId: turns[local].local!.sendId, accepted: true } };
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
    // A cancelled turn is the person's own Stop: it ends quietly.
    t.error = item.error && !item.cancelled ? item.error : null;
    turns.push(t);
    return;
  }
  if (!turns.length) turns.push(newTurn());
  const t = turns[turns.length - 1];
  if (item.kind === 'assistant') { t.parts.push({ kind: 'text', text: item.text ?? '' }); return; }
  const c: ToolCall = { id: item.id ?? '', name: item.name ?? '', state: item.state ?? 'done', isError: !!item.is_error };
  const last = t.parts[t.parts.length - 1];
  if (last?.kind === 'group') last.calls.push(c); else t.parts.push({ kind: 'group', calls: [c] });
}

/** snapshotState is the state GET /chats/{id} describes, verbatim. Only the
 *  turns the person sent from here that the snapshot cannot list yet (their
 *  POST had not answered when the read started) are kept after it. */
export function snapshotState(snap: ChatSnapshot, prev: ChatState = initialChatState, confirmed: number[] = []): ChatState {
  const turns: ChatTurn[] = [];
  snap.items.forEach((it, at) => addItem(turns, it, at));
  for (let i = turns.length; i < prev.turns.length; i++) {
    const t = prev.turns[i];
    if (t.local && !confirmed.includes(t.local.sendId)) turns.push(t);
  }
  const approvals: Record<string, ChatApproval> = {};
  for (const a of snap.pending_approvals ?? []) approvals[a.call_id] = a;
  return {
    turns, running: snap.running, approvals, ended: false,
    lastUsageSeq: snap.usage_seq ?? prev.lastUsageSeq, staleSeq: prev.staleSeq, loaded: true,
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
        ? { ...t, at: t.at ?? e.turn, local: { sendId: e.sendId, accepted: true } }
        : t);
      return { ...s, turns };
    }
    case 'send_failed': {
      // Out outright, whatever it holds: nothing is routed to a turn the
      // server never had.
      const turns = s.turns.filter(t => t.local?.sendId !== e.sendId);
      return turns.length === s.turns.length ? s : { ...s, turns, running: false };
    }
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
                calls: p.calls.map(c => c.id !== e.id || c.state === 'done' ? c : { ...c, state: e.state, isError: !!e.is_error }),
              }
              : p),
          };
        }
        const c: ToolCall = { id: e.id, name: e.name, state: e.state, isError: !!e.is_error };
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
      return onTurn({ ...s, lastUsageSeq: e.seq ?? s.lastUsageSeq }, e.turn, t => {
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
      return onTurn(s, e.turn, t => ({ ...t, warning: e.message }));
    case 'stopped':
      return { ...s, running: false, approvals: {} };
    case 'turn_done':
    case 'turn_error': {
      const r = route(s, e.turn);
      let st = r.state;
      // A turn_error names its turn's failure; a cancelled one is the person's
      // own Stop and ends quietly.
      if (e.kind === 'turn_error' && !e.cancelled && r.index >= 0) {
        const turns = st.turns.slice();
        turns[r.index] = { ...turns[r.index], error: e.message };
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
