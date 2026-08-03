import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { createRef } from 'react';
import { render, screen, fireEvent, act } from '@testing-library/react';
import { ConnectionsDrawer } from './ConnectionsDrawer';
import type { RefGroup } from './api';

const group = (path: string, title: string, deleted = false): RefGroup => ({
  path, title, type: 'observation', deleted,
  versions: [{ commit: `${path}-c`, committed_at: 1, deleted }],
});

const incoming = [group('kb/in.md', 'Inbound')];
const outgoing = [group('kb/out.md', 'Outbound')];

const base = {
  id: 'drawer',
  incoming,
  outgoing,
  error: null,
  onClose: () => {},
  onHop: () => {},
  barRef: createRef<HTMLElement>(),
};

beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
afterEach(() => vi.useRealTimers());

describe('ConnectionsDrawer', () => {
  // One direction at a time is the whole point of the control, so the other
  // direction's rows must be ABSENT, not merely below the fold.
  it('IN shows incoming only', () => {
    render(<ConnectionsDrawer {...base} open="in" />);
    expect(screen.getByText('↙ Referenced by')).toBeInTheDocument();
    expect(screen.getByText('Inbound')).toBeInTheDocument();
    expect(screen.queryByText('Outbound')).toBeNull();
  });

  it('OUT shows outgoing only', () => {
    render(<ConnectionsDrawer {...base} open="out" />);
    expect(screen.getByText('↗ References')).toBeInTheDocument();
    expect(screen.getByText('Outbound')).toBeInTheDocument();
    expect(screen.queryByText('Inbound')).toBeNull();
  });

  it('renders the retracted count in the header when non-zero', () => {
    render(<ConnectionsDrawer {...base} open="in" incoming={[group('kb/a.md', 'A'), group('kb/b.md', 'B', true)]} />);
    expect(screen.getByTestId('drawer-retracted')).toHaveTextContent('1 retracted');
  });

  // THE CLOSED OFFSET MUST CLEAR THE BAR, not just the drawer's own width. The
  // drawer is anchored at right:36, so translating by 340 leaves its leading
  // 36px exactly on top of the bar — and at zIndex 6 it wins, painting a sliver
  // of its own header over the counts. pointerEvents:'none' meant clicks still
  // reached the bar underneath, so every behavioural assertion passed while the
  // bar was invisible. 340 + 36.
  it('sits fully off-canvas when closed, clear of the bar', () => {
    const { rerender } = render(<ConnectionsDrawer {...base} open={null} />);
    const drawer = screen.getByTestId('connections-drawer');
    expect(drawer.style.transform).toBe('translateX(376px)');
    // Closed, it must not swallow clicks meant for the fact body behind it.
    expect(drawer.style.pointerEvents).toBe('none');

    rerender(<ConnectionsDrawer {...base} open="in" />);
    expect(drawer.style.transform).toBe('translateX(0)');
  });

  it('closes on Escape, on the × and on an outside click', () => {
    const onClose = vi.fn();
    const { unmount } = render(<ConnectionsDrawer {...base} open="in" onClose={onClose} />);
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();

    onClose.mockClear();
    fireEvent.click(screen.getByTestId('drawer-close'));
    expect(onClose).toHaveBeenCalled();

    onClose.mockClear();
    fireEvent.mouseDown(document.body);
    expect(onClose).toHaveBeenCalled();
    unmount();
  });

  describe('hover-out', () => {
    it('closes after the grace period', () => {
      const onClose = vi.fn();
      render(<ConnectionsDrawer {...base} open="in" onClose={onClose} />);
      fireEvent.mouseLeave(screen.getByTestId('connections-drawer'));
      expect(onClose).not.toHaveBeenCalled();      // not immediately
      act(() => { vi.advanceTimersByTime(300); });
      expect(onClose).toHaveBeenCalled();
    });

    // Without the grace period, a pointer clipping the corner on its way
    // elsewhere dismisses the drawer.
    it('re-entering within the grace period cancels the close', () => {
      const onClose = vi.fn();
      render(<ConnectionsDrawer {...base} open="in" onClose={onClose} />);
      const drawer = screen.getByTestId('connections-drawer');
      fireEvent.mouseLeave(drawer);
      act(() => { vi.advanceTimersByTime(100); });
      fireEvent.mouseEnter(drawer);
      act(() => { vi.advanceTimersByTime(500); });
      expect(onClose).not.toHaveBeenCalled();
    });

    // A multi-version edge's dropdown is portalled to document.body, so it is
    // not a descendant of the drawer: reaching for it fires mouseleave. Closing
    // then would dismiss the panel you opened the dropdown from.
    it('does not close while a portalled version dropdown is open', () => {
      const onClose = vi.fn();
      render(<ConnectionsDrawer {...base} open="in" onClose={onClose} />);
      const portal = document.createElement('div');
      portal.setAttribute('data-connections-portal', '');
      document.body.appendChild(portal);

      fireEvent.mouseLeave(screen.getByTestId('connections-drawer'));
      act(() => { vi.advanceTimersByTime(500); });
      expect(onClose).not.toHaveBeenCalled();

      portal.remove();
    });
  });

  it('shows a fetch error instead of rows', () => {
    render(<ConnectionsDrawer {...base} open="in" error="backend down" />);
    expect(screen.getByText(/backend down/)).toBeInTheDocument();
    expect(screen.queryByText('Inbound')).toBeNull();
  });
});
