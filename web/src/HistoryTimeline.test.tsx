import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { HistoryTimeline } from './HistoryTimeline';
import { init } from './state';

vi.mock('./api', () => ({
  api: {
    history: vi.fn().mockResolvedValue({
      entries: [
        { commit: 'aaa1111', date: '2026-05-01T00:00:00Z', message: 'first', operation: 'learn',
          files: { added: 2, modified: 1, deleted: 0 } },
        { commit: 'bbb2222', date: '2026-04-01T00:00:00Z', message: 'second', operation: 'update',
          files: { added: 0, modified: 3, deleted: 1 } },
        { commit: 'ccc3333', date: '2026-03-01T00:00:00Z', message: 'third', operation: 'retract',
          files: { added: 0, modified: 0, deleted: 2 } },
      ],
    }),
  },
}));

describe('HistoryList', () => {
  it('renders +A ~M -D file counts split', async () => {
    render(<HistoryTimeline state={init} dispatch={vi.fn()} navigate={vi.fn()} />);
    expect(await screen.findByText('+2')).toBeInTheDocument();
    expect(screen.getByText('~1')).toBeInTheDocument();
    expect(screen.getByText('~3')).toBeInTheDocument();
    expect(screen.getByText('−1')).toBeInTheDocument();
    expect(screen.getByText('−2')).toBeInTheDocument();
  });

  it('renders HEAD chip on first row', async () => {
    render(<HistoryTimeline state={init} dispatch={vi.fn()} navigate={vi.fn()} />);
    expect(await screen.findByText('HEAD')).toBeInTheDocument();
  });

  it('renders AT chip on the anchored row when scrubbed', async () => {
    const state = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'bbb2222' } };
    render(<HistoryTimeline state={state} dispatch={vi.fn()} navigate={vi.fn()} />);
    expect(await screen.findByText('AT')).toBeInTheDocument();
  });

  it('renders the legend strip', async () => {
    render(<HistoryTimeline state={init} dispatch={vi.fn()} navigate={vi.fn()} />);
    expect(await screen.findByText('● scrub')).toBeInTheDocument();
    expect(screen.getByText('┃ HEAD')).toBeInTheDocument();
    expect(screen.getByText('⌥+click = range')).toBeInTheDocument();
  });

  it('plain click navigates to history with scrubbed asOf', async () => {
    const navigate = vi.fn();
    render(<HistoryTimeline state={init} dispatch={vi.fn()} navigate={navigate} />);
    const rows = await screen.findAllByTestId('history-commit');
    fireEvent.click(rows[1]);
    expect(navigate).toHaveBeenCalledWith({
      view: 'history', factPath: null, asOf: { mode: 'scrubbed', commit: 'bbb2222' },
    });
  });

  it('⌥+click in live mode dispatches diff with HEAD as from', async () => {
    const dispatch = vi.fn();
    const state = { ...init, headCommit: 'aaa1111' };
    render(<HistoryTimeline state={state} dispatch={dispatch} navigate={vi.fn()} />);
    const rows = await screen.findAllByTestId('history-commit');
    fireEvent.click(rows[2], { altKey: true });
    expect(dispatch).toHaveBeenCalledWith({
      type: 'SET_AS_OF',
      asOf: { mode: 'diff', from: 'aaa1111', to: 'ccc3333' },
    });
  });

  it('⌥+click in scrubbed mode dispatches diff using current commit as from', async () => {
    const dispatch = vi.fn();
    const state = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'bbb2222' } };
    render(<HistoryTimeline state={state} dispatch={dispatch} navigate={vi.fn()} />);
    const rows = await screen.findAllByTestId('history-commit');
    fireEvent.click(rows[0], { altKey: true });
    expect(dispatch).toHaveBeenCalledWith({
      type: 'SET_AS_OF',
      asOf: { mode: 'diff', from: 'bbb2222', to: 'aaa1111' },
    });
  });

  it('⌥+click in diff mode re-anchors to', async () => {
    const dispatch = vi.fn();
    const state = { ...init, asOf: { mode: 'diff' as const, from: 'aaa1111', to: 'bbb2222' } };
    render(<HistoryTimeline state={state} dispatch={dispatch} navigate={vi.fn()} />);
    const rows = await screen.findAllByTestId('history-commit');
    fireEvent.click(rows[2], { altKey: true });
    expect(dispatch).toHaveBeenCalledWith({
      type: 'SET_AS_OF',
      asOf: { mode: 'diff', from: 'aaa1111', to: 'ccc3333' },
    });
  });

  it('plain click in diff mode demotes to scrubbed', async () => {
    const navigate = vi.fn();
    const state = { ...init, asOf: { mode: 'diff' as const, from: 'aaa1111', to: 'bbb2222' } };
    render(<HistoryTimeline state={state} dispatch={vi.fn()} navigate={navigate} />);
    const rows = await screen.findAllByTestId('history-commit');
    fireEvent.click(rows[0]);
    expect(navigate).toHaveBeenCalledWith({
      view: 'history', factPath: null, asOf: { mode: 'scrubbed', commit: 'aaa1111' },
    });
  });

  it('⌥+click on already-anchored commit collapses to plain click', async () => {
    const dispatch = vi.fn();
    const navigate = vi.fn();
    const state = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'bbb2222' } };
    render(<HistoryTimeline state={state} dispatch={dispatch} navigate={navigate} />);
    const rows = await screen.findAllByTestId('history-commit');
    fireEvent.click(rows[1], { altKey: true });
    // No SET_AS_OF for diff; navigate fires the plain-click path.
    const diffCalls = dispatch.mock.calls.filter(c => c[0].type === 'SET_AS_OF' && c[0].asOf.mode === 'diff');
    expect(diffCalls.length).toBe(0);
    expect(navigate).toHaveBeenCalledWith({
      view: 'history', factPath: null, asOf: { mode: 'scrubbed', commit: 'bbb2222' },
    });
  });
});
