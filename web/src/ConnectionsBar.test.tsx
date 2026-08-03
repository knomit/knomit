import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ConnectionsBar } from './ConnectionsBar';

const base = { incoming: 2, outgoing: 3, open: null, onToggle: () => {}, drawerId: 'drawer' };

describe('ConnectionsBar', () => {
  it('renders both counts with horizontal labels', () => {
    render(<ConnectionsBar {...base} />);
    expect(screen.getByTestId('connections-in')).toHaveTextContent('2');
    expect(screen.getByTestId('connections-in')).toHaveTextContent('IN');
    expect(screen.getByTestId('connections-out')).toHaveTextContent('3');
    expect(screen.getByTestId('connections-out')).toHaveTextContent('OUT');
  });

  // A vertical label was tried and abandoned: at the size a 36px column allows
  // it measured 2.03:1, so the control whose premise is "IN or OUT?" could not
  // legibly say which.
  it('does not set a vertical writing mode on the labels', () => {
    render(<ConnectionsBar {...base} />);
    const label = screen.getByTestId('connections-out').querySelectorAll('span')[1];
    expect(label.style.writingMode).toBe('');
  });

  it('is reachable by accessible name', () => {
    render(<ConnectionsBar {...base} />);
    expect(screen.getByRole('button', { name: '2 incoming references' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '3 outgoing references' })).toBeInTheDocument();
  });

  it('fires onToggle with the direction', () => {
    const onToggle = vi.fn();
    render(<ConnectionsBar {...base} onToggle={onToggle} />);
    fireEvent.click(screen.getByTestId('connections-in'));
    expect(onToggle).toHaveBeenCalledWith('in');
    fireEvent.click(screen.getByTestId('connections-out'));
    expect(onToggle).toHaveBeenCalledWith('out');
  });

  // Zero is not a button: no pointer, no hover, no drawer. It is how the bar
  // says "nothing here" for the many facts that have nothing — and it is
  // rendered rather than hidden so the prose never reflows between facts.
  it('renders a zero count as inert, not a button', () => {
    const onToggle = vi.fn();
    render(<ConnectionsBar {...base} incoming={0} onToggle={onToggle} />);
    const zero = screen.getByTestId('connections-in');
    expect(zero.tagName.toLowerCase()).not.toBe('button');
    expect(zero.style.cursor).toBe('default');
    fireEvent.click(zero);
    expect(onToggle).not.toHaveBeenCalled();
  });

  it('still renders both counts at 0/0', () => {
    render(<ConnectionsBar {...base} incoming={0} outgoing={0} />);
    expect(screen.getByTestId('connections-in')).toHaveTextContent('0');
    expect(screen.getByTestId('connections-out')).toHaveTextContent('0');
  });

  // THE REGRESSION THE MOCK SHIPPED. The accent must live on the ITEM so the
  // count, the label and the selected indicator all resolve currentColor from
  // it. Put the accent on a child and the indicator inherits the ambient #eee,
  // rendering a white bar beside a blue count.
  it('gives the selected side an indicator in its own accent', () => {
    render(<ConnectionsBar {...base} open="in" />);
    const inItem = screen.getByTestId('connections-in');
    expect(inItem.style.color).toBe('rgb(136, 170, 255)'); // #8af
    expect(inItem.style.boxShadow).toContain('currentColor');
    expect(screen.getByTestId('connections-out').style.boxShadow).toBe('none');
  });

  it('marks the open side with aria-expanded', () => {
    render(<ConnectionsBar {...base} open="out" />);
    expect(screen.getByTestId('connections-out')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('connections-in')).toHaveAttribute('aria-expanded', 'false');
  });
});
