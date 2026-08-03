import { useRef, useEffect, useCallback } from 'react';
import type { RefObject } from 'react';
import type { RefGroup } from './api';
import { EdgeRow } from './EdgeRow';
import { useDismiss } from './hooks';
import type { EdgeDir } from './ConnectionsBar';
import { CONNECTIONS_BAR_WIDTH } from './ConnectionsBar';

export const CONNECTIONS_DRAWER_WIDTH = 340;

/** Grace period before a hover-out closes the drawer. */
const HOVER_CLOSE_MS = 250;

// Matches LeftPanel's guard, including the jsdom/SSR checks.
const prefersReducedMotion =
  typeof window !== 'undefined' &&
  typeof window.matchMedia === 'function' &&
  window.matchMedia('(prefers-reduced-motion: reduce)').matches;

interface Props {
  id: string;
  /** null = closed. Which direction is showing, when open. */
  open: EdgeDir | null;
  incoming: RefGroup[];
  outgoing: RefGroup[];
  error: string | null;
  onClose: () => void;
  /** Same shape as tt.hopEdge; the RefGroup → path bridge is done here. */
  onHop: (path: string, pinnedCommit: string) => void;
  /** The bar. Clicking it must not read as an outside click, or the toggle fights the dismiss. */
  barRef: RefObject<HTMLElement | null>;
}

/**
 * The connections drawer: one direction at a time, slid out from the bar.
 *
 * A DRAWER, NOT A CARD. Flush to the bar (`right: 36`, no right border, no
 * radius) and full height of the pane region, so it reads as having come from
 * the handle you clicked. An earlier attempt with a 10px gap, an all-round
 * border and a radius read as a floating popup — which is also why it starts at
 * the top of the pane rather than below the fact header, even though that means
 * covering the version chip and the retract button while open. That cost is
 * transient and smaller than the 300px column this replaces.
 *
 * It animates TRANSFORM, not opacity: a fade reads as a dialog appearing, and
 * the slide is what makes it legible as having come from the bar.
 */
export function ConnectionsDrawer({
  id, open, incoming, outgoing, error, onClose, onHop, barRef,
}: Props) {
  const ref = useRef<HTMLDivElement>(null);
  const closeTimer = useRef<number | undefined>(undefined);

  useDismiss(open !== null, onClose, [ref, barRef]);

  const cancelHoverClose = () => {
    window.clearTimeout(closeTimer.current);
    closeTimer.current = undefined;
  };

  /**
   * Hover-out closes, after a grace period.
   *
   * The delay is not decoration: without it, a pointer clipping the corner of
   * the drawer on its way somewhere else dismisses it, and re-entering after a
   * 1px exit would have to re-open it.
   *
   * THE PORTAL GUARD is the subtle half. A multi-version edge renders its
   * version dropdown through createPortal into document.body, so it is NOT a
   * DOM descendant of this drawer — moving the pointer into that dropdown fires
   * mouseleave here. Closing then would dismiss the drawer the moment you
   * reached for the version you wanted to open. So a hover-close is suppressed
   * while any such dropdown is mounted.
   */
  const scheduleHoverClose = () => {
    cancelHoverClose();
    closeTimer.current = window.setTimeout(() => {
      if (document.querySelector('[data-connections-portal]')) return;
      onClose();
    }, HOVER_CLOSE_MS);
  };

  // A pending close must not fire after the drawer has already gone, or after
  // the fact changed underneath it.
  useEffect(() => cancelHoverClose, []);
  useEffect(() => { if (open === null) cancelHoverClose(); }, [open]);

  // Stable identity: EdgeRow is memoized, and an inline arrow here would be a
  // fresh prop on every render, making that memo inert.
  const handleHop = useCallback((group: RefGroup, commit: string) => {
    onHop(group.path, commit);
  }, [onHop]);

  const dir: EdgeDir = open ?? 'in';
  const groups = dir === 'in' ? incoming : outgoing;
  const accent = dir === 'in' ? '#8af' : '#fa8';
  const title = dir === 'in' ? '↙ Referenced by' : '↗ References';
  const live = groups.filter(g => !g.deleted).length;
  const retracted = groups.filter(g => g.deleted).length;

  return (
    <div
      id={id}
      ref={ref}
      data-testid="connections-drawer"
      data-open={open !== null ? 'true' : 'false'}
      aria-hidden={open === null}
      onMouseEnter={cancelHoverClose}
      onMouseLeave={scheduleHoverClose}
      style={{
        position: 'absolute',
        right: CONNECTIONS_BAR_WIDTH,
        top: 0,
        bottom: 0,
        width: CONNECTIONS_DRAWER_WIDTH,
        background: '#101010',
        borderLeft: '1px solid #2f2f2f',
        borderRight: 0,
        borderRadius: 0,
        boxShadow: '-20px 0 50px rgba(0,0,0,0.7)',
        zIndex: 6,
        overflow: 'hidden',
        display: 'flex',
        flexDirection: 'column',
        // Off-canvas must clear the BAR too, not just the drawer's own width.
        // Anchored at right:36, translating by 340 leaves the leading 36px
        // sitting exactly on top of the bar — and at zIndex 6 it wins, so the
        // closed drawer painted a sliver of its own header over the counts.
        // pointerEvents:'none' meant clicks still reached the bar underneath,
        // so it was invisible to every behavioural test.
        transform: open
          ? 'translateX(0)'
          : `translateX(${CONNECTIONS_DRAWER_WIDTH + CONNECTIONS_BAR_WIDTH}px)`,
        transition: prefersReducedMotion ? 'none' : 'transform 180ms ease-out',
        // Closed, it must not swallow clicks aimed at the fact body behind it.
        pointerEvents: open ? 'auto' : 'none',
      }}
    >
      <div style={{
        padding: '9px 12px', background: '#0d0d0d',
        borderBottom: '1px solid #1a1a1a',
        fontFamily: 'var(--k-font-mono)', fontSize: 10,
        letterSpacing: '0.06em', textTransform: 'uppercase',
        display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0,
      }}>
        <span style={{ color: accent }}>{title}</span>
        <span style={{ fontSize: 13, fontWeight: 600, color: accent }}>{live}</span>
        {retracted > 0 && (
          <span data-testid="drawer-retracted" style={{ fontSize: 9, color: '#f88', textTransform: 'none' }}>
            {retracted} retracted
          </span>
        )}
        <span style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 8 }}>
          <span style={{
            fontSize: 9, color: '#555', border: '1px solid #2a2a2a',
            borderRadius: 3, padding: '0 4px', textTransform: 'none',
          }}>esc</span>
          <button
            type="button"
            data-testid="drawer-close"
            onClick={onClose}
            aria-label="Close connections"
            style={{
              background: 'none', border: 'none', outline: 'none', padding: 0,
              color: '#666', cursor: 'pointer', fontSize: 13, lineHeight: 1,
            }}
          >×</button>
        </span>
      </div>

      <div style={{ flex: 1, overflowY: 'auto', minHeight: 0 }}>
        {error && <div style={{ color: '#f66', fontSize: 12, padding: '8px 12px' }}>{error}</div>}
        {!error && groups.map(g => (
          <EdgeRow key={g.path} group={g} onHop={handleHop} />
        ))}
      </div>
    </div>
  );
}
