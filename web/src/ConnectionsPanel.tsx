import { useRef, useCallback } from 'react';
import type { RefObject } from 'react';
import type { RefGroup } from './api';
import { EdgeRow } from './EdgeRow';
import { useDismiss } from './hooks';
import type { EdgeDir } from './utils';
import { EDGE_ACCENT, EDGE_GLYPH } from './utils';

export const CONNECTIONS_PANEL_WIDTH = 360;

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
  /** The header menu. Clicking a cell must not read as an outside click, or the toggle fights the dismiss. */
  menuRef: RefObject<HTMLElement | null>;
  onMouseEnter: () => void;
  onMouseLeave: () => void;
}

/**
 * The references panel: one direction at a time, dropped from the fact header's
 * connections cell.
 *
 * Right-aligned under the menu rather than spanning the pane, so it reads as
 * having opened from the cell that was clicked. It OVERLAYS the fact body — a
 * variant that pushed the prose down would relayout the whole fact on every
 * open and close, for a panel meant to be glanced at and dismissed.
 *
 * Positioned absolutely inside the header's control group, which means it
 * scrolls with the fact body. That is acceptable precisely because it is
 * transient: it closes when the pointer leaves, so it is not something a reader
 * scrolls away from while still using it.
 */
export function ConnectionsPanel({
  id, open, incoming, outgoing, error, onClose, onHop, menuRef, onMouseEnter, onMouseLeave,
}: Props) {
  const ref = useRef<HTMLDivElement>(null);

  useDismiss(open !== null, onClose, [ref, menuRef]);

  // Stable identity: EdgeRow is memoized, and an inline arrow here would be a
  // fresh prop on every render, making that memo inert.
  const handleHop = useCallback((group: RefGroup, commit: string) => {
    onHop(group.path, commit);
  }, [onHop]);

  const dir: EdgeDir = open ?? 'in';
  const groups = dir === 'in' ? incoming : outgoing;
  const accent = EDGE_ACCENT[dir];
  const title = dir === 'in' ? `${EDGE_GLYPH.in} Referenced by` : `${EDGE_GLYPH.out} References`;
  const live = groups.filter(g => !g.deleted).length;
  const retracted = groups.filter(g => g.deleted).length;

  return (
    <div
      id={id}
      ref={ref}
      data-testid="connections-panel"
      data-open={open !== null ? 'true' : 'false'}
      aria-hidden={open === null}
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
      style={{
        position: 'absolute',
        top: '100%',
        right: 0,
        marginTop: 6,
        width: CONNECTIONS_PANEL_WIDTH,
        maxHeight: 360,
        background: '#101010',
        border: '1px solid #2f2f2f',
        borderRadius: 6,
        boxShadow: '0 10px 30px rgba(0,0,0,0.6)',
        zIndex: 20,
        overflow: 'hidden',
        display: 'flex',
        flexDirection: 'column',
        textAlign: 'left',
        // Slides DOWN into place. Closed it is lifted, transparent, and
        // visibility:hidden so it cannot paint or be read — the delayed
        // visibility transition is what lets the close animate before it goes.
        transform: open ? 'translateY(0)' : 'translateY(-8px)',
        opacity: open ? 1 : 0,
        visibility: open ? 'visible' : 'hidden',
        transition: prefersReducedMotion
          ? 'none'
          : `opacity 160ms ease-out, transform 160ms ease-out, visibility 0s linear ${open ? '0s' : '160ms'}`,
        // Closed, it must not swallow clicks aimed at the fact body behind it.
        pointerEvents: open ? 'auto' : 'none',
      }}
    >
      <div style={{
        padding: '8px 10px', background: '#0d0d0d',
        borderBottom: '1px solid #1a1a1a',
        fontFamily: 'var(--k-font-mono)', fontSize: 10,
        letterSpacing: '0.06em', textTransform: 'uppercase',
        display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0,
      }}>
        <span style={{ color: accent }}>{title}</span>
        <span style={{ fontSize: 13, fontWeight: 600, color: accent }}>{live}</span>
        {retracted > 0 && (
          <span data-testid="panel-retracted" style={{ fontSize: 9, color: '#f88', textTransform: 'none' }}>
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
            data-testid="panel-close"
            onClick={onClose}
            aria-label="Close connections"
            style={{
              background: 'none', border: 'none', outline: 'none', padding: 0,
              borderRadius: 0, color: '#666', cursor: 'pointer', fontSize: 13, lineHeight: 1,
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
