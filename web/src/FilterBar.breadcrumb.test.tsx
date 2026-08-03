import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FilterBar } from './FilterBar';
import { init, reducer } from './state';

describe('FilterBar — history breadcrumb', () => {
  it('shows the trail breadcrumb when history', () => {
    // Build a history state with a 2-crumb trail via the reducer
    let state = reducer(init, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/a.md',
      asOf: { mode: 'live' },
    });
    state = reducer(state, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/b.md',
      asOf: { mode: 'history', commit: 'bbb1111' },
    });

    render(
      <FilterBar
        state={state}
        dispatch={vi.fn()}
        onJumpTrail={vi.fn()}
      />,
    );

    // The trail breadcrumb renders the crumb trail (titles fall back to the
    // basename here since api.fact is not mocked). The live affordance now lives
    // in the left-rail TimelineNav, not the breadcrumb.
    expect(screen.getByText('a')).toBeInTheDocument();
    expect(screen.getByText('b')).toBeInTheDocument();
  });
});
