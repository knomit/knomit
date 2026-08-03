import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { LibraryHeader } from './LibraryHeader';

// Every render needs the location props now, and almost every test cares about
// exactly one of them. `base` carries a plain scoped location so each test
// overrides only what it is about.
const base = {
  count: 5,
  ancestors: ['kb', 'architecture'],
  leaf: 'store',
  narrow: false,
  sort: 'recent' as const,
  searchActive: false,
  onSortChange: () => {},
  canBack: true,
  onBack: () => {},
  onJumpAncestor: () => {},
};

describe('LibraryHeader', () => {
  // Was "renders fact count and scope label". The header names WHERE you are
  // instead of asserting a scoped boolean that never carried its value.
  it('renders the count, the current folder and its ancestors', () => {
    render(<LibraryHeader {...base} count={42} />);
    expect(screen.getByTestId('library-leaf')).toHaveTextContent('store');
    expect(screen.getAllByTestId('ancestor-seg').map(b => b.textContent)).toEqual(['kb', 'architecture']);
    expect(screen.getByText('42')).toBeInTheDocument();
  });

  // Was "renders global when not scoped". The root names the context.
  it('renders "All facts" at the root, keeping both lines and naming no repo', () => {
    render(<LibraryHeader {...base} ancestors={[]} leaf={null} count={1284} />);
    expect(screen.getByTestId('library-leaf')).toHaveTextContent('All facts');
    // The repo/branch is already in the TopBar; the header must not repeat it.
    expect(screen.getByTestId('library-header').textContent).not.toMatch(/core|main/);
    // Both lines present in every state, or the header changes height on the
    // first navigation and the whole list shifts under the cursor.
    expect(screen.getByTestId('library-ancestors')).toBeInTheDocument();
    expect(screen.queryByTestId('ancestor-seg')).toBeNull();
  });

  it('hides Relevance segment when search is not active', () => {
    render(<LibraryHeader {...base} count={5} sort="recent" searchActive={false} onSortChange={vi.fn()} />);
    expect(screen.queryByTestId('sort-relevance')).toBeNull();
  });

  it('shows Relevance segment when search is active', () => {
    render(<LibraryHeader {...base} count={5} sort="relevance" searchActive={true} onSortChange={vi.fn()} />);
    expect(screen.getByTestId('sort-relevance')).toBeInTheDocument();
  });

  it('dispatches onSortChange when a segment is clicked', () => {
    const handler = vi.fn();
    render(<LibraryHeader {...base} count={5} sort="recent" searchActive={false} onSortChange={handler} />);
    fireEvent.click(screen.getByTestId('sort-path'));
    expect(handler).toHaveBeenCalledWith('path');
  });

  it('renders sort axes as glyphs with the label as the accessible name (no visible text)', () => {
    render(<LibraryHeader {...base} count={5} sort="path" searchActive={false} onSortChange={vi.fn()} />);
    expect(screen.getByLabelText('Sort by Path')).toBeInTheDocument();
    expect(screen.getByLabelText('Sort by Recent')).toBeInTheDocument();
    const path = screen.getByTestId('sort-path');
    expect(path).not.toHaveTextContent('Path');
    expect(path.querySelector('svg')).not.toBeNull();
  });

  it('marks the active axis with aria-pressed and uses no selection border', () => {
    render(<LibraryHeader {...base} count={5} sort="path" searchActive={false} onSortChange={vi.fn()} />);
    const path = screen.getByTestId('sort-path');
    const recent = screen.getByTestId('sort-recent');
    expect(path.getAttribute('aria-pressed')).toBe('true');
    expect(recent.getAttribute('aria-pressed')).toBe('false');
    // Selection is color-only — no border on the active or idle control.
    expect((path as HTMLElement).style.borderStyle).toBe('none');
    expect((recent as HTMLElement).style.borderStyle).toBe('none');
  });

  it('disables Path/Recent while a search/relevance filter is active (only Relevance stays enabled)', () => {
    render(<LibraryHeader {...base} count={5} sort="relevance" searchActive={true} onSortChange={vi.fn()} />);
    expect((screen.getByTestId('sort-path') as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('sort-recent') as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('sort-relevance') as HTMLButtonElement).disabled).toBe(false);
  });

  it('does not dispatch onSortChange when a disabled (search-mode) segment is clicked', () => {
    const handler = vi.fn();
    render(<LibraryHeader {...base} count={5} sort="relevance" searchActive={true} onSortChange={handler} />);
    fireEvent.click(screen.getByTestId('sort-path'));
    fireEvent.click(screen.getByTestId('sort-recent'));
    expect(handler).not.toHaveBeenCalled();
  });

  it('keeps Path/Recent enabled when not searching', () => {
    render(<LibraryHeader {...base} count={5} sort="path" searchActive={false} onSortChange={vi.fn()} />);
    expect((screen.getByTestId('sort-path') as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByTestId('sort-recent') as HTMLButtonElement).disabled).toBe(false);
  });

  it('suppresses the focus ring so a clicked segment does not look extra-"selected"', () => {
    render(<LibraryHeader {...base} count={5} sort="recent" searchActive={false} onSortChange={vi.fn()} />);
    expect((screen.getByTestId('sort-path') as HTMLElement).style.outline).toBe('none');
    expect((screen.getByTestId('sort-recent') as HTMLElement).style.outline).toBe('none');
  });
});

