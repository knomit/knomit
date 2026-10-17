import { describe, it, expect, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FacetColumns, FACET_NARROW_PX } from './FacetColumns';
import { IndexColumn } from './IndexColumn';

// #531 re-review S5: below FACET_NARROW_PX of its own width the facet
// stacks its index above the content, so the content keeps the whole width.
// jsdom measures every element at 0, so the width is stubbed.
// SABOTAGE: always lay out side by side -> the 375px case is not stacked -> red.
describe('FacetColumns', () => {
  const desc = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth');
  afterEach(() => { if (desc) Object.defineProperty(HTMLElement.prototype, 'clientWidth', desc); });
  const at = (w: number) => Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => w });

  it('stacks the index above the content when narrow', () => {
    at(375);
    render(<FacetColumns left={<nav>index</nav>} testId="cols"><div>content</div></FacetColumns>);
    expect(screen.getByTestId('cols')).toHaveAttribute('data-layout', 'stacked');
    expect(screen.queryByTestId('facet-splitter')).toBeNull();
  });

  it('keeps the side-by-side columns at desktop width', () => {
    at(FACET_NARROW_PX + 400);
    render(<FacetColumns left={<nav>index</nav>} testId="cols"><div>content</div></FacetColumns>);
    expect(screen.getByTestId('cols')).not.toHaveAttribute('data-layout');
    expect(screen.getByTestId('facet-splitter')).toBeInTheDocument();
  });

  // #533 review N4: stacked, the index is ONE horizontal strip with no
  // heading, so at 375px it takes a row above the content instead of a
  // third of the height over it; side by side it stays a column.
  // SABOTAGE: drop the FacetStacked provider from the stacked layout -> the
  // index stays a column with its heading at 375px -> red.
  it('draws the index as one strip when stacked, a column beside the content', () => {
    const items = [{ id: 'a', title: 'This instance' }, { id: 'b', title: 'Danger zone', danger: true }];
    at(375);
    const { unmount } = render(<FacetColumns left={<IndexColumn heading="Fleet" items={items} active="a" onPick={() => {}} />} testId="cols"><div>content</div></FacetColumns>);
    let nav = screen.getByRole('navigation', { name: 'Fleet' });
    expect(nav).toHaveAttribute('data-layout', 'strip');
    expect(nav.style.flexDirection).toBe('row');
    expect(screen.queryByTestId('settings-index-heading')).toBeNull();
    unmount();
    at(FACET_NARROW_PX + 400);
    render(<FacetColumns left={<IndexColumn heading="Fleet" items={items} active="a" onPick={() => {}} />} testId="cols"><div>content</div></FacetColumns>);
    nav = screen.getByRole('navigation', { name: 'Fleet' });
    expect(nav).not.toHaveAttribute('data-layout');
    expect(nav.style.flexDirection).toBe('column');
    expect(screen.getByTestId('settings-index-heading')).toHaveTextContent('Fleet');
  });
});
