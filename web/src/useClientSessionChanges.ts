import { useEffect, useRef } from 'react';
import { subscribeClientSessions } from './api';

/** At most one re-read per second, however many events arrive. */
const THROTTLE_MS = 1_000;

/**
 * useClientSessionChanges re-reads the client-session list whenever the
 * server says it changed, while `enabled`.
 *
 * THROTTLED, and that is the point of the hook: every MCP request from every
 * connected client publishes an event, so a few busy Claude sessions produce
 * several per second. Leading edge fires immediately — the first arrival of a
 * new session must be visible at once, which is the whole reason this exists —
 * and the trailing edge is guaranteed, so the last event of a burst is never
 * the one that gets dropped. A burst of N events inside one second is at most
 * two reads.
 *
 * `onChange` is held in a ref: it is almost always a `useCallback` whose
 * identity changes with its own deps, and putting it in the effect's
 * dependencies would tear down and re-open the stream each time it did.
 *
 * Lives outside ManageSessions.tsx because RepoManager subscribes with it too
 * (and because that file exports components only, for react-refresh).
 */
export function useClientSessionChanges(enabled: boolean, onChange: () => void) {
  const latest = useRef(onChange);
  useEffect(() => { latest.current = onChange; }, [onChange]);

  useEffect(() => {
    if (!enabled) return;
    let lastRun = 0;
    let pending: ReturnType<typeof setTimeout> | null = null;

    const run = () => { pending = null; lastRun = Date.now(); latest.current(); };
    const onEvent = () => {
      // A trailing read is already scheduled; it will cover this event too.
      if (pending !== null) return;
      const wait = THROTTLE_MS - (Date.now() - lastRun);
      if (wait <= 0) run(); else pending = setTimeout(run, wait);
    };

    const close = subscribeClientSessions(onEvent);
    return () => {
      close();
      // A trailing read after the caller has gone would be a request for a
      // list nobody is rendering — and, on unmount, a setState on a dead
      // component.
      if (pending !== null) clearTimeout(pending);
    };
  }, [enabled]);
}
