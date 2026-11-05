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
// The one thing the page holds beyond a snapshot is the turn the person just
// sent (`local`). The server appends a turn to its view BEFORE it answers the
// POST, so a snapshot started after the POST was answered 2xx lists it; until
// then (and for a read that started before) the local turn is kept after the
// snapshot's turns. A POST that fails takes its local turn out outright. While
// a local turn is unconfirmed (`accepted` false) live events are not applied to
// it: a late event from the turn before belongs to that one.
import type { ChatApproval, ChatSnapshot, ChatViewItem } from './chatApi';

export interface ToolCall { id: string; name: string; state: 'running' | 'done'; isError: boolean }
export type ChatPart = { kind: 'text'; text: string } | { kind: 'group'; calls: ToolCall[] };
export interface TurnUsage { input: number; cacheRead: number; cacheWrite: number; output: number }

export interface ChatTurn {
  /** The person's message; empty for a turn the page joined mid-way. */
  user: string;
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
   *  server accepted it has listed it yet. `accepted` is true once the POST
   *  answered 2xx. */
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
  /** The POST answered 2xx: the server has the turn. */
  | { kind: 'send_accepted'; sendId: number }
  /** The POST failed (non-2xx, a network error, no answer in time): the turn
   *  never started, so it is taken back out whatever it holds. */
  | { kind: 'send_failed'; sendId: number }
  | { kind: 'text_delta'; text: string }
  | { kind: 'tool_call'; id: string; name: string; state: 'running' | 'done'; is_error?: boolean }
  | { kind: 'approval'; approval: ChatApproval }
  | { kind: 'approval_answered'; callId: string }
  | { kind: 'usage'; usage: UsagePayload; cost_usd?: number; seq?: number }
  | { kind: 'warning'; message: string }
  | { kind: 'turn_done' }
  /** The person pressed Stop: the turn is over here whatever the stream says. */
  | { kind: 'stopped' }
  | { kind: 'turn_error'; message: string; cancelled?: boolean }
  | { kind: 'chat_end' };

export const initialChatState: ChatState = { turns: [], running: false, approvals: {}, ended: false, lastUsageSeq: 0, loaded: false };

const newTurn = (user = ''): ChatTurn => ({ user, parts: [], usage: null, costUsd: null, error: null, warning: null });

const unconfirmed = (t: ChatTurn): boolean => !!t.local && !t.local.accepted;

/** A live event's turn: the last turn that is not an unconfirmed local one (a
 *  late event from the turn before the send is that turn's), made if there is
 *  none (the page joined a turn already running). With only an unconfirmed
 *  local turn there is nothing to attach to and the event is dropped: the
 *  snapshot a turn end starts has it. */
function withLastTurn(s: ChatState, edit: (t: ChatTurn) => ChatTurn): ChatState {
  if (!s.turns.length) return { ...s, turns: [edit(newTurn())] };
  let i = s.turns.length - 1;
  while (i >= 0 && unconfirmed(s.turns[i])) i--;
  if (i < 0) return s;
  const turns = s.turns.slice();
  turns[i] = edit(turns[i]);
  return { ...s, turns };
}

function addItem(turns: ChatTurn[], item: ChatViewItem): void {
  if (item.kind === 'user') {
    const t = newTurn(item.text ?? '');
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
  for (const it of snap.items) addItem(turns, it);
  for (let i = turns.length; i < prev.turns.length; i++) {
    const t = prev.turns[i];
    if (t.local && !confirmed.includes(t.local.sendId)) turns.push(t);
  }
  const approvals: Record<string, ChatApproval> = {};
  for (const a of snap.pending_approvals ?? []) approvals[a.call_id] = a;
  return {
    turns, running: snap.running, approvals, ended: false,
    lastUsageSeq: snap.usage_seq ?? prev.lastUsageSeq, loaded: true,
  };
}

export function chatReducer(s: ChatState, e: ChatEvent): ChatState {
  switch (e.kind) {
    case 'ready':
      return snapshotState(e.snapshot, s, e.confirmed);
    case 'user':
      return { ...s, turns: [...s.turns, { ...newTurn(e.text), local: { sendId: e.sendId, accepted: false } }], running: true };
    case 'send_accepted':
      return { ...s, turns: s.turns.map(t => t.local?.sendId === e.sendId ? { ...t, local: { sendId: e.sendId, accepted: true } } : t) };
    case 'send_failed': {
      // Out outright, whatever it holds: nothing live is ever applied to an
      // unconfirmed turn, so there is nothing of the person's to lose.
      const turns = s.turns.filter(t => t.local?.sendId !== e.sendId);
      return turns.length === s.turns.length ? s : { ...s, turns, running: false };
    }
    case 'text_delta':
      return withLastTurn({ ...s, running: true }, t => {
        const last = t.parts[t.parts.length - 1];
        const parts = last?.kind === 'text'
          ? [...t.parts.slice(0, -1), { kind: 'text' as const, text: last.text + e.text }]
          : [...t.parts, { kind: 'text' as const, text: e.text }];
        return { ...t, parts };
      });
    case 'tool_call':
      return withLastTurn({ ...s, running: true }, t => {
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
    case 'approval':
      return { ...s, approvals: { ...s.approvals, [e.approval.call_id]: e.approval } };
    case 'approval_answered': {
      const { [e.callId]: _gone, ...rest } = s.approvals;
      void _gone;
      return { ...s, approvals: rest };
    }
    case 'usage': {
      if (e.seq !== undefined && e.seq <= s.lastUsageSeq) return s;
      return withLastTurn({ ...s, lastUsageSeq: e.seq ?? s.lastUsageSeq }, t => {
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
      return withLastTurn(s, t => ({ ...t, warning: e.message }));
    case 'turn_done':
    case 'stopped':
      return { ...s, running: false, approvals: {} };
    case 'turn_error':
      // A cancelled turn is the person's own Stop, not a failure: it ends
      // quietly with the text it had.
      if (e.cancelled) return { ...s, running: false, approvals: {} };
      return withLastTurn({ ...s, running: false, approvals: {} }, t => ({ ...t, error: e.message }));
    case 'chat_end':
      return { ...s, running: false, ended: true };
  }
}

/** toolRowLabel words a tool call's row: knomit_repos -> "knomit: Knomit repos". */
export function toolRowLabel(tool: string): string {
  const rest = tool.startsWith('knomit_') ? tool.slice('knomit_'.length) : '';
  return rest ? `knomit: Knomit ${rest.replaceAll('_', ' ')}` : tool;
}

/** The markdown a knomit_materialize result stands for: its document `text`
 *  (audience "agent"), or its facts (audience "human") as a title and a body
 *  each. Anything else is shown as it came. */
export function materializedMarkdown(result: string): string {
  try {
    const v = JSON.parse(result) as { text?: unknown; facts?: unknown };
    if (typeof v.text === 'string') return v.text;
    if (Array.isArray(v.facts)) {
      return v.facts.map((f: { title?: string; body?: string }) =>
        `${f.title ? `# ${f.title}\n\n` : ''}${f.body ?? ''}`).join('\n\n---\n\n');
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
