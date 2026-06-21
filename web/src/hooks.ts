import { useEffect } from 'react';
import type { DependencyList, RefObject } from 'react';

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
