import { describe, it, expect, vi } from 'vitest';
import { createRef } from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import { ConnectionsPanel } from './ConnectionsPanel';
import type { RefGroup } from './api';

const group = (path: string, title: string, deleted = false): RefGroup => ({
  path, title, type: 'observation', deleted,
  versions: [{ commit: `${path}-c`, committed_at: 1, deleted }],
});

const incoming = [group('kb/in.md', 'Inbound')];
const outgoing = [group('kb/out.md', 'Outbound')];

const base = {
  id: 'panel',
  incoming,
  outgoing,
  error: null,
  onClose: () => {},
  onHop: () => {},
  menuRef: createRef<HTMLElement>(),
  onMouseEnter: () => {},
  onMouseLeave: () => {},
};

describe('ConnectionsPanel', () => {
  // One direction at a time is the whole point of the control, so the other
  // direction's rows must be ABSENT, not merely below the fold.
  it('IN shows incoming only', () => {
    render(<ConnectionsPanel {...base} open="in" />);
    expect(screen.getByText('↙ Referenced by')).toBeInTheDocument();
    expect(screen.getByText('Inbound')).toBeInTheDocument();
    expect(screen.queryByText('Outbound')).toBeNull();
  });

  it('OUT shows outgoing only', () => {
    render(<ConnectionsPanel {...base} open="out" />);
    expect(screen.getByText('↗ References')).toBeInTheDocument();
    expect(screen.getByText('Outbound')).toBeInTheDocument();
    expect(screen.queryByText('Inbound')).toBeNull();
  });

  it('renders the retracted count in the header when non-zero', () => {
    render(<ConnectionsPanel {...base} open="in" incoming={[group('kb/a.md', 'A'), group('kb/b.md', 'B', true)]} />);
    expect(screen.getByTestId('panel-retracted')).toHaveTextContent('1 retracted');
  });

  // Closed it must be truly gone: transparent AND visibility:hidden, so it can
  // neither paint over the header nor be reached by a screen reader, and
  // pointerEvents:'none' so it cannot swallow clicks aimed at the fact body.
  // The previous side drawer taught this one — it stayed painted over the
  // control it belonged to while every behavioural test passed.
  it('is invisible, unreachable and inert when closed', () => {
    const { rerender } = render(<ConnectionsPanel {...base} open={null} />);
    const panel = screen.getByTestId('connections-panel');
    expect(panel.style.opacity).toBe('0');
    expect(panel.style.visibility).toBe('hidden');
    expect(panel.style.pointerEvents).toBe('none');
    expect(panel).toHaveAttribute('aria-hidden', 'true');
    // Lifted, so opening slides it DOWN into place.
    expect(panel.style.transform).toBe('translateY(-8px)');

    rerender(<ConnectionsPanel {...base} open="in" />);
    expect(panel.style.transform).toBe('translateY(0)');
    expect(panel.style.visibility).toBe('visible');
    expect(panel.style.pointerEvents).toBe('auto');
  });

  it('hangs from the menu, right-aligned', () => {
    render(<ConnectionsPanel {...base} open="in" />);
    const panel = screen.getByTestId('connections-panel');
    expect(panel.style.position).toBe('absolute');
    expect(panel.style.top).toBe('100%');
    expect(panel.style.right).toBe('0px');
  });

  it('closes on Escape, on the × and on an outside click', () => {
    const onClose = vi.fn();
    const { unmount } = render(<ConnectionsPanel {...base} open="in" onClose={onClose} />);
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();

    onClose.mockClear();
    fireEvent.click(screen.getByTestId('panel-close'));
    expect(onClose).toHaveBeenCalled();

    onClose.mockClear();
    fireEvent.mouseDown(document.body);
    expect(onClose).toHaveBeenCalled();
    unmount();
  });

  // The hover group is the header's control span (menu + panel), owned by
  // RightPanel, because the 6px gap between them belongs to neither element.
  // The panel only forwards.
  it('forwards hover to the owner of the close timer', () => {
    const onMouseEnter = vi.fn();
    const onMouseLeave = vi.fn();
    render(<ConnectionsPanel {...base} open="in" onMouseEnter={onMouseEnter} onMouseLeave={onMouseLeave} />);
    const panel = screen.getByTestId('connections-panel');
    fireEvent.mouseEnter(panel);
    expect(onMouseEnter).toHaveBeenCalled();
    fireEvent.mouseLeave(panel);
    expect(onMouseLeave).toHaveBeenCalled();
  });

  it('shows a fetch error instead of rows', () => {
    render(<ConnectionsPanel {...base} open="in" error="backend down" />);
    expect(screen.getByText(/backend down/)).toBeInTheDocument();
    expect(screen.queryByText('Inbound')).toBeNull();
  });
});
