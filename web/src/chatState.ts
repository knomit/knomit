// The chat transcript as the page keeps it: a reducer over the server's events
// (internal/edge/web/api/chat/chat.go), and the pure wording helpers.
//
// A TURN is one user message and everything the assistant did for it. Its
// parts are in the order they happened: text, and GROUPS of consecutive tool
// calls (the page draws each group under one "Used Knomit" header).
//
// `ready` is the first event of every connect. It carries no state of its own:
// the page refetches GET /chats/{id} and hands the snapshot to the reducer,
// which replaces the transcript with the server's view (there is no replay).
// The snapshot has no usage, error or warning, so the reducer carries those
// over from the state it replaces, turn by turn (a turn matches when its user
// message does). The page also refetches when a turn ends, so an end that
// arrives while a snapshot is in flight cannot leave the chat running.
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
}

export interface ChatState {
  turns: ChatTurn[];
  running: boolean;
  /** Tool calls waiting for the person, by call id. */
  approvals: Record<string, ChatApproval>;
  /** The server's price-tier warning, verbatim. */
  warning: string | null;
  ended: boolean;
  /** The `seq` of every usage event already counted: the server numbers a
   *  chat's requests, so a repeat of one is not added twice. */
  seenUsage: Record<number, true>;
  /** A snapshot has been applied. */
  loaded: boolean;
}

/** The `usage` event's payload, as the server names the fields. */
export interface UsagePayload { input: number; cache_read: number; cache_write: number; output: number }

export type ChatEvent =
  | { kind: 'ready'; snapshot: ChatSnapshot }
  /** The person sent a message (the server's events do not echo it). */
  | { kind: 'user'; text: string }
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

export const initialChatState: ChatState = { turns: [], running: false, approvals: {}, warning: null, ended: false, seenUsage: {}, loaded: false };

const newTurn = (user = ''): ChatTurn => ({ user, parts: [], usage: null, costUsd: null, error: null });

/** The last turn, made if there is none (the page joined a turn already
 *  running), and the turns with it. */
function withLastTurn(s: ChatState, edit: (t: ChatTurn) => ChatTurn): ChatState {
  const turns = s.turns.length ? s.turns.slice() : [newTurn()];
  turns[turns.length - 1] = edit(turns[turns.length - 1]);
  return { ...s, turns };
}

function addItem(turns: ChatTurn[], item: ChatViewItem): void {
  if (item.kind === 'user') { turns.push(newTurn(item.text ?? '')); return; }
  if (!turns.length) turns.push(newTurn());
  const t = turns[turns.length - 1];
  if (item.kind === 'assistant') { t.parts.push({ kind: 'text', text: item.text ?? '' }); return; }
  const c: ToolCall = { id: item.id ?? '', name: item.name ?? '', state: item.state ?? 'done', isError: !!item.is_error };
  const last = t.parts[t.parts.length - 1];
  if (last?.kind === 'group') last.calls.push(c); else t.parts.push({ kind: 'group', calls: [c] });
}

/** snapshotState is the state GET /chats/{id} describes. `prev` is the state it
 *  replaces: what only the page knows (usage, a turn's error, the warning) is
 *  carried over onto the turns that are still the same. */
export function snapshotState(snap: ChatSnapshot, prev: ChatState = initialChatState): ChatState {
  const turns: ChatTurn[] = [];
  for (const it of snap.items) addItem(turns, it);
  turns.forEach((t, i) => {
    const old = prev.turns[i];
    if (old && old.user === t.user) { t.usage = old.usage; t.costUsd = old.costUsd; t.error = old.error; }
  });
  const approvals: Record<string, ChatApproval> = {};
  for (const a of snap.pending_approvals ?? []) approvals[a.call_id] = a;
  return { turns, running: snap.running, approvals, warning: prev.warning, ended: false, seenUsage: prev.seenUsage, loaded: true };
}

export function chatReducer(s: ChatState, e: ChatEvent): ChatState {
  switch (e.kind) {
    case 'ready':
      return snapshotState(e.snapshot, s);
    case 'user':
      return { ...s, turns: [...s.turns, newTurn(e.text)], running: true };
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
      if (e.seq !== undefined && s.seenUsage[e.seq]) return s;
      const seen = e.seq === undefined ? s.seenUsage : { ...s.seenUsage, [e.seq]: true as const };
      return withLastTurn({ ...s, seenUsage: seen }, t => {
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
      return { ...s, warning: e.message };
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
