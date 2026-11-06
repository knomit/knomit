import { useCallback, useEffect, useRef, useState } from 'react';
import { listChats, listChatProviders } from './chatApi';
import type { ChatInfo, ChatProvider } from './chatApi';
import { subscribeChatEvents } from './chatEvents';

/**
 * useChats is GET /chats: the caller's live chats, which is the BACKING LIST of
 * a pinned chat target. The pin-backing-list invariant needs a new chat to be
 * in the list no later than the render that pins it, so `add` is a state
 * update the caller batches with its SELECT_PINNED, never a refetch.
 *
 * An add() or remove() made while a read is in flight survives that read's
 * answer: the answer was cut before it and does not know about it.
 *
 * The list is read again when the window regains focus.
 *
 * `loaded` is false until the first answer (or while `enabled` is false). A
 * caller must not unpin a chat because it is missing from a list that has not
 * loaded.
 */
export function useChats(enabled = true): {
  chats: ChatInfo[]; loaded: boolean; add: (c: ChatInfo) => void; remove: (id: string) => void;
} {
  const [chats, setChats] = useState<ChatInfo[]>([]);
  const [loaded, setLoaded] = useState(false);
  const rev = useRef(0);
  const added = useRef(new Map<string, number>());
  const removed = useRef(new Map<string, number>());
  const reloadRef = useRef<() => void>(() => {});
  // One map per list read still in flight: the rows chat_state updated meanwhile.
  const inFlight = useRef(new Set<Map<string, Partial<ChatInfo>>>());

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    const load = () => {
      const startedAt = rev.current;
      // chat_state events applied while this read is in flight: the read was cut
      // before them, so its rows for those chats are older and must not win.
      const during = new Map<string, Partial<ChatInfo>>();
      inFlight.current.add(during);
      listChats().then(list => {
        inFlight.current.delete(during);
        if (cancelled) return;
        const gone = (id: string) => (removed.current.get(id) ?? -1) > startedAt;
        setChats(prev => {
          const out = list.filter(c => !gone(c.id)).map(c => (during.has(c.id) ? { ...c, ...during.get(c.id) } : c));
          for (const c of prev) {
            if ((added.current.get(c.id) ?? -1) > startedAt && !out.some(o => o.id === c.id)) out.push(c);
          }
          return out;
        });
        setLoaded(true);
      }).catch(() => {
        inFlight.current.delete(during);
        // An unreadable list leaves it as it was. Never loaded, so nothing is
        // unpinned on its account.
      });
    };
    load();
    reloadRef.current = load;
    // Back to this tab: a chat may have ended while it was away (the server
    // ends one nobody is attached to), so the list is read again.
    const onFocus = () => load();
    window.addEventListener('focus', onFocus);
    return () => { cancelled = true; window.removeEventListener('focus', onFocus); };
  }, [enabled]);

  // The rail is live from the page's one events stream: each listed chat's
  // `chat_state` (title, state, turns, cost, last activity) is applied to its
  // row, mounted or not, and a reconnect of the stream reads the list again.
  const ids = chats.map(c => c.id).join('\n');
  useEffect(() => {
    if (!enabled || ids === '') return;
    // The connect-time `ready` is not a change (the list was just read); a
    // later one is a reconnect. `ready` reaches every subscriber, so one of them
    // (the first) answers it, not one per chat.
    let connected = false;
    const offs = ids.split('\n').map((id, i) => subscribeChatEvents(id, (name, data) => {
      if (name === 'ready') {
        if (i !== 0) return;
        if (connected) reloadRef.current(); else connected = true;
        return;
      }
      if (name !== 'chat_state') return;
      const patch: Partial<ChatInfo> = {
        title: String(data.title ?? ''), state: data.state as ChatInfo['state'], turns: Number(data.turns ?? 0),
        last_activity: String(data.last_activity ?? ''), started_at: String(data.started_at ?? ''),
        cost_usd: typeof data.cost_usd === 'number' ? data.cost_usd : undefined,
      };
      // A list read in flight is older than this event: it must not undo it.
      for (const during of inFlight.current) during.set(id, patch);
      setChats(prev => prev.map(c => c.id !== id ? c : { ...c, ...patch }));
    }));
    return () => offs.forEach(off => off());
  }, [enabled, ids]);

  const add = useCallback((c: ChatInfo) => {
    rev.current += 1;
    added.current.set(c.id, rev.current);
    removed.current.delete(c.id);
    setChats(prev => (prev.some(p => p.id === c.id) ? prev : [...prev, c]));
  }, []);
  const remove = useCallback((id: string) => {
    rev.current += 1;
    removed.current.set(id, rev.current);
    added.current.delete(id);
    setChats(prev => (prev.some(p => p.id === id) ? prev.filter(p => p.id !== id) : prev));
  }, []);
  return { chats, loaded, add, remove };
}

/**
 * useChatProviders is GET /chat-providers: null until it answers (or when it
 * cannot be read), else the configured providers. The rail shows the Chats
 * group only when there is at least one.
 */
export function useChatProviders(enabled = true): ChatProvider[] | null {
  const [providers, setProviders] = useState<ChatProvider[] | null>(null);
  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    listChatProviders().then(p => { if (!cancelled) setProviders(p); }).catch(() => { /* no chat here */ });
    return () => { cancelled = true; };
  }, [enabled]);
  return providers;
}
