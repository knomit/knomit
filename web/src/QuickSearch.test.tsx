// QuickSearch is the dashboard's way back to free-text search after the filter
// bar came off it. It is a BOX, not the bar: on a panel whose facet columns are
// already the picker, the bar's "+" has no job — and the chips have nothing to
// describe until there are results.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QuickSearch } from './QuickSearch';

const noop = () => {};

describe('QuickSearch', () => {
  it('is just a button until you open it', () => {
    render(<QuickSearch open={false} onOpen={noop} onClose={noop} onSubmit={noop} />);
    expect(screen.getByTestId('quick-search-open')).toBeTruthy();
    expect(screen.queryByTestId('quick-search-input')).toBeNull();
  });

  it('opens on the button', () => {
    const onOpen = vi.fn();
    render(<QuickSearch open={false} onOpen={onOpen} onClose={noop} onSubmit={noop} />);
    fireEvent.click(screen.getByTestId('quick-search-open'));
    expect(onOpen).toHaveBeenCalled();
  });

  it('takes the caret when it opens, so the key and the click both land typing', () => {
    render(<QuickSearch open onOpen={noop} onClose={noop} onSubmit={noop} />);
    expect(document.activeElement).toBe(screen.getByTestId('quick-search-input'));
  });

  it('holds the query until Enter — the dashboard must not vanish mid-word', () => {
    // Searching flips the list to relevance, which auto-opens the first result,
    // which REPLACES this panel. On a 300ms debounce that happens in the middle
    // of typing, taking the input's own surroundings with it.
    const onSubmit = vi.fn();
    render(<QuickSearch open onOpen={noop} onClose={noop} onSubmit={onSubmit} />);
    const input = screen.getByTestId('quick-search-input');
    fireEvent.change(input, { target: { value: 'agent failures' } });
    expect(onSubmit).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onSubmit).toHaveBeenCalledWith('agent failures');
  });

  it('ignores Enter on an empty or blank box', () => {
    const onSubmit = vi.fn();
    render(<QuickSearch open onOpen={noop} onClose={noop} onSubmit={onSubmit} />);
    const input = screen.getByTestId('quick-search-input');
    fireEvent.keyDown(input, { key: 'Enter' });
    fireEvent.change(input, { target: { value: '   ' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('trims what it submits', () => {
    const onSubmit = vi.fn();
    render(<QuickSearch open onOpen={noop} onClose={noop} onSubmit={onSubmit} />);
    fireEvent.change(screen.getByTestId('quick-search-input'), { target: { value: '  auth  ' } });
    fireEvent.keyDown(screen.getByTestId('quick-search-input'), { key: 'Enter' });
    expect(onSubmit).toHaveBeenCalledWith('auth');
  });

  it('closes on Escape and on the ×, because it opens two ways too', () => {
    // A keyboard-only exit strands whoever opened it by clicking.
    const onClose = vi.fn();
    const { rerender } = render(<QuickSearch open onOpen={noop} onClose={onClose} onSubmit={noop} />);
    fireEvent.keyDown(screen.getByTestId('quick-search-input'), { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);

    rerender(<QuickSearch open onOpen={noop} onClose={onClose} onSubmit={noop} />);
    fireEvent.click(screen.getByTestId('quick-search-close'));
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it('keeps Escape to itself', () => {
    // Escape at window level clears every filter. While this box is open it
    // means "put the box away", so the event must not travel on.
    const onClose = vi.fn();
    render(<QuickSearch open onOpen={noop} onClose={onClose} onSubmit={noop} />);
    const ev = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    const stop = vi.spyOn(ev, 'stopPropagation');
    screen.getByTestId('quick-search-input').dispatchEvent(ev);
    expect(stop).toHaveBeenCalled();
  });

  it('forgets the query between openings', () => {
    // Reopening to find yesterday's half-typed word still in the box reads as a
    // filter that is somehow still applied.
    const { rerender } = render(<QuickSearch open onOpen={noop} onClose={noop} onSubmit={noop} />);
    fireEvent.change(screen.getByTestId('quick-search-input'), { target: { value: 'stale' } });
    rerender(<QuickSearch open={false} onOpen={noop} onClose={noop} onSubmit={noop} />);
    rerender(<QuickSearch open onOpen={noop} onClose={noop} onSubmit={noop} />);
    expect((screen.getByTestId('quick-search-input') as HTMLInputElement).value).toBe('');
  });
});
