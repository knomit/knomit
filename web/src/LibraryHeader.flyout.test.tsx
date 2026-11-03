// The results-list header as decided in the book design (§5 "Entry point",
// prototype item 1, option (d)) plus the user's 2026-11-02 additions:
// Materialize ALONE at the right with a hover colour, a vertical bar between
// it and the modes, no total; the modes collapse into a Photoshop-style
// flyout that opens INLINE TO THE LEFT.
//
// jsdom has no layout: "to the left" is pinned as DOM order plus the strip's
// right: 100% anchoring; the real-browser screenshots cover the geometry.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen } from '@testing-library/react';
import { LibraryHeader } from './LibraryHeader';
import { COLLAPSE_MS } from './ModeFlyout';

const base = {
  count: 5,
  ancestors: ['kb', 'architecture'],
  leaf: 'store',
  narrow: false,
  sort: 'recent' as const,
  searchActive: false,
  contentFiltered: false,
  onSortChange: () => {},
  canBack: true,
  onBack: () => {},
  onJumpAncestor: () => {},
};

const group = () => screen.getByTestId('mode-flyout');
const strip = () => screen.queryByTestId('mode-flyout-strip');
const current = () => group().querySelector('button[data-current="true"]') as HTMLButtonElement;
const isOpen = () => group().getAttribute('data-open') === 'true';

afterEach(() => { vi.useRealTimers(); });

describe('rest state', () => {
  it('one mode icon with the corner hint, then the separator, then Materialize last; no count', () => {
    render(<LibraryHeader {...base} count={1919} factset={{ enabled: true, onOpen: vi.fn() }} />);
    const g = group();
    expect(g.querySelectorAll('button')).toHaveLength(1);
    expect(current().getAttribute('data-testid')).toBe('sort-recent');
    expect(screen.queryByTestId('sort-path')).toBeNull();
    expect(strip()).toBeNull();
    const hint = screen.getByTestId('mode-flyout-hint');
    expect(current().contains(hint)).toBe(true);
    expect(hint.style.borderLeft).toBe('4px solid transparent');
    expect(hint.style.borderBottom).toMatch(/^4px solid/); // jsdom drops currentColor
    expect(hint.style.right).toBe('1px');
    expect(hint.style.bottom).toBe('1px');

    const header = screen.getByTestId('library-header');
    const kids = Array.from(header.children);
    expect(kids.slice(-3).map(k => k.getAttribute('data-testid')))
      .toEqual(['mode-flyout', 'library-header-sep', 'library-factset']);
    const sep = screen.getByTestId('library-header-sep');
    expect(sep.style.width).toBe('1px');
    expect(sep.style.background).toBe('rgb(58, 58, 58)');
    expect(header.textContent).not.toContain('1919');
    expect(screen.queryByTestId('library-count')).toBeNull();
  });

  it('without Materialize (the path tree) there is no separator either', () => {
    render(<LibraryHeader {...base} sort="path" />);
    expect(screen.queryByTestId('library-factset')).toBeNull();
    expect(screen.queryByTestId('library-header-sep')).toBeNull();
    expect(screen.getByTestId('library-header').lastElementChild).toBe(group());
  });
});

