import { useCallback, useEffect, useRef, useState } from 'react';
import { listChats } from './chatApi';
import type { ChatInfo } from './chatApi';

/**
 * useChats is GET /chats: the caller's live chats, which is the BACKING LIST of
 * a pinned chat target. The pin-backing-list invariant needs a new chat to be
 * in the list no later than the render that pins it, so `add` is a state
 * update the caller batches with its SELECT_PINNED, never a refetch.
 *
 * An add() or remove() made while a read is in flight survives that read's
 * answer: the answer was cut before it and does not know about it.
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

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    const startedAt = rev.current;
    listChats().then(list => {
      if (cancelled) return;
      const gone = (id: string) => (removed.current.get(id) ?? -1) > startedAt;
      setChats(prev => {
        const out = list.filter(c => !gone(c.id));
        for (const c of prev) {
          if ((added.current.get(c.id) ?? -1) > startedAt && !out.some(o => o.id === c.id)) out.push(c);
        }
        return out;
      });
      setLoaded(true);
    }).catch(() => {
      // An unreadable list leaves it empty: a rail with no Chats rows. Not
      // loaded, so nothing is unpinned on its account.
    });
    return () => { cancelled = true; };
  }, [enabled]);

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
