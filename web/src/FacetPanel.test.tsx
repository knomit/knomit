// Tests for the summary view's facet strip: three ranked columns, the overflow
// browser behind "+N more", and the one-click filter on every row.

import { it, expect, vi, describe } from 'vitest';
import { render, screen, fireEvent, within } from '@testing-library/react';
import { FacetPanel } from './FacetPanel';

// A corpus shaped like a real one: the three facets overflow by wildly
// different magnitudes, which is the whole reason the browser exists.
const domains = {
  'agentic-engineering': 234, reliability: 95, architecture: 82, operations: 80,
  'distributed-systems': 63, security: 53, evaluation: 46, tools: 44,
  'context-engineering': 32, 'coding-agents': 26,
};
const entities = Object.fromEntries(
  Array.from({ length: 40 }, (_, i) => [`entity-${String(i).padStart(2, '0')}`, 100 - i]),
);
const types = { observation: 47, pattern: 46, heuristic: 39, reference: 30 };

function renderPanel(dispatch = vi.fn(), over: Partial<Parameters<typeof FacetPanel>[0]> = {}) {
  render(<FacetPanel domains={domains} entities={entities} types={types}
    dispatch={dispatch} {...over} />);
  return dispatch;
}

const col = (facet: string) =>
  screen.getAllByTestId('facet-column').find(c => c.getAttribute('data-facet') === facet)!;
const more = (facet: string) =>
  screen.getAllByTestId('facet-more').find(c => c.getAttribute('data-facet') === facet)!;

