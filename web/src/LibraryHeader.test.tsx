import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { LibraryHeader } from './LibraryHeader';

describe('LibraryHeader', () => {
  it('renders fact count and scope label', () => {
    render(<LibraryHeader count={42} scoped={true} sort="recent" searchActive={false} onSortChange={vi.fn()} />);
    expect(screen.getByText(/42 facts/)).toBeInTheDocument();
    expect(screen.getByText(/scoped/)).toBeInTheDocument();
  });

  it('renders "global" when not scoped', () => {
    render(<LibraryHeader count={5} scoped={false} sort="recent" searchActive={false} onSortChange={vi.fn()} />);
    expect(screen.getByText(/global/)).toBeInTheDocument();
  });

  it('hides Relevance segment when search is not active', () => {
    render(<LibraryHeader count={5} scoped={false} sort="recent" searchActive={false} onSortChange={vi.fn()} />);
    expect(screen.queryByTestId('sort-relevance')).toBeNull();
  });

  it('shows Relevance segment when search is active', () => {
    render(<LibraryHeader count={5} scoped={false} sort="relevance" searchActive={true} onSortChange={vi.fn()} />);
    expect(screen.getByTestId('sort-relevance')).toBeInTheDocument();
  });

  it('dispatches onSortChange when a segment is clicked', () => {
    const handler = vi.fn();
    render(<LibraryHeader count={5} scoped={false} sort="recent" searchActive={false} onSortChange={handler} />);
    fireEvent.click(screen.getByTestId('sort-path'));
    expect(handler).toHaveBeenCalledWith('path');
  });

  it('renders sort axes as glyphs with the label as the accessible name (no visible text)', () => {
    render(<LibraryHeader count={5} scoped={false} sort="path" searchActive={false} onSortChange={vi.fn()} />);
    expect(screen.getByLabelText('Sort by Path')).toBeInTheDocument();
    expect(screen.getByLabelText('Sort by Recent')).toBeInTheDocument();
    const path = screen.getByTestId('sort-path');
    expect(path).not.toHaveTextContent('Path');
    expect(path.querySelector('svg')).not.toBeNull();
  });

  it('marks the active axis with aria-pressed and uses no selection border', () => {
    render(<LibraryHeader count={5} scoped={false} sort="path" searchActive={false} onSortChange={vi.fn()} />);
    const path = screen.getByTestId('sort-path');
    const recent = screen.getByTestId('sort-recent');
    expect(path.getAttribute('aria-pressed')).toBe('true');
    expect(recent.getAttribute('aria-pressed')).toBe('false');
    // Selection is color-only — no border on the active or idle control.
    expect((path as HTMLElement).style.borderStyle).toBe('none');
    expect((recent as HTMLElement).style.borderStyle).toBe('none');
  });

  it('disables Path/Recent while a search/relevance filter is active (only Relevance stays enabled)', () => {
    render(<LibraryHeader count={5} scoped={false} sort="relevance" searchActive={true} onSortChange={vi.fn()} />);
    expect((screen.getByTestId('sort-path') as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('sort-recent') as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('sort-relevance') as HTMLButtonElement).disabled).toBe(false);
  });

  it('does not dispatch onSortChange when a disabled (search-mode) segment is clicked', () => {
    const handler = vi.fn();
    render(<LibraryHeader count={5} scoped={false} sort="relevance" searchActive={true} onSortChange={handler} />);
    fireEvent.click(screen.getByTestId('sort-path'));
    fireEvent.click(screen.getByTestId('sort-recent'));
    expect(handler).not.toHaveBeenCalled();
  });

  it('keeps Path/Recent enabled when not searching', () => {
    render(<LibraryHeader count={5} scoped={false} sort="path" searchActive={false} onSortChange={vi.fn()} />);
    expect((screen.getByTestId('sort-path') as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByTestId('sort-recent') as HTMLButtonElement).disabled).toBe(false);
  });

  it('suppresses the focus ring so a clicked segment does not look extra-"selected"', () => {
    render(<LibraryHeader count={5} scoped={false} sort="recent" searchActive={false} onSortChange={vi.fn()} />);
    expect((screen.getByTestId('sort-path') as HTMLElement).style.outline).toBe('none');
    expect((screen.getByTestId('sort-recent') as HTMLElement).style.outline).toBe('none');
  });
});
