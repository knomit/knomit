/**
 * Flap-limited connection logging for an EventSource, shared by every stream
 * the UI holds open.
 *
 * Extracted from App.tsx's branch-events effect rather than copied, because
 * the reasoning below was learned once and would rot in two places:
 *
 * EventSource silently auto-reconnects. Without an error handler, a backend
 * that 500s the stream leaves the UI stale with no signal at all. But logging
 * every disconnect/reconnect pair is its own failure: a backend that accepts
 * and immediately drops re-arms on every retry (~3s), which is ~40 lines a
 * minute — enough to flush the console's 500-entry ring, of exactly the lines
 * it exists for, in about twelve minutes.
 *
 * So outages are counted over a rolling window: the first few report normally,
 * and past the limit the pair goes quiet behind ONE summary line until the
 * stream has been calm for a full window.
 */

/** See the note above on why the re-arm needs a ceiling. */
export const FLAP_WINDOW_MS = 60_000;
export const FLAP_LIMIT = 3;

export interface OutageLog {
  /**
   * Report that the stream dropped. Deduplicated: repeated errors before a
   * recovery are one outage, which is what EventSource emits while retrying.
   */
  lost(message: string): void;
  /**
   * Report that the stream is back, and say whether it actually logged.
   *
   * Only reports a recovery for an outage that was REPORTED. The very first
   * open has nothing to recover from, and a reconnect during a suppressed flap
   * storm must stay as quiet as the disconnect it paired with — otherwise
   * suppression halves the noise instead of stopping it.
   *
   * The return value lets a caller hang its own recovery work (re-reading
   * state that the gap may have invalidated) on the same condition, which
   * keeps that work off the hot path of a flapping stream: a stream that is
   * flapping must not also become a request storm.
   */
  recovered(message: string): boolean;
}

export function createOutageLog(report: (level: 'info' | 'error', message: string) => void): OutageLog {
  let loggedDisconnect = false;
  let windowStart = 0;
  let outages = 0;
  let suppressed = false;

  return {
    lost(message: string) {
      if (loggedDisconnect) return;
      loggedDisconnect = true;

      const now = Date.now();
      if (now - windowStart > FLAP_WINDOW_MS) { windowStart = now; outages = 0; suppressed = false; }
      outages += 1;
      if (outages > FLAP_LIMIT) {
        if (!suppressed) {
          suppressed = true;
          report('error', '[events] stream flapping — suppressing further connection lines');
        }
        return;
      }
      report('error', message);
    },

    recovered(message: string): boolean {
      const report_it = loggedDisconnect && !suppressed;
      if (report_it) report('info', message);
      loggedDisconnect = false;
      return report_it;
    },
  };
}
