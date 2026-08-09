import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';

// P1.7 row-memoization evidence. Library's four list bodies used to be inline
// .map blocks: every row re-rendered (and re-allocated its 4–6 inline style
// objects) on ANY Library render — a parent state change, a paged append, a
// selection move. The rows are now extracted + memoized.
//
// Renders are counted through TypeIcon, which every row renders exactly once,
// so the count is a direct read of "how many rows rendered".

const renders = vi.hoisted(() => ({ typeIcon: 0 }));

vi.mock('./icons', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./icons')>();
  return {
    ...actual,
    TypeIcon: (props: React.ComponentProps<typeof actual.TypeIcon>) => {
      renders.typeIcon += 1;
      return <actual.TypeIcon {...props} />;
    },
  };
});

const ROWS = 40;

vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({ path: 'kb', children: [] }),
    recent: vi.fn(),
    search: vi.fn().mockResolvedValue({ results: [] }),
    listLensFacts: vi.fn(),
    lensSearch: vi.fn().mockResolvedValue([]),
    lensBrowse: vi.fn().mockResolvedValue({ children: [] }),
  },
}));

const lens: Lens = { name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-infra', name: 'infra' }] };

function lensState(overrides: Partial<AppState> = {}): AppState {
  return {
    ...init,
    repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa',
    context: { kind: 'lens', name: 'eng' }, lens, lensSources: null,
    librarySort: 'recent',
    ...overrides,
  };
}

function repoState(overrides: Partial<AppState> = {}): AppState {
  return {
    ...init,
    repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa',
    librarySort: 'recent',
    ...overrides,
  };
}

beforeEach(async () => {
  renders.typeIcon = 0;
  const { api } = await import('./api');
  (api.listLensFacts as ReturnType<typeof vi.fn>).mockResolvedValue({
    facts: Array.from({ length: ROWS }, (_, i) => ({
      path: `kb/f${i}.md`, title: `T${i}`, type: 'process', committed_at: i,
      source: { repo: 'infra', id: 'aaaaaaaaaaaa', branch: 'agent/main' },
    })),
    total: ROWS,
  });
  (api.recent as ReturnType<typeof vi.fn>).mockResolvedValue({
    facts: Array.from({ length: ROWS }, (_, i) => ({
      path: `kb/f${i}.md`, title: `T${i}`, type: 'process', committed_at: i,
    })),
    total: ROWS,
  });
});

describe('Library rows are memoized — a parent re-render costs zero row renders', () => {
  it('lens union rows do not re-render when Library re-renders with equivalent props', async () => {
    // Callback identities must be held stable, exactly as App does (dispatch is
    // a stable useCallback, navigate is a stable useRef).
    const dispatch = vi.fn();
    const navigate = vi.fn();
    const { rerender } = render(<Library state={lensState()} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(ROWS));

    const afterFirstPaint = renders.typeIcon;
    expect(afterFirstPaint).toBeGreaterThanOrEqual(ROWS);

    // A fresh state OBJECT with identical contents — what a parent re-render
    // looks like from the row's point of view.
    rerender(<Library state={lensState()} dispatch={dispatch} navigate={navigate} />);
    rerender(<Library state={lensState()} dispatch={dispatch} navigate={navigate} />);

    expect(screen.getAllByTestId('lens-item').length).toBe(ROWS);
    // Inline rows would have added 2 × ROWS (80) renders here.
    expect(renders.typeIcon).toBe(afterFirstPaint);
  });

  it('repo Recent rows do not re-render when Library re-renders with equivalent props', async () => {
    const dispatch = vi.fn();
    const navigate = vi.fn();
    const { rerender } = render(<Library state={repoState()} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(ROWS));

    const afterFirstPaint = renders.typeIcon;
    rerender(<Library state={repoState()} dispatch={dispatch} navigate={navigate} />);
    expect(renders.typeIcon).toBe(afterFirstPaint);
  });

  it('selecting a row re-renders only the rows whose selection actually changed', async () => {
    const dispatch = vi.fn();
    const navigate = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(ROWS));

    const rows = screen.getAllByTestId('lens-item');
    renders.typeIcon = 0;

    // Nothing selected → select row 5. Exactly one row's `selected` prop moves.
    fireEvent.click(rows[5]);
    expect(renders.typeIcon).toBe(1);

    // Move the selection: row 5 deselects, row 12 selects. Two rows, no more —
    // the other 38 are untouched. Inline rows re-rendered all 40 each time.
    renders.typeIcon = 0;
    fireEvent.click(rows[12]);
    expect(renders.typeIcon).toBe(2);
  });
});
