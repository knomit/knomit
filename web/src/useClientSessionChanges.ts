import { useEffect, useRef } from 'react';
import { subscribeClientSessions } from './api';
import type { ClientSessionChange } from './api';

/**
 * How long a `touch` waits before it is worth a list read.
 *
 * A touch says only that an existing row's last-seen, request count, and
 * possibly idle→live have moved. Every MCP request from every connected client
 * produces one, so a busy agent emits them several times a second — and
 * re-reading the list per request would turn one open Manage tab into a load
 * multiplier on the very server it is watching. Five seconds bounds that at one
 * read per open tab per window, which is still six times finer than the 30s
 * poll it supplements.
 */
const TOUCH_WINDOW_MS = 5_000;

/**
 * The floor between reads for a change that is worth showing at once. Small
 * enough to read as instant, large enough that a burst of arrivals — a fleet of
 * agents reconnecting after a restart — is still one read.
 */
const URGENT_WINDOW_MS = 1_000;

/**
 * useClientSessionChanges re-reads the client-session list when the server says
 * it changed, while `enabled`.
 *
 * URGENCY IS BY KIND, and that is the point of the hook:
 *
 *  - `init`, `end`, `purge` and a reconnect are rows APPEARING or GOING AWAY.
 *    That is what a reader watching this page is waiting for, so they read on
 *    the leading edge, at most one read per URGENT_WINDOW_MS.
 *  - `touch` is trailing-only on TOUCH_WINDOW_MS. It moves nothing a reader is
 *    waiting on, and it is the only kind that arrives at request rate.
 *
 * Steady state with a busy session is therefore one read per 5s per open tab,
 * while a session appearing or disappearing still shows up at once.
 *
 * An unreadable frame counts as urgent: something changed, and the cheap
 * mistake is a spare read, not a missed arrival.
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
    // Seeded to now, NOT to 0. The caller has just read the list — that is what
    // mounting this hook alongside a load means — so the window starts spent,
    // and the first moments of a subscription cannot bill a second identical
    // read.
    let lastRun = Date.now();
    let timer: ReturnType<typeof setTimeout> | null = null;
    let dueAt = 0;

    const run = () => { timer = null; lastRun = Date.now(); latest.current(); };

    // Schedules a read, keeping the EARLIEST pending deadline: a touch must
    // never push back a read an urgent change has already queued.
    const scheduleAt = (at: number) => {
      if (timer !== null && dueAt <= at) return;
      if (timer !== null) clearTimeout(timer);
      dueAt = at;
      timer = setTimeout(run, Math.max(0, at - Date.now()));
    };

    const onStreamChange = (change: ClientSessionChange) => {
      const urgent = change.type === 'reconnect' || change.kind !== 'touch';
      if (!urgent) {
        scheduleAt(Math.max(Date.now(), lastRun) + TOUCH_WINDOW_MS);
        return;
      }
      if (timer === null && Date.now() - lastRun >= URGENT_WINDOW_MS) run();
      else scheduleAt(lastRun + URGENT_WINDOW_MS);
    };

    const close = subscribeClientSessions(onStreamChange);
    return () => {
      close();
      // A trailing read after the caller has gone would be a request for a
      // list nobody is rendering — and, on unmount, a setState on a dead
      // component.
      if (timer !== null) clearTimeout(timer);
    };
  }, [enabled]);
}