describe('LibraryHeader — location', () => {
  const deep = ['core', 'kb', 'backend', 'architecture'];

  // THE off-by-one this API exists to prevent: the collapsed layout has fewer
  // items than the chain, so a handler wired to the RENDERED position would
  // navigate to the wrong ancestor.
  // The height invariant, as far as jsdom can see it: jsdom does no layout, so
  // the real assertion (root and scoped headers measure the same) can only be
  // made in a browser. What IS checkable is the mechanism — the empty root slot
  // carries the same font size and padding as a segment button, which is what
  // makes the two boxes equal. A hard-coded pixel height was off by 2.25px.
  it('gives the empty root slot the same box metrics as an ancestor segment', () => {
    const { unmount } = render(<LibraryHeader {...base} ancestors={[]} leaf={null} />);
    const placeholder = screen.getByTestId('library-context');
    const pad = placeholder.style.padding;
    const size = placeholder.style.fontSize;
    unmount();

    render(<LibraryHeader {...base} ancestors={['kb']} leaf="store" />);
    const seg = screen.getByTestId('ancestor-seg');
    expect(pad).toBe(seg.style.padding);
    expect(size).toBe(seg.style.fontSize);
  });

  it('calls onJumpAncestor with the FULL-array index, not the rendered position', () => {
    const onJumpAncestor = vi.fn();
    render(<LibraryHeader {...base} ancestors={deep} leaf="store" onJumpAncestor={onJumpAncestor} />);
    const segs = screen.getAllByTestId('ancestor-seg');
    // Collapsed: [core] […] [architecture] — the last button is rendered 2nd
    // but is index 3 in the chain.
    expect(segs).toHaveLength(2);
    fireEvent.click(segs[1]);
    expect(onJumpAncestor).toHaveBeenCalledWith(3);
  });

  it('collapses to root › … › parent above the inline limit', () => {
    render(<LibraryHeader {...base} ancestors={deep} leaf="store" />);
    expect(screen.getAllByTestId('ancestor-seg').map(b => b.textContent)).toEqual(['core', 'architecture']);
    expect(screen.getByTestId('crumb-overflow')).toBeInTheDocument();
  });

  it('the … lists the hidden segments in order and jumps by full index', () => {
    const onJumpAncestor = vi.fn();
    render(<LibraryHeader {...base} ancestors={deep} leaf="store" onJumpAncestor={onJumpAncestor} />);
    fireEvent.click(screen.getByTestId('crumb-overflow'));
    const items = screen.getAllByRole('menuitem');
    expect(items.map(i => i.textContent)).toEqual(['kb', 'backend']);
    fireEvent.click(items[1]);
    expect(onJumpAncestor).toHaveBeenCalledWith(2);
  });

  it('renders every ancestor inline when the chain is short enough', () => {
    render(<LibraryHeader {...base} ancestors={['kb', 'architecture']} leaf="store" />);
    expect(screen.getAllByTestId('ancestor-seg')).toHaveLength(2);
    expect(screen.queryByTestId('crumb-overflow')).toBeNull();
  });

  // Narrow drops the ROOT and keeps the immediate parent: going up one level is
  // the common move, and the root stays reachable inside the overflow menu.
  it('drops the root before the parent when narrow', () => {
    render(<LibraryHeader {...base} ancestors={deep} leaf="store" narrow />);
    expect(screen.getAllByTestId('ancestor-seg').map(b => b.textContent)).toEqual(['architecture']);
    fireEvent.click(screen.getByTestId('crumb-overflow'));
    expect(screen.getAllByRole('menuitem').map(i => i.textContent)).toEqual(['core', 'kb', 'backend']);
  });

  // The leaf is a destination, not a link — mirrors FilterBar.breadcrumb's
  // assertion that clicking the deepest segment dispatched nothing.
  it('does not render the current folder as a button', () => {
    render(<LibraryHeader {...base} ancestors={['kb']} leaf="store" />);
    const leaf = screen.getByTestId('library-leaf');
    expect(leaf.tagName.toLowerCase()).not.toBe('button');
    expect(leaf.querySelector('button')).toBeNull();
  });

  it('fires onBack when back is enabled', () => {
    const onBack = vi.fn();
    render(<LibraryHeader {...base} canBack onBack={onBack} />);
    fireEvent.click(screen.getByTestId('library-back'));
    expect(onBack).toHaveBeenCalled();
  });

  it('renders back disabled and does not fire it when there is nowhere to go', () => {
    const onBack = vi.fn();
    render(<LibraryHeader {...base} canBack={false} onBack={onBack} />);
    const btn = screen.getByTestId('library-back');
    expect(btn).toBeDisabled();
    fireEvent.click(btn);
    expect(onBack).not.toHaveBeenCalled();
  });
});

