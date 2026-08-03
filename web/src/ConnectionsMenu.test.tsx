import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ConnectionsCell } from './ConnectionsMenu';

const base = { dir: 'in' as const, count: 2, open: false, onToggle: () => {}, panelId: 'panel' };

describe('ConnectionsCell', () => {
  it('renders the direction glyph and the count', () => {
    render(<ConnectionsCell {...base} />);
    const cell = screen.getByTestId('connections-in');
    expect(cell).toHaveTextContent('↙');
    expect(cell).toHaveTextContent('2');
  });

  it('is reachable by accessible name', () => {
    render(<><ConnectionsCell {...base} /><ConnectionsCell {...base} dir="out" count={3} /></>);
    expect(screen.getByRole('button', { name: '2 incoming references' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '3 outgoing references' })).toBeInTheDocument();
  });

  it('fires onToggle with its direction', () => {
    const onToggle = vi.fn();
    render(<ConnectionsCell {...base} dir="out" onToggle={onToggle} />);
    fireEvent.click(screen.getByTestId('connections-out'));
    expect(onToggle).toHaveBeenCalledWith('out');
  });

  // Zero is not a button: no accent, no pointer, no panel. It still RENDERS, so
  // the header's control row does not reflow between a fact with edges and one
  // without.
  it('renders a zero count as inert, not a button', () => {
    const onToggle = vi.fn();
    render(<ConnectionsCell {...base} count={0} onToggle={onToggle} />);
    const zero = screen.getByTestId('connections-in');
    expect(zero.tagName.toLowerCase()).not.toBe('button');
    expect(zero.style.cursor).toBe('default');
    fireEvent.click(zero);
    expect(onToggle).not.toHaveBeenCalled();
    expect(zero).toHaveTextContent('0');
  });

  // The accent must live on the CELL so the glyph, the count and the open
  // marker all resolve currentColor from it. On a child, the marker inherits
  // the ambient colour and renders grey beside a coloured count.
  it('marks the open cell with an indicator in its own accent', () => {
    render(<ConnectionsCell {...base} open />);
    const cell = screen.getByTestId('connections-in');
    expect(cell.style.color).toBe('rgb(136, 170, 255)'); // #8af
    expect(cell.style.boxShadow).toContain('currentColor');
    // The panel hangs below, so the marker is on the bottom edge.
    expect(cell.style.boxShadow).toContain('-2px');
  });

  it('uses the out accent for the out direction', () => {
    render(<ConnectionsCell {...base} dir="out" open />);
    expect(screen.getByTestId('connections-out').style.color).toBe('rgb(255, 170, 136)'); // #fa8
  });

  // index.css styles bare `button` with border-radius:8px and font-size:1em.
  // Unnoticed, the cell renders as a rounded pill at 16px beside an 11px chip —
  // and the button and the div a zero renders as are then different heights, so
  // a count going 0 → 1 moves the header.
  it('overrides the global button styling, and matches the inert form', () => {
    render(<ConnectionsCell {...base} />);
    const asButton = screen.getByTestId('connections-in');
    expect(asButton.tagName.toLowerCase()).toBe('button');
    // Square: the cell is a slot in the header's control strip, which draws the
    // outer radius. What matters is that index.css's 8px did not win.
    expect(asButton.style.borderRadius).toBe('0px');

    const { container } = render(<ConnectionsCell {...base} count={0} />);
    const asDiv = container.querySelector('[data-testid="connections-in"]') as HTMLElement;
    expect(asDiv.tagName.toLowerCase()).toBe('div');
    expect(asDiv.style.fontSize).toBe(asButton.style.fontSize);
    expect(asDiv.style.padding).toBe(asButton.style.padding);
    expect(asDiv.style.lineHeight).toBe(asButton.style.lineHeight);
  });

  it('marks the open cell with aria-expanded', () => {
    render(<ConnectionsCell {...base} open />);
    expect(screen.getByTestId('connections-in')).toHaveAttribute('aria-expanded', 'true');
  });
});
