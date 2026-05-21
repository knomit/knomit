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
});