describe('columns', () => {
  it('renders one column per facet, headed by the FULL distinct count', () => {
    renderPanel();
    expect(screen.getAllByTestId('facet-column')).toHaveLength(3);
    // Not the count of visible rows — the panel's whole claim is that the top
    // six are a window onto more.
    expect(col('domain').textContent).toContain('Domains · 10');
    expect(col('entity').textContent).toContain('Entities · 40');
    expect(col('type').textContent).toContain('Types · 4');
  });

  it('shows the top six by count, descending', () => {
    renderPanel();
    const names = within(col('domain')).getAllByTestId('tag-item')
      .map(r => r.getAttribute('data-value'));
    expect(names).toEqual(['agentic-engineering', 'reliability', 'architecture',
      'operations', 'distributed-systems', 'security']);
  });

  it('breaks count ties by name so two renders of one corpus cannot disagree', () => {
    renderPanel(vi.fn(), { domains: { zebra: 5, alpha: 5, mid: 9 } });
    const names = within(col('domain')).getAllByTestId('tag-item')
      .map(r => r.getAttribute('data-value'));
    expect(names).toEqual(['mid', 'alpha', 'zebra']);
  });

  it('a row dispatches ADD_FILTER in its own facet category', () => {
    const dispatch = renderPanel();
    fireEvent.click(within(col('domain')).getByText('reliability'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'domain', value: 'reliability' },
    });

    fireEvent.click(within(col('entity')).getByText('entity-00'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'entity', value: 'entity-00' },
    });

    fireEvent.click(within(col('type')).getByText('observation'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'type', value: 'observation' },
    });
  });

  it('the type census lists EVERY type, including the ones excluded from highlight rows', () => {
    // observation and reference never rank as highlights, but they are the only
    // filter a folder of raw observations has. The census used to live in
    // HighlightsPanel and had to survive an empty highlights list; here it is
    // unconditional.
    renderPanel();
    const names = within(col('type')).getAllByTestId('tag-item')
      .map(r => r.getAttribute('data-value'));
    expect(names).toContain('observation');
    expect(names).toContain('reference');
  });

  it('offers "+N more" only where the facet actually overflows', () => {
    renderPanel();
    expect(more('domain').textContent).toBe('+4 more');
    expect(more('entity').textContent).toBe('+34 more');
    // Four types fit in six rows — no affordance, but the spacer holds the
    // column's height so the three stay aligned.
    expect(screen.getAllByTestId('facet-more').some(m => m.getAttribute('data-facet') === 'type'))
      .toBe(false);
    expect(screen.getAllByTestId('facet-more-spacer')).toHaveLength(1);
  });

  it('omits a facet with no values instead of heading an empty column', () => {
    renderPanel(vi.fn(), { types: {} });
    const facets = screen.getAllByTestId('facet-column').map(c => c.getAttribute('data-facet'));
    expect(facets).toEqual(['domain', 'entity']);
  });

  it('renders nothing at all when the folder has no facets', () => {
    const { container } = render(
      <FacetPanel domains={{}} entities={{}} types={{}} dispatch={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe('overflow browser', () => {
  it('"+N more" opens the browser for THAT facet, listing every value', () => {
    renderPanel();
    fireEvent.click(more('domain'));

    const browser = screen.getByTestId('facet-browser');
    expect(browser.getAttribute('data-facet')).toBe('domain');
    expect(within(browser).getAllByTestId('tag-item')).toHaveLength(10);
    // The columns are gone — the browser takes the same space, so Highlights
    // below it does not move.
    expect(screen.queryByTestId('facet-column')).toBeNull();
  });

  it('search narrows to values that never made the top six — the 714-entity case', () => {
    renderPanel();
    fireEvent.click(more('entity'));
    // entity-39 is 34 rows below the fold; no amount of scrolling a column
    // would have been a reasonable way to reach it.
    fireEvent.change(screen.getByTestId('facet-search'), { target: { value: 'ty-39' } });

    const rows = within(screen.getByTestId('facet-browser')).getAllByTestId('tag-item');
    expect(rows).toHaveLength(1);
    expect(rows[0].getAttribute('data-value')).toBe('entity-39');
  });

  it('search matches anywhere in the value, folding case on BOTH sides', () => {
    // Entities are proper nouns — "AWS Builders' Library", "Claude Code" — so
    // lowercasing only the query would leave the ones users actually hunt for
    // unreachable. Both directions are checked against the same fixture.
    renderPanel(vi.fn(), {
      entities: {
        "AWS Builders' Library": 58, Amazon: 58, Anthropic: 52, MCP: 29,
        'Claude Code': 24, OWASP: 13, Microsoft: 12, 'context rot': 8,
      },
    });
    fireEvent.click(more('entity'));
    const search = screen.getByTestId('facet-search');
    const shown = () => within(screen.getByTestId('facet-browser'))
      .getAllByTestId('tag-item').map(r => r.getAttribute('data-value'));

    fireEvent.change(search, { target: { value: 'aws' } });   // lower query, upper value
    expect(shown()).toEqual(["AWS Builders' Library"]);

    fireEvent.change(search, { target: { value: 'ROT' } });   // upper query, lower value
    expect(shown()).toEqual(['context rot']);

    fireEvent.change(search, { target: { value: 'o' } });     // matches mid-word, not just prefixes
    expect(shown()).toContain('Amazon');
    expect(shown()).toContain('Microsoft');
  });

  it('says so when nothing matches, rather than showing a blank pane', () => {
    renderPanel();
    fireEvent.click(more('domain'));
    fireEvent.change(screen.getByTestId('facet-search'), { target: { value: 'nope' } });
    expect(screen.getByTestId('facet-no-match')).toHaveTextContent('No domains match');
    expect(within(screen.getByTestId('facet-browser')).queryAllByTestId('tag-item')).toHaveLength(0);
  });

  it('a browser row filters and the browser STAYS open — picking several is normal', () => {
    // domain and type chips are OR-combined, entity chips AND-combined; closing
    // on the first pick would make every extra chip a fresh round trip.
    const dispatch = renderPanel();
    fireEvent.click(more('domain'));
    fireEvent.click(within(screen.getByTestId('facet-browser')).getByText('tools'));

    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'domain', value: 'tools' },
    });
    expect(screen.getByTestId('facet-browser')).toBeInTheDocument();
  });

  it('back returns to the columns', () => {
    renderPanel();
    fireEvent.click(more('entity'));
    fireEvent.click(screen.getByTestId('facet-back'));
    expect(screen.queryByTestId('facet-browser')).toBeNull();
    expect(screen.getAllByTestId('facet-column')).toHaveLength(3);
  });

  it('Escape closes the browser', () => {
    renderPanel();
    fireEvent.click(more('domain'));
    fireEvent.keyDown(screen.getByTestId('facet-browser'), { key: 'Escape' });
    expect(screen.queryByTestId('facet-browser')).toBeNull();
    expect(screen.getAllByTestId('facet-column')).toHaveLength(3);
  });

  it('a facet that empties under an open browser falls back to the columns', () => {
    // A refetch on a narrower path can drop every value of the open facet. A
    // browser over an empty list is a dead pane with no way back that reads as
    // one — the columns are the honest fallback.
    const { rerender } = render(
      <FacetPanel domains={domains} entities={entities} types={types} dispatch={vi.fn()} />);
    fireEvent.click(more('domain'));
    expect(screen.getByTestId('facet-browser')).toBeInTheDocument();

    rerender(<FacetPanel domains={{}} entities={entities} types={types} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('facet-browser')).toBeNull();
    expect(screen.getAllByTestId('facet-column').map(c => c.getAttribute('data-facet')))
      .toEqual(['entity', 'type']);
  });
});
