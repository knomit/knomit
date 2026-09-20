import { useEffect, useState } from 'react';
import type { DependencyList, RefObject } from 'react';
import { fetchVersion } from './api';

/**
 * Runs an async side-effect with automatic stale-flag management.
 * Equivalent to useEffect but provides a stale() checker so async
 * callbacks can bail out if the component re-ran before they resolved.
 * Pass all values read inside fn as deps (same rules as useEffect).
 */
export function useAsync(fn: (stale: () => boolean) => void, deps: DependencyList) {
  useEffect(() => {
    let isStale = false;
    fn(() => isStale);
    return () => { isStale = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

/**
 * Dismisses an open popover/dropdown on outside mousedown or Escape.
 * `insideRefs` are the elements that count as "inside" (e.g. the trigger and
 * the menu) — a mousedown within any of them is ignored. No-op while closed.
 */
export function useDismiss(
  open: boolean,
  onDismiss: () => void,
  insideRefs: ReadonlyArray<RefObject<HTMLElement | null>>,
) {
  useEffect(() => {
    if (!open) return;
    const onMouseDown = (e: MouseEvent) => {
      const target = e.target as Node | null;
      if (!target) return;
      if (insideRefs.some(r => r.current?.contains(target))) return;
      onDismiss();
    };
    const onKeyDown = (e: KeyboardEvent) => { if (e.key === 'Escape') onDismiss(); };
    document.addEventListener('mousedown', onMouseDown);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('mousedown', onMouseDown);
      window.removeEventListener('keydown', onKeyDown);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
}

/**
 * Fetches the running server's build version once the server is reachable and
 * returns its full string (e.g. "0.5.6.8a0f0e44"), or null until/unless it
 * resolves. A failed fetch stays null so callers can render nothing rather than
 * noise.
 *
 * `enabled` exists for the desktop, where the page can load minutes before the
 * API does. Firing early is not merely wasted: with no API base the URL
 * resolves against the webview origin, and the catch below would swallow the
 * result — so the version would stay null for the whole session with nothing
 * anywhere to say why. It re-fires when `enabled` flips, which is the point.
 */
export function useVersion(enabled = true): string | null {
  const [full, setFull] = useState<string | null>(null);
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    fetchVersion()
      .then(v => { if (alive) setFull(v.full); })
      .catch(() => { /* best-effort: no version on failure */ });
    return () => { alive = false; };
  }, [enabled]);
  return full;
}
