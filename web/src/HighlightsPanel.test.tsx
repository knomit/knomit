// Tests for the overview highlights block: the axis control, opening a row,
// and the type glyph. The type census that used to render here is FacetPanel's
// — see FacetPanel.test.tsx.

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

it('renders rows in server order under the impact axis', () => {
  render(<HighlightsPanel highlights={highlights}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} />);
  const rows = screen.getAllByTestId('highlight-row');
  expect(rows).toHaveLength(2);
  expect(rows[0]).toHaveTextContent('Big synthesis');
  // The impact count is no longer printed — it survives only as the tooltip.
  expect(rows[0]).not.toHaveTextContent('22');
  expect(rows[0].querySelector('[title]')).toHaveAttribute(
    'title', 'synthesis — derived from 22 facts');
});

it('renders a type glyph per row, coloured by the type', () => {
  render(<HighlightsPanel highlights={highlights}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} />);
  const icons = screen.getAllByTestId('highlight-type-icon');
  expect(icons).toHaveLength(2);
  // typeStyles.synthesis.color — the same pairing Library rows use, so a fact
  // reads identically wherever it appears.
  expect(icons[0].querySelector('svg')).toHaveAttribute('stroke', '#fa0');
});

it('opens a fact by path — a listing opens live, it is not a ref hop', () => {
  const onOpen = vi.fn();
  render(<HighlightsPanel highlights={highlights}
    axis="impact" onAxisChange={vi.fn()} onOpen={onOpen} />);
  fireEvent.click(screen.getByText('Big synthesis'));
  expect(onOpen).toHaveBeenCalledWith('kb/s/a.md');
});

it('an axis button asks the owner to refetch and never re-sorts locally', () => {
  const onAxisChange = vi.fn();
  render(<HighlightsPanel highlights={highlights}
    axis="impact" onAxisChange={onAxisChange} onOpen={vi.fn()} />);
  fireEvent.click(screen.getByRole('button', { name: 'Confidence' }));

  expect(onAxisChange).toHaveBeenCalledWith('confidence');
  // Rows are unchanged — the server owns ranking; a local re-sort of an
  // already-truncated top-10 would not be the top-10 for the new axis.
  const rows = screen.getAllByTestId('highlight-row');
  expect(rows[0]).toHaveTextContent('Big synthesis');
});

it('keeps server order when the axis prop changes — the server ranks, not us', () => {
  const { rerender } = render(<HighlightsPanel highlights={highlights}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} />);
  expect(screen.getAllByTestId('highlight-row')[0]).toHaveTextContent('Big synthesis');

  rerender(<HighlightsPanel highlights={highlights}
    axis="confidence" onAxisChange={vi.fn()} onOpen={vi.fn()} />);
  // Confidence-descending would put Small (0.99) first. Server order must survive.
  expect(screen.getAllByTestId('highlight-row')[0]).toHaveTextContent('Big synthesis');
});

it('renders no caption and no facet pills — the type census moved to FacetPanel', () => {
  render(<HighlightsPanel highlights={highlights}
    axis="impact" onAxisChange={vi.fn()} onOpen={vi.fn()} />);
  expect(screen.queryByTestId('highlights-caption')).toBeNull();
  expect(screen.queryByText('Types')).toBeNull();
  expect(screen.queryAllByTestId('tag-item')).toHaveLength(0);
});

it('renders nothing when there is no row to rank — a folder with only a type census is FacetPanel\'s job now', () => {
  const { container } = render(<HighlightsPanel highlights={[]}
    axis="confidence" onAxisChange={vi.fn()} onOpen={vi.fn()} />);
  expect(container).toBeEmptyDOMElement();
});
