import { useSyncExternalStore } from 'react';

/** What the top-bar pill shows about one chat. ChatFacet publishes it (it owns
 *  the transcript and the context window); TopBar, which sits outside it, reads
 *  it. */
export interface ChatPillData {
  /** Summed turn costs; null when no turn was priced. */
  cost: number | null;
  /** Tokens the latest request's prompt used. */
  used: number;
  /** The model's context window in tokens; 0 until GET /chats/{id}/context answers. */
  window: number;
}

const data = new Map<string, ChatPillData>();
const listeners = new Set<() => void>();

/** publishChatPill records what the pill shows for a chat. A no-change publish
 *  is dropped so a render does not notify. */
export function publishChatPill(chatId: string, next: ChatPillData): void {
  const cur = data.get(chatId);
  if (cur && cur.cost === next.cost && cur.used === next.used && cur.window === next.window) return;
  data.set(chatId, next);
  listeners.forEach(l => l());
}

/** forgetChatPill drops a chat's entry (the chat is over). */
export function forgetChatPill(chatId: string): void {
  if (data.delete(chatId)) listeners.forEach(l => l());
}

const subscribe = (l: () => void) => { listeners.add(l); return () => { listeners.delete(l); }; };

/** useChatPill reads what the pill shows for a chat; undefined until published. */
export function useChatPill(chatId: string): ChatPillData | undefined {
  return useSyncExternalStore(subscribe, () => data.get(chatId));
}

