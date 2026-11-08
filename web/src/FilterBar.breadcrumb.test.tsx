import { describe, it, expect, vi } from 'vitest';
import { useReducer } from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
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

  // A leftover crumb without any commit link, by the bar's own controls: open a
  // fact, add a path chip (the fact closes and its departure is on the stack), then
  // anchor with "at:". The fact the reader had open is a way back, not the view
  // on screen, so it is drawn as an ordinary back-crumb.
  it('draws no crumb as current when the filter bar anchored a view with no fact open', () => {
    function Bar() {
      const [state, dispatch] = useReducer(reducer, reducer(init, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/a.md', asOf: { mode: 'live' } }));
      return <><span data-testid="fact-path">{String(state.factPath)}</span><FilterBar state={state} dispatch={dispatch} onJumpTrail={vi.fn()} /></>;
    }
    render(<Bar />);
    const type = (text: string) => {
      const input = screen.getByRole('textbox');
      fireEvent.change(input, { target: { value: text } });
      fireEvent.keyDown(input, { key: 'Enter' });
    };
    type('path:kb/x');
    expect(screen.getByTestId('fact-path')).toHaveTextContent('null');
    type('at:c0ffee1');
    expect((screen.getByText('a') as HTMLElement).style.color).toBe('rgb(136, 136, 136)');
  });

  it('with a fact open in history, its crumb is drawn as current', () => {
    const open = reducer(reducer(init, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/a.md', asOf: { mode: 'live' } }), { type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'c0ffee1' } });
    render(<FilterBar state={open} dispatch={vi.fn()} onJumpTrail={vi.fn()} />);
    expect((screen.getByText('a') as HTMLElement).style.color).toBe('rgb(221, 221, 221)');
  });
});
