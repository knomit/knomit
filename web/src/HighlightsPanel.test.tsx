// Tests for the overview highlights block: the axis control, the two click
// targets (type pill filters, title opens), and the type glyph.

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
  // The impact count is no longer printed — it survives only as the tooltip.
  expect(rows[0]).not.toHaveTextContent('22');
  expect(rows[0].querySelector('[title]')).toHaveAttribute(
    'title', 'synthesis — derived from 22 facts');
});

it('renders a type glyph per row, coloured by the type', () => {
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  const icons = screen.getAllByTestId('highlight-type-icon');
  expect(icons).toHaveLength(2);
  // typeStyles.synthesis.color — the same pairing Library rows use, so a fact
  // reads identically wherever it appears.
  expect(icons[0].querySelector('svg')).toHaveAttribute('stroke', '#fa0');
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
  // synthesis") also match that regex, so it's ambiguous. Select the pill by
  // its value — the census now renders one per type.
  fireEvent.click(screen.getAllByTestId('tag-item')
    .find(el => el.getAttribute('data-value') === 'synthesis')!);
  expect(dispatch).toHaveBeenCalledWith({
    type: 'ADD_FILTER', chip: { category: 'type', value: 'synthesis' },
  });
});

it('type pills list EVERY type, including the two excluded from the rows', () => {
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  const pills = screen.getAllByTestId('tag-item').map(el => el.getAttribute('data-value'));
  // observation and reference never appear as ROWS, but the pills are the
  // folder's type filter — omitting them would make those types unfilterable.
  expect(pills).toContain('observation');
  expect(pills).toContain('reference');
  expect(pills).toContain('synthesis');
  // ...and the rows still exclude them.
  const rowText = screen.getAllByTestId('highlight-row').map(r => r.textContent).join(' ');
  expect(rowText).not.toMatch(/observation|reference/);
});

it('a folder of only observations still gets its type filter', () => {
  // The server returns no highlights (everything eligible is excluded) but a
  // full census. The pills must survive; the ranked list must not appear.
  render(<HighlightsPanel highlights={[]} types={{ observation: 12, reference: 2 }}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  const pills = screen.getAllByTestId('tag-item').map(el => el.getAttribute('data-value'));
  expect(pills).toEqual(['observation', 'reference']);
  expect(screen.queryByTestId('highlight-row')).toBeNull();
  expect(screen.queryByText('Highlights')).toBeNull();
});

it('an observation pill dispatches ADD_FILTER like any other type', () => {
  const dispatch = vi.fn();
  render(<HighlightsPanel highlights={[]} types={{ observation: 12 }}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={dispatch} />);
  fireEvent.click(screen.getByTestId('tag-item'));
  expect(dispatch).toHaveBeenCalledWith({
    type: 'ADD_FILTER', chip: { category: 'type', value: 'observation' },
  });
});

it('an axis button asks the owner to refetch and never re-sorts locally', () => {
  const onAxisChange = vi.fn();
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={onAxisChange} onOpen={vi.fn()} dispatch={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Confidence' }));

  expect(onAxisChange).toHaveBeenCalledWith('confidence');
  // Rows are unchanged — the server owns ranking; a local re-sort of an
  // already-truncated top-10 would not be the top-10 for the new axis.
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

it('renders no caption, but does label the pills — they sit beside Domains and Entities', () => {
  render(<HighlightsPanel highlights={highlights} types={types}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  expect(screen.queryByTestId('highlights-caption')).toBeNull();
  expect(screen.getByText('Types')).toBeInTheDocument();
});

it('renders nothing only when there is neither a type to filter nor a row to rank', () => {
  const { container } = render(<HighlightsPanel highlights={[]} types={{}}
    axis="confidence" onAxisChange={vi.fn()} onOpen={vi.fn()} dispatch={vi.fn()} />);
  expect(container).toBeEmptyDOMElement();
});
