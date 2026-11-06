import { useCallback, useState } from 'react';
import type { MouseEvent as ReactMouseEvent } from 'react';

// The chat's side panel: what it shows, the history of what it has shown, and
// how wide it is.
//
// WHAT IT SHOWS is one of two things, so PR 6's fact links and the timeline's
// steps open the same panel: a tool call's result, or a fact. Only tool results
// can open it today; the fact entry is complete (the panel fetches it) so a
// fact link only has to call open().
//
// HISTORY works like a browser's: open() drops whatever was ahead of the
// current entry and pushes; back and forward move along. Closing the panel
// keeps the history (it is "what has been opened in this chat"), so reopening
// a step does not erase the way back.

export type PanelEntry =
  | { kind: 'result'; callId: string }
  | { kind: 'fact'; repo: string; branch: string; path: string; commit?: string };

export interface PanelState {
  entries: PanelEntry[];
  /** The entry on screen (or last on screen, while closed). -1 with no entries. */
  index: number;
  open: boolean;
}

export const initialPanelState: PanelState = { entries: [], index: -1, open: false };

export type PanelAction =
  | { type: 'open'; entry: PanelEntry }
  | { type: 'back' }
  | { type: 'forward' }
  | { type: 'close' };

export const sameEntry = (a: PanelEntry | undefined, b: PanelEntry | undefined): boolean => {
  if (!a || !b || a.kind !== b.kind) return false;
  if (a.kind === 'result' && b.kind === 'result') return a.callId === b.callId;
  if (a.kind === 'fact' && b.kind === 'fact') return a.repo === b.repo && a.branch === b.branch && a.path === b.path && a.commit === b.commit;
  return false;
};

export function panelReducer(s: PanelState, a: PanelAction): PanelState {
  switch (a.type) {
    case 'open': {
      // Opening what is already showing changes nothing; opening it again after
      // a close just reopens it.
      if (sameEntry(s.entries[s.index], a.entry)) return s.open ? s : { ...s, open: true };
      const entries = [...s.entries.slice(0, s.index + 1), a.entry];
      return { entries, index: entries.length - 1, open: true };
    }
    case 'back': return s.index > 0 ? { ...s, index: s.index - 1 } : s;
    case 'forward': return s.index < s.entries.length - 1 ? { ...s, index: s.index + 1 } : s;
    case 'close': return s.open ? { ...s, open: false } : s;
  }
}

/** The entry on screen, or null while the panel is closed. */
export const shownEntry = (s: PanelState): PanelEntry | null => (s.open ? s.entries[s.index] ?? null : null);

// ── width ──

export const PANEL_WIDTH_KEY = 'knomit.chatPanelWidth';
export const PANEL_DEFAULT_WIDTH = 470;
export const PANEL_MIN_WIDTH = 320;
/** The largest share of the window the panel may take, so the chat column keeps room. */
export const PANEL_MAX_FRACTION = 0.6;

const clamp = (w: number): number => {
  const max = Math.max(PANEL_MIN_WIDTH, Math.floor((typeof window === 'undefined' ? 1600 : window.innerWidth) * PANEL_MAX_FRACTION));
  return Math.min(max, Math.max(PANEL_MIN_WIDTH, Math.round(w)));
};

export function loadPanelWidth(): number {
  try {
    const n = Number(localStorage.getItem(PANEL_WIDTH_KEY));
    if (Number.isFinite(n) && n > 0) return clamp(n);
  } catch { /* storage disabled */ }
  return PANEL_DEFAULT_WIDTH;
}

/** usePanelWidth: the remembered width and the splitter's drag handler. The
 *  panel is on the right, so dragging LEFT widens it. Persisted on release, not
 *  on every frame (the same pattern as the Library's width). */
export function usePanelWidth(): { width: number; onSplitterDown: (e: ReactMouseEvent) => void } {
  const [width, setWidth] = useState<number>(loadPanelWidth);
  const onSplitterDown = useCallback((e: ReactMouseEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const start = width;
    const onMove = (ev: MouseEvent) => setWidth(clamp(start - (ev.clientX - startX)));
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