describe('expansion', () => {
  it('hover opens the other modes INLINE TO THE LEFT, icon-only, in an opaque strip', () => {
    render(<LibraryHeader {...base} sort="recent" />);
    fireEvent.mouseEnter(group());
    const s = strip()!;
    expect(s).not.toBeNull();
    // Left of the current icon: before it in the DOM and anchored at right: 100%.
    expect(s.compareDocumentPosition(current()) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(s.style.position).toBe('absolute');
    expect(s.style.right).toBe('100%');
    expect(s.style.background).toBe('rgb(15, 15, 15)');
    const path = screen.getByTestId('sort-path');
    expect(s.contains(path)).toBe(true);
    expect(path.textContent).toBe('');
    expect(path.querySelector('svg')?.getAttribute('width')).toBe('14');
    expect(current().querySelector('svg')?.getAttribute('width')).toBe('14');
  });

  it('keyboard focus on the current icon opens it too', () => {
    render(<LibraryHeader {...base} />);
    act(() => current().focus());
    expect(isOpen()).toBe(true);
    expect(screen.getByTestId('sort-path')).toBeTruthy();
  });

  it('one click picks a mode and collapses', () => {
    const onSortChange = vi.fn();
    render(<LibraryHeader {...base} sort="recent" onSortChange={onSortChange} />);
    fireEvent.mouseEnter(group());
    fireEvent.click(screen.getByTestId('sort-path'));
    expect(onSortChange).toHaveBeenCalledWith('path');
    expect(isOpen()).toBe(false);
    expect(strip()).toBeNull();
  });
});

describe('collapse', () => {
  it(`collapses ${COLLAPSE_MS}ms after the pointer leaves, not before`, () => {
    vi.useFakeTimers();
    render(<LibraryHeader {...base} />);
    fireEvent.mouseEnter(group());
    fireEvent.mouseLeave(group());
    act(() => { vi.advanceTimersByTime(COLLAPSE_MS - 1); });
    expect(isOpen()).toBe(true);
    act(() => { vi.advanceTimersByTime(1); });
    expect(isOpen()).toBe(false);
    expect(COLLAPSE_MS).toBe(250);
  });

  it('coming back before the delay cancels it (no flicker)', () => {
    vi.useFakeTimers();
    render(<LibraryHeader {...base} />);
    fireEvent.mouseEnter(group());
    fireEvent.mouseLeave(group());
    act(() => { vi.advanceTimersByTime(200); });
    fireEvent.mouseEnter(group());
    act(() => { vi.advanceTimersByTime(1000); });
    expect(isOpen()).toBe(true);
  });

  it('Esc collapses and puts focus back on the current icon, without re-opening', () => {
    render(<LibraryHeader {...base} />);
    act(() => current().focus());
    act(() => screen.getByTestId('sort-path').focus());
    fireEvent.keyDown(screen.getByTestId('sort-path'), { key: 'Escape' });
    expect(isOpen()).toBe(false);
    expect(document.activeElement).toBe(current());
  });

  it('focus leaving the group collapses it; moving inside does not', () => {
    render(<LibraryHeader {...base} />);
    act(() => current().focus());
    act(() => screen.getByTestId('sort-path').focus());
    expect(isOpen()).toBe(true);
    act(() => screen.getByTestId('library-back').focus());
    expect(isOpen()).toBe(false);
  });
});

describe('clicking the current icon', () => {
  it('keeps it open for a mouse', () => {
    render(<LibraryHeader {...base} />);
    const c = current();
    fireEvent.pointerDown(c, { pointerType: 'mouse' });
    fireEvent.click(c);
    expect(isOpen()).toBe(true);
    fireEvent.pointerDown(c, { pointerType: 'mouse' });
    fireEvent.click(c);
    expect(isOpen()).toBe(true);
  });

  it('toggles for touch and pen, even though the press focuses it', () => {
    render(<LibraryHeader {...base} />);
    const c = current();
    for (const pointerType of ['touch', 'pen']) {
      fireEvent.pointerDown(c, { pointerType });
      act(() => c.focus()); // a tap focuses the button between pointerdown and click
      fireEvent.click(c);
      expect(isOpen()).toBe(true);
      fireEvent.pointerDown(c, { pointerType });
      fireEvent.click(c);
      expect(isOpen()).toBe(false);
      act(() => c.blur());
    }
  });

  it('does not pick (or re-dispatch) its own mode', () => {
    const onSortChange = vi.fn();
    render(<LibraryHeader {...base} onSortChange={onSortChange} />);
    fireEvent.click(current());
    expect(onSortChange).not.toHaveBeenCalled();
  });
});

describe('keyboard', () => {
  it('arrow keys move between the modes in on-screen order while open', () => {
    render(<LibraryHeader {...base} sort="recent" />);
    act(() => current().focus());
    fireEvent.keyDown(current(), { key: 'ArrowLeft' });
    expect(document.activeElement).toBe(screen.getByTestId('sort-path'));
    // The left end holds.
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowLeft' });
    expect(document.activeElement).toBe(screen.getByTestId('sort-path'));
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(current());
  });

  it('arrow keys do not reach the list\'s window-level keys', () => {
    const seen: string[] = [];
    const onKey = (e: KeyboardEvent) => seen.push(e.key);
    window.addEventListener('keydown', onKey);
    try {
      render(<LibraryHeader {...base} />);
      act(() => current().focus());
      fireEvent.keyDown(current(), { key: 'ArrowLeft' });
      fireEvent.keyDown(document.activeElement!, { key: 'Enter' });
      expect(seen).toEqual([]);
    } finally {
      window.removeEventListener('keydown', onKey);
    }
  });

  it('only the current icon is in the Tab order', () => {
    render(<LibraryHeader {...base} />);
    fireEvent.mouseEnter(group());
    expect(current().tabIndex).toBe(0);
    expect(screen.getByTestId('sort-path').tabIndex).toBe(-1);
  });
});

describe('aria', () => {
  it('each icon is labelled "Sort by X" with a tooltip; the current one has haspopup and expanded', () => {
    render(<LibraryHeader {...base} sort="recent" />);
    expect(current()).toHaveAttribute('aria-haspopup', 'true');
    expect(current()).toHaveAttribute('aria-expanded', 'false');
    expect(current()).toHaveAttribute('aria-label', 'Sort by Recent');
    expect(current()).toHaveAttribute('title', 'Sort by Recent');
    fireEvent.mouseEnter(group());
    expect(current()).toHaveAttribute('aria-expanded', 'true');
    const path = screen.getByTestId('sort-path');
    expect(path).toHaveAttribute('aria-label', 'Sort by Path');
    expect(path).toHaveAttribute('title', 'Sort by Path');
    expect(path).not.toHaveAttribute('aria-haspopup');
  });
});

describe('Materialize', () => {
  it('is disabled for an empty list, and a click opens nothing', () => {
    const onOpen = vi.fn();
    const { rerender } = render(<LibraryHeader {...base} count={0} factset={{ enabled: true, onOpen }} />);
    const btn = screen.getByTestId('library-factset') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    fireEvent.click(btn);
    expect(onOpen).not.toHaveBeenCalled();
    rerender(<LibraryHeader {...base} count={1} factset={{ enabled: true, onOpen }} />);
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it('takes the header accent on hover, and only while enabled', () => {
    const { rerender } = render(<LibraryHeader {...base} factset={{ enabled: true, onOpen: vi.fn() }} />);
    const btn = screen.getByTestId('library-factset') as HTMLButtonElement;
    expect(btn.style.color).toBe('rgb(136, 136, 136)');
    expect(btn.querySelector('svg')?.getAttribute('stroke')).toBe('currentColor');
    fireEvent.mouseEnter(btn);
    // #7c9: the same accent as the lit mode.
    expect(btn.style.color).toBe('rgb(119, 204, 153)');
    fireEvent.mouseEnter(group());
    expect(current().style.color).toBe('rgb(119, 204, 153)');
    fireEvent.mouseLeave(btn);
    expect(btn.style.color).toBe('rgb(136, 136, 136)');
    // Hovered when it turns disabled (history, or the list empties): no accent.
    fireEvent.mouseEnter(btn);
    expect(btn.style.color).toBe('rgb(119, 204, 153)');
    rerender(<LibraryHeader {...base} count={0} factset={{ enabled: true, onOpen: vi.fn() }} />);
    expect(btn.style.color).toBe('rgb(136, 136, 136)');
  });
});
