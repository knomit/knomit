// Tests for the overview highlights block: the axis control, the two click
// targets (type pill filters, title opens), and the caption that explains
// what the list is.

import { it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { HighlightsPanel } from './HighlightsPanel';
import type { Highlight } from './api';

const highlights: Highlight[] = [
  { path: 'kb/s/a.md', title: 'Big synthesis', type: 'synthesis',
    confidence: 0.60, impact: 22, committed_at: 1780000000 },
  { path: 'kb/s/b.md', title: 'Small synthesis', type: 'synthesis',
    confidence: 0.99, impact: 3, committed_at: 1781000000 },
];
const types = { synthesis: 2, observation: 40, reference: 3 };

it('renders rows in server order under the impact axis', () => {
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  const rows = screen.getAllByTestId('highlight-row');
  expect(rows).toHaveLength(2);
  expect(rows[0]).toHaveTextContent('Big synthesis');
  expect(rows[0]).toHaveTextContent('22');
});

it('opens a fact by path — a listing opens live, it is not a ref hop', () => {
  const onOpen = vi.fn();
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={onOpen} dispatch={vi.fn()} />);
  fireEvent.click(screen.getByText('Big synthesis'));
  expect(onOpen).toHaveBeenCalledWith('kb/s/a.md');
});

it('a type pill dispatches ADD_FILTER with category type', () => {
  const dispatch = vi.fn();
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={dispatch} />);
  // Not getByText(/synthesis/): both fixture titles ("Big synthesis", "Small
  // synthesis") also match that regex, so it's ambiguous. The type pill is
  // the only element carrying data-testid="tag-item" here.
  fireEvent.click(screen.getByTestId('tag-item'));
  expect(dispatch).toHaveBeenCalledWith({
    type: 'ADD_FILTER', chip: { category: 'type', value: 'synthesis' },
  });
});

it('excludes observation and reference from the type pills', () => {
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  expect(screen.queryByText(/observation/)).toBeNull();
  expect(screen.queryByText(/reference/)).toBeNull();
});

it('an axis button asks the owner to refetch and never re-sorts locally', () => {
  const onAxisChange = vi.fn();
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={onAxisChange} onOpen={vi.fn()} dispatch={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Confidence' }));

  expect(onAxisChange).toHaveBeenCalledWith('confidence');
  // Rows are unchanged — the server owns ranking; a local re-sort of a
  // truncated top-10 would contradict the caption.
  const rows = screen.getAllByTestId('highlight-row');
  expect(rows[0]).toHaveTextContent('Big synthesis');
});

it('keeps server order when the axis prop changes — the server ranks, not us', () => {
  const { rerender } = render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  expect(screen.getAllByTestId('highlight-row')[0]).toHaveTextContent('Big synthesis');

  rerender(<HighlightsPanel highlights={highlights} types={types}
    axis="confidence" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  // Confidence-descending would put Small (0.99) first. Server order must survive.
  expect(screen.getAllByTestId('highlight-row')[0]).toHaveTextContent('Big synthesis');
});

it('the caption names the active axis', () => {
  const { rerender } = render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  expect(screen.getByTestId('highlights-caption'))
    .toHaveTextContent(/most others were built on/i);

  rerender(<HighlightsPanel highlights={highlights} types={types}
    axis="recent" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  expect(screen.getByTestId('highlights-caption'))
    .toHaveTextContent(/most recently committed/i);
});

it('renders nothing when there are no highlights', () => {
  const { container } = render(<HighlightsPanel highlights={[]} types={{}}
    axis="confidence" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  expect(container).toBeEmptyDOMElement();
});
