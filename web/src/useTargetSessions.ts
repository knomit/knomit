import { useCallback, useEffect, useState } from 'react';
import { api } from './api';
import type { ClientSession, ClientSessionPolicy } from './api';
import { useClientSessionChanges } from './useClientSessionChanges';

// The MCP client sessions of ONE target (a repo or a lens). There is no
// unfiltered reader: the global sessions view went with Manage's Sessions tab,
// and sessions bound to nothing are listed nowhere (accepted by the spec).
//
// Presence is LAST-SEEN derived — the server records each request's time and
// computes live/idle/dead at read time. No MCP client holds a connection open
// for presence, and kb/decisions/mcp/client-sessions/liveness-last-seen says
// it must stay that way. That decision is about the MCP client; it says
// nothing about the BROWSER, and this hook does subscribe to a server-pushed
// change stream, because a session that has already arrived should not wait
// up to 30s to appear. The stream carries "row X changed" and nothing else:
// every render still comes from the list endpoint.
//
// The 30s poll stays. It is the clock — the ages in the table advance with no
// events at all — and it is the fallback while the stream is down.

const POLL_MS = 30_000;

/** A target's binding, as the server spells it: `repo:<uid>` / `lens:<uid>`. */
export interface SessionTarget { kind: 'repo' | 'lens'; uid: string }

// Show hidden is a VIEW preference, not state the server owns, so it is
// remembered here rather than round-tripped. Every access is wrapped: a
// blocked or throwing localStorage (private windows, embedded webviews) must
// cost the page the preference and nothing else — the same shape
// repoSelection.ts uses for the last-context and splitter keys.
const SHOW_HIDDEN_KEY = 'knomit.sessions.showHidden';

export function readShowHidden(): boolean {
  try { return localStorage.getItem(SHOW_HIDDEN_KEY) === '1'; } catch { return false; }
}
export function writeShowHidden(on: boolean): void {
  // '0' rather than removing the key: an explicit "off" has to beat whatever
  // default a later version picks, and a missing key cannot say that.
  try { localStorage.setItem(SHOW_HIDDEN_KEY, on ? '1' : '0'); } catch { /* preference only */ }
}

export interface TargetSessions {
  rows: ClientSession[];
  /** The server bounds the page. A cut list looks exactly like a complete
   *  one, so this is the only thing that can tell a reader there is more. */
  truncated: boolean;
  policy: ClientSessionPolicy | null;
  error: string | null;
  /** The clock the ages are rendered against; advances on every attempt. */
  now: Date;
}

/**
 * useTargetSessions reads the sessions bound to `target` — filtered SERVER
 * side by `?binding=` — re-reading on the 30s poll, on window focus and on the
 * change stream. Exactly one sessions stream is open per mounted reader.
 *
 * `truncated` travels with the rows: they are a bounded page, so when the
 * server cut the list any count of them is a lower bound, not a total
 * (kb/decisions/web/sessions/badge-lower-bound).
 *
 * A target whose uid is unknown (an older server that does not send a lens's
 * uid) reads nothing: an empty binding would ask for every session.
 */
export function useTargetSessions(
  target: SessionTarget,
  showHidden: boolean,
): TargetSessions {
  const [rows, setRows] = useState<ClientSession[]>([]);
  const [truncated, setTruncated] = useState(false);
  const [policy, setPolicy] = useState<ClientSessionPolicy | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => new Date());
  const binding = target.uid ? `${target.kind}:${target.uid}` : null;

  const load = useCallback(() => {
    if (binding === null) return;
    api.listClientSessions({ binding, includeHidden: showHidden })
      .then(r => {
        setRows(r.sessions); setPolicy(r.policy); setTruncated(r.truncated); setError(null);
      })
      .catch(e => { setError(String(e)); })
      // finally, so the clock advances on EVERY attempt, success or failure.
      // The rows on screen are the last good data either way, so their ages
      // must keep moving while the error line is up — a frozen "2 min ago"
      // under an error reads as a session that is still fresh. It also keeps
      // this off the synchronous path: a setState in the effect body would
      // cascade a render on every mount.
      .finally(() => setNow(new Date()));
  }, [binding, showHidden]);

  // Live: the server pings when a row changes, and the hook throttles the
  // resulting re-reads.
  useClientSessionChanges(binding !== null, load);

  useEffect(() => {
    load();
    const t = setInterval(load, POLL_MS);
    const onFocus = () => load();
    window.addEventListener('focus', onFocus);
    return () => { clearInterval(t); window.removeEventListener('focus', onFocus); };
  }, [load]);

  return { rows, truncated, policy, error, now };
}
