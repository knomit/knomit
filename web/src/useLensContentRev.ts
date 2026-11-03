import { useEffect, useState } from 'react';
import { apiUrl } from './apiBase';
import { openEventStream } from './eventStream';

/** The lens's member-content stream (GET /lenses/{lens}/events): one
 *  `content` notice per member whose content changed, after its index
 *  absorbed it. It takes no query parameter. Built here, not in api.ts, so
 *  the tests that mock './api' wholesale still get it. */
export function lensEventsUrl(lens: string): string {
  return apiUrl(`/api/v1/lenses/${lens}/events`);
}

/**
 * useLensContentRev holds GET /lenses/{lens}/events open while a lens list is
 * on screen and counts the times a lens member's content changed: every
 * `content` notice (a member's index absorbed a write, the write repo or any
 * read member) and every REOPEN of the stream. The server sends no event ids
 * and replays nothing, and it ends the stream when the lens is edited; the
 * EventSource reconnects on its own, and a reopen may have missed notices, so
 * it counts as a change too. The first open does not: the list has just
 * loaded.
 *
 * The count is part of the lens list's head (Library), so a bump runs the
 * same quiet keep-place refresh a new write-repo head does (#739: one refresh
 * in flight, trailing-edge coalescing, read-on). '' (not a lens) holds no
 * stream and returns 0.
 */
export function useLensContentRev(lens: string): number {
  const [rev, setRev] = useState(0);
  useEffect(() => {
    if (!lens) return;
    const es = openEventStream(lensEventsUrl(lens));
    let opened = false;
    const bump = () => setRev(r => r + 1);
    es.addEventListener('open', () => {
      if (opened) bump();
      opened = true;
    });
    es.addEventListener('content', bump);
    return () => es.close();
  }, [lens]);
  return rev;
}
