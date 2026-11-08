import { useCallback, useState } from 'react';
import type { MouseEvent as ReactMouseEvent } from 'react';
import type { FilterChip } from './state';

// The chat's side panel: what it shows and how wide it is.
//
// WHAT IT SHOWS is one of three things, so the fact links, the commit links and
// the timeline's steps open the same panel: a tool call's result, a fact, or a
// commit. A fact or commit entry is complete (the panel fetches it), so a link
// only has to open it. There is no history here: the reader picks from the chat.

export type PanelEntry =
  | { kind: 'result'; callId: string }
  | { kind: 'fact'; repo: string; branch: string; path: string; commit?: string }
  | { kind: 'commit'; repo: string; branch: string; commit: string };

export interface PanelState {
  /** The entry on screen, or null while the panel is closed. */
  entry: PanelEntry | null;
}

export const initialPanelState: PanelState = { entry: null };

export type PanelAction =
  | { type: 'open'; entry: PanelEntry }
  | { type: 'close' };

export function panelReducer(s: PanelState, a: PanelAction): PanelState {
  switch (a.type) {
    case 'open': return { entry: a.entry };
    case 'close': return s.entry ? { entry: null } : s;
  }
}

/** The entry on screen, or null while the panel is closed. */
export const shownEntry = (s: PanelState): PanelEntry | null => s.entry;

/** A fact the panel sends to Browse: that fact, at `commit` when the panel read
 *  it at one, on the branch it was read from, with `panel` opened on landing and
 *  `filter` added to the list. */
export interface BrowseFactRequest {
  repo: string;
  path: string;
  branch?: string;
  commit?: string;
  filter?: FilterChip;
  /** A motif pivot picked from the fact's motif menu: Browse lands on the fact
   *  with that motif as its query, as its own pivot does. */
  motif?: string;
}

/** Whether the key went to something the person types into. */
export function typingIn(el: Element | null): boolean {
  if (!el) return false;
  const tag = el.tagName;
  return tag === 'TEXTAREA' || tag === 'INPUT' || tag === 'SELECT' || (el as HTMLElement).isContentEditable === true;
}

// ── width ──

export const PANEL_WIDTH_KEY = 'knomit.chatPanelWidth';
export const PANEL_DEFAULT_WIDTH = 470;
export const PANEL_MIN_WIDTH = 320;
/** The largest share of the chat body (the chat column plus the panel) the panel
 *  may take, so the chat column keeps room. App.css's `.k-chat-result`
 *  `max-width` is this same fraction of the same box; a test pins the two. */
export const PANEL_MAX_FRACTION = 0.6;

/** The width to keep: at least the minimum, at most the share of `container`
 *  (the chat body's width in px; unknown, 0, means no upper bound here and the
 *  stylesheet's cap alone shows). */
export function clampPanelWidth(w: number, container: number): number {
  const max = container > 0 ? Math.max(PANEL_MIN_WIDTH, Math.floor(container * PANEL_MAX_FRACTION)) : Infinity;
  return Math.min(max, Math.max(PANEL_MIN_WIDTH, Math.round(w)));
}

export function loadPanelWidth(): number {
  try {
    const n = Number(localStorage.getItem(PANEL_WIDTH_KEY));
    if (Number.isFinite(n) && n > 0) return clampPanelWidth(n, 0);
  } catch { /* storage disabled */ }
  return PANEL_DEFAULT_WIDTH;
}

/** usePanelWidth: the remembered width and the splitter's drag handler. The
 *  panel is on the right, so dragging LEFT widens it. A drag starts from the
 *  width the panel is SHOWN at (the stylesheet may cap what was stored, and a
 *  drag from the stored value would then seem stuck) and is clamped to the
 *  chat body's width, the box the stylesheet caps against. Persisted on
 *  release, not on every frame (the same pattern as the Library's width). */
export function usePanelWidth(): { width: number; onSplitterDown: (e: ReactMouseEvent) => void } {
  const [width, setWidth] = useState<number>(loadPanelWidth);
  const onSplitterDown = useCallback((e: ReactMouseEvent) => {
    e.preventDefault();
    const splitter = e.currentTarget as HTMLElement;
    const container = splitter.parentElement?.clientWidth ?? 0;
    const shown = (splitter.nextElementSibling as HTMLElement | null)?.offsetWidth ?? 0;
    const startX = e.clientX;
    const start = shown > 0 ? shown : width;
    const onMove = (ev: MouseEvent) => setWidth(clampPanelWidth(start - (ev.clientX - startX), container));
    const onUp = () => {
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
      setWidth(w => {
        try { localStorage.setItem(PANEL_WIDTH_KEY, String(w)); } catch { /* quota / disabled */ }
        return w;
      });
    };
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  }, [width]);
  return { width, onSplitterDown };
}

/** The breadcrumb for a path: the repo, then the path's directories (not the file). */
export function crumbOf(repo: string, path: string): string[] {
  const parts = path.split('/').filter(Boolean);
  return [repo, ...parts.slice(0, -1)].filter(Boolean);
}
