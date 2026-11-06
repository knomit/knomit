import { useState } from 'react';
import type { CSSProperties } from 'react';
import type { ChatInfo } from './chatApi';
import {
  RAIL_STATE_ATTENTION, RAIL_STATE_FAILED, RAIL_STATE_NEW_ANSWER, RAIL_STATE_RUNNING,
} from './chatCopy';

/** What a chat row's mark says. State is the mark's colour and nothing else. */
export type RailState = 'idle' | 'running' | 'attention' | 'new' | 'failed';

/** How many chats the rail shows before "N more". */
export const RAIL_CHATS_SHOWN = 5;

/** The mark's look per state (design section 6). Needs attention keeps the
 *  amber ring (ruling 2026-11-06). */
export const MARK_STYLE: Record<RailState, CSSProperties> = {
  idle: { color: '#b59a5e' },
  running: { color: '#f5d495', background: 'rgba(232,194,122,.14)', boxShadow: '0 0 8px rgba(232,194,122,.35)' },
  attention: { color: '#e8c27a', boxShadow: 'inset 0 0 0 1.5px #e8c27a' },
  new: { color: '#f0f0f0' },
  failed: { color: '#f06a60' },
};

/** The state in words for the tooltip; idle has none. */
export const STATE_WORDS: Record<RailState, string | null> = {
  idle: null, running: RAIL_STATE_RUNNING, attention: RAIL_STATE_ATTENTION, new: RAIL_STATE_NEW_ANSWER, failed: RAIL_STATE_FAILED,
};

/** The turns whose answer is in: the running turn does not count yet. */
export function completedTurns(c: ChatInfo): number {
  const n = c.turns ?? 0;
  return c.state === 'running' || c.state === 'awaiting_approval' ? Math.max(n - 1, 0) : n;
}

/** railState: a call waiting for the person outranks a running turn, which
 *  outranks a failed last turn; an idle chat whose answer the person has not
 *  seen (more completed turns than the last one seen) is "new" unless it is the
 *  chat on screen. `seen` is undefined until the chat has been seen once. */
export function railState(c: ChatInfo, seen: number | undefined, selected: boolean): RailState {
  if (c.state === 'awaiting_approval') return 'attention';
  if (c.state === 'running') return 'running';
  if (c.state === 'error') return 'failed';
  if (!selected && seen !== undefined && completedTurns(c) > seen) return 'new';
  return 'idle';
}

/** recentChats: newest activity first (ties keep list order). */
export function recentChats(chats: ChatInfo[]): ChatInfo[] {
  return chats
    .map((c, i) => ({ c, i }))
    .sort((a, b) => (b.c.last_activity ?? '').localeCompare(a.c.last_activity ?? '') || a.i - b.i)
    .map(x => x.c);
}

/** useSeenTurns remembers, per chat, the last completed turn index seen: a
 *  chat's first sight sets the baseline (a chat listed after a reload is not
 *  "new"), and the chat on screen is always seen. */
export function useSeenTurns(chats: ChatInfo[], shownId: string | null): Record<string, number> {
  const [seen, setSeen] = useState<Record<string, number>>({});
  // Adjusted during render (the documented pattern for state derived from
  // props), so the row and its colour arrive in one paint.
  const changes = chats
    .filter(c => seen[c.id] === undefined || (c.id === shownId && seen[c.id] !== completedTurns(c)))
    .map(c => [c.id, completedTurns(c)] as const);
  const next = changes.length ? { ...seen, ...Object.fromEntries(changes) } : seen;
  if (next !== seen) setSeen(next);
  return next;
}

// A change in what a chat is doing (a turn started or ended, an approval asked
// or answered) asks the list to be read again, so the rail stays live through
// the chat's own events stream.
const listeners = new Set<() => void>();
export function notifyChatActivity(): void { listeners.forEach(l => l()); }
export function onChatActivity(l: () => void): () => void { listeners.add(l); return () => { listeners.delete(l); }; }
