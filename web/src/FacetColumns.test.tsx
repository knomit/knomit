import { describe, it, expect, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FacetColumns, FACET_NARROW_PX } from './FacetColumns';

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
});
