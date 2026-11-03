// The results-list header as decided in the book design (§5 "Entry point",
// prototype item 1, option (d)) plus the user's 2026-11-02 additions:
// Materialize ALONE at the right with a hover colour, a vertical bar between
// it and the modes, no total; the modes collapse into a Photoshop-style
// flyout that opens INLINE TO THE LEFT.
//
// jsdom has no layout: "to the left" is pinned as DOM order plus the strip's
// right: 100% anchoring; the real-browser screenshots cover the geometry.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
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

  // #734 review (Safari): a pressed button is blurred with a null
  // relatedTarget BEFORE its click. Collapsing then would unmount the strip
  // and the click would land on whatever is underneath.
  it('a blur with no relatedTarget during a press inside does not eat the click', () => {
    const onSortChange = vi.fn();
    render(<LibraryHeader {...base} sort="recent" onSortChange={onSortChange} />);
    act(() => current().focus());
    const path = screen.getByTestId('sort-path');
    fireEvent.pointerDown(path, { pointerType: 'mouse' });
    fireEvent.focusOut(current(), { relatedTarget: null }); // React's onBlur is focusout
    expect(isOpen()).toBe(true);
    fireEvent.pointerUp(path, { pointerType: 'mouse' });
    fireEvent.click(path, { detail: 1 });
    expect(onSortChange).toHaveBeenCalledWith('path');
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

  // #734 review: Up/Down never belong to the flyout, and Left/Right only while
  // it is open, so the list keeps its arrows when focus sits on a mode icon.
  it('closed, every arrow reaches the list; open, Up/Down still do', () => {
    const seen: string[] = [];
    const onKey = (e: KeyboardEvent) => seen.push(e.key);
    window.addEventListener('keydown', onKey);
    try {
      render(<LibraryHeader {...base} />);
      act(() => current().focus());
      fireEvent.keyDown(current(), { key: 'Escape' }); // closed, focus kept
      expect(isOpen()).toBe(false);
      for (const key of ['ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight']) fireEvent.keyDown(current(), { key });
      expect(seen).toEqual(['ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight']);
      seen.length = 0;
      fireEvent.mouseEnter(group());
      for (const key of ['ArrowDown', 'ArrowUp']) fireEvent.keyDown(current(), { key });
      expect(seen).toEqual(['ArrowDown', 'ArrowUp']);
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

  // The hover accent is ONE CSS rule shared with the fact header's
  // Materialize (App.css .k-materialize, user 2026-11-03), so this pins the
  // class, that no inline colour outranks the :hover, and that the rule's
  // colour IS the lit mode's accent. Disabled is handled by :not(:disabled).
  // SABOTAGE: change the colour in App.css's .k-materialize:hover -> red.
  it('takes the header accent on hover via the shared rule, and only while enabled', () => {
    render(<LibraryHeader {...base} factset={{ enabled: true, onOpen: vi.fn() }} />);
    const btn = screen.getByTestId('library-factset') as HTMLButtonElement;
    expect(btn.classList.contains('k-materialize')).toBe(true);
    expect(btn.style.color).toBe('');
    expect(btn.style.getPropertyValue('--k-materialize-rest')).toBe('#888');
    expect(btn.querySelector('svg')?.getAttribute('stroke')).toBe('currentColor');
    const css = readFileSync('src/App.css', 'utf8');
    const m = css.match(/\.k-materialize:not\(:disabled\):hover\s*\{\s*color:\s*(#[0-9a-fA-F]+);\s*\}/);
    expect(m).not.toBeNull();
    // Resolve the rule's colour the way the browser would and compare it with
    // the lit mode's (ACCENT, #7c9).
    const probe = document.createElement('span');
    probe.style.color = m![1];
    fireEvent.mouseEnter(group());
    expect(current().style.color).toBe('rgb(119, 204, 153)');
    expect(probe.style.color).toBe(current().style.color);
  });
});


// User, 2026-11-03: unavailable modes are NOT SHOWN. When the current mode is
// all that is left (a motif pivot, a search, a chip-filtered list) there is
// nothing to pick, so the group neither expands nor shows the corner hint.
describe('nothing to pick', () => {
  const MOTIF = { canonical: 'failure-presents-as-success' };
  const cases: Array<[string, Partial<React.ComponentProps<typeof LibraryHeader>>, string]> = [
    ['a motif pivot', { sort: 'recent', motif: MOTIF, onExitMotif: vi.fn() }, 'sort-motif'],
    ['a relevance search', { sort: 'relevance', searchActive: true, onExitSearch: vi.fn() }, 'sort-relevance'],
    ['a chip-filtered list', { sort: 'recent', contentFiltered: true }, 'sort-recent'],
  ];

  // SABOTAGE: render the hint unconditionally in ModeFlyout.tsx -> red; drop
  // the .filter() after flyoutItems in LibraryHeader.tsx -> red.
  for (const [name, props, testid] of cases) {
    it(`${name}: one icon, no corner hint, and hover or focus expands nothing`, () => {
      render(<LibraryHeader {...base} {...props} />);
      expect(current().getAttribute('data-testid')).toBe(testid);
      expect(screen.queryByTestId('mode-flyout-hint')).toBeNull();
      expect(current()).not.toHaveAttribute('aria-haspopup');
      expect(current()).not.toHaveAttribute('aria-expanded');
      fireEvent.mouseEnter(group());
      expect(isOpen()).toBe(false);
      expect(strip()).toBeNull();
      act(() => current().focus());
      expect(isOpen()).toBe(false);
      expect(group().querySelectorAll('button')).toHaveLength(1);
    });
  }

  // Closed, Left/Right belong to the list, and a group with nothing to pick
  // is always closed — so the list keeps its arrows even after a hover.
  it('the list keeps Left/Right while the lone icon is hovered and focused', () => {
    const seen: string[] = [];
    const onKey = (e: KeyboardEvent) => seen.push(e.key);
    window.addEventListener('keydown', onKey);
    try {
      render(<LibraryHeader {...base} sort="recent" motif={MOTIF} onExitMotif={vi.fn()} />);
      fireEvent.mouseEnter(group());
      act(() => current().focus());
      fireEvent.keyDown(current(), { key: 'ArrowLeft' });
      fireEvent.keyDown(current(), { key: 'ArrowRight' });
      expect(seen).toEqual(['ArrowLeft', 'ArrowRight']);
    } finally {
      window.removeEventListener('keydown', onKey);
    }
  });

  it('the current icon keeps its exit', () => {
    const onExitMotif = vi.fn();
    render(<LibraryHeader {...base} sort="recent" motif={MOTIF} onExitMotif={onExitMotif} />);
    fireEvent.click(current());
    expect(onExitMotif).toHaveBeenCalledTimes(1);
  });
});

describe('arrow keys skip nothing visible', () => {
  // Every icon drawn in the open group is reachable by the arrows: walking
  // Left from the current icon to the end and back Right visits each one. A
  // greyed (disabled) icon would be drawn but skipped, which is the state this
  // pins out. SABOTAGE: drop the .filter() after flyoutItems in
  // LibraryHeader.tsx -> red (the chip case draws a disabled Path).
  const states: Array<[string, Partial<React.ComponentProps<typeof LibraryHeader>>]> = [
    ['path tree', { sort: 'path' }],
    ['recent', { sort: 'recent' }],
    ['chip-filtered', { sort: 'recent', contentFiltered: true }],
    ['searching', { sort: 'relevance', searchActive: true, onExitSearch: vi.fn() }],
  ];
  for (const [name, props] of states) {
    it(name, () => {
      render(<LibraryHeader {...base} {...props} />);
      fireEvent.mouseEnter(group());
      act(() => current().focus());
      const drawn = Array.from(group().querySelectorAll('button'));
      const visited = new Set<Element>([document.activeElement!]);
      for (let i = 0; i < drawn.length; i++) {
        fireEvent.keyDown(document.activeElement!, { key: 'ArrowLeft' });
        visited.add(document.activeElement!);
      }
      for (let i = 0; i < drawn.length; i++) {
        fireEvent.keyDown(document.activeElement!, { key: 'ArrowRight' });
        visited.add(document.activeElement!);
      }
      expect(drawn.every(b => visited.has(b))).toBe(true);
      expect(visited.size).toBe(drawn.length);
    });
  }
});
