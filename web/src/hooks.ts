import { useEffect, useLayoutEffect, useState } from 'react';
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
 * Marks the element that CLIPS a hanging panel — an overflow:hidden ancestor —
 * so usePanelClamp can measure it. The fact view's pane wrapper carries it.
 */
export const PANEL_BOUNDS_ATTR = 'data-panel-bounds';
/** Room left between a clamped panel and the bounds' edge, so its shadow and border read as inside. */
export const PANEL_BOUNDS_GUTTER = 8;
/** The gap between a hanging panel and the bottom of the element it hangs from (its marginTop). */
export const PANEL_GAP = 6;

export interface PanelClamp {
  /** Available width from the panel's left edge to the bounds' right edge; undefined = unclamped. */
  maxWidth?: number;
  /** Available height from the panel's top to the bounds' bottom edge; undefined = unclamped. */
  maxHeight?: number;
}

/**
 * The space a panel hanging below its containing block (top:100%, left:0,
 * marginTop PANEL_GAP) has before the nearest PANEL_BOUNDS_ATTR ancestor cuts
 * it off. Measured on open and again on the triggers listed below.
 *
 * Measured from the panel's PARENT, not the panel: the parent is the
 * containing block the panel is anchored to, and the panel's own rect is
 * mid-transition while it slides in, so it would report a top up to 8px off.
 *
 * Only ever an upper bound. A panel keeps its nominal width and height and
 * shrinks to this when the pane is smaller; it scrolls its own list rather
 * than being cut. Outside a bounds element nothing is clamped.
 *
 * Re-measured whenever the answer can change: the bounds or the anchor
 * resizing (ResizeObserver — the list rail resizes the pane without resizing
 * the window), and the bounds SCROLLING, which moves the anchor inside a pane
 * of unchanged size (an overflow:hidden element still scrolls
 * programmatically, e.g. on scroll-into-view). Window `resize` is only the
 * fallback where ResizeObserver does not exist; alongside one it would
 * measure every resize twice. A measure that changes nothing keeps the
 * previous object, so it does not re-render the panel.
 */
export function usePanelClamp(ref: RefObject<HTMLElement | null>, active: boolean): PanelClamp {
  const [clamp, setClamp] = useState<PanelClamp>({});
  useLayoutEffect(() => {
    if (!active) return;
    const el = ref.current;
    const anchor = el?.parentElement;
    const bounds = el?.closest<HTMLElement>(`[${PANEL_BOUNDS_ATTR}]`);
    const update = (next: PanelClamp) => setClamp(prev =>
      prev.maxWidth === next.maxWidth && prev.maxHeight === next.maxHeight ? prev : next);
    if (!anchor || !bounds) { update({}); return; }
    const measure = () => {
      const b = bounds.getBoundingClientRect();
      const a = anchor.getBoundingClientRect();
      update({
        maxWidth: Math.max(0, b.right - PANEL_BOUNDS_GUTTER - a.left),
        maxHeight: Math.max(0, b.bottom - PANEL_BOUNDS_GUTTER - (a.bottom + PANEL_GAP)),
      });
    };
    measure();
    bounds.addEventListener('scroll', measure);
    const ro = typeof ResizeObserver === 'function' ? new ResizeObserver(measure) : null;
    if (ro) {
      ro.observe(bounds);
      ro.observe(anchor);
    } else {
      window.addEventListener('resize', measure);
    }
    return () => {
      bounds.removeEventListener('scroll', measure);
      if (ro) ro.disconnect();
      else window.removeEventListener('resize', measure);
    };
  }, [ref, active]);
  return clamp;
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
