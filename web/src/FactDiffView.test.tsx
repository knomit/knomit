import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { FactDiffView } from './FactDiffView';
import { init } from './state';

vi.mock('./api', () => ({
  api: {
    factDiff: vi.fn(),
  },
}));

import { api } from './api';

const baseFact = {
  path: 'kb/x.md', title: 'X', body: '', domain: [], confidence: 1, sources: 1, entities: [], refs: [],
};

describe('FactDiffView', () => {
  function makeState(overrides: Record<string, unknown> = {}) {
    return {
      ...init,
      factPath: 'kb/x.md',
      asOf: { mode: 'diff' as const, from: 'aaaaaaa', to: 'bbbbbbb' },
      ...overrides,
    } as Parameters<typeof FactDiffView>[0]['state'];
  }

  it('renders both commit chips', async () => {
    (api.factDiff as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      from: { ...baseFact, body: 'old' },
      to:   { ...baseFact, body: 'new' },
    });
    render(<FactDiffView state={makeState()} dispatch={vi.fn()} />);
    expect(await screen.findByText('aaaaaaa')).toBeInTheDocument();
    expect(screen.getByText('bbbbbbb')).toBeInTheDocument();
  });

  it('renders +/− glyphs for changed lines', async () => {
    (api.factDiff as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      from: { ...baseFact, body: 'a\nold\nb' },
      to:   { ...baseFact, body: 'a\nnew\nb' },
    });
    render(<FactDiffView state={makeState()} dispatch={vi.fn()} />);
    await waitFor(() => expect(screen.getByText(/^−\s+old$/)).toBeInTheDocument());
    expect(screen.getByText(/^\+\s+new$/)).toBeInTheDocument();
  });

  it('shows "not yet created" chip when from is null', async () => {
    (api.factDiff as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      from: null,
      to:   { ...baseFact, body: 'hello' },
    });
    render(<FactDiffView state={makeState()} dispatch={vi.fn()} />);
    expect(await screen.findByText(/not yet created at aaaaaaa/)).toBeInTheDocument();
  });

  it('shows "retracted at <to>" chip when to is null', async () => {
    (api.factDiff as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      from: { ...baseFact, body: 'gone' },
      to:   null,
    });
    render(<FactDiffView state={makeState()} dispatch={vi.fn()} />);
    expect(await screen.findByText(/retracted at bbbbbbb/)).toBeInTheDocument();
  });

  it('Exit diff button dispatches SET_AS_OF history-at-to', async () => {
    (api.factDiff as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ from: { ...baseFact }, to: { ...baseFact } });
    const dispatch = vi.fn();
    render(<FactDiffView state={makeState()} dispatch={dispatch} />);
    fireEvent.click(await screen.findByText('Exit diff'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'bbbbbbb' },
    });
  });

  it('aborts in-flight request on factPath change', async () => {
    let abortCalled = false;
    (api.factDiff as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (_r: string, _b: string, _p: string, _f: string, _t: string, signal: AbortSignal) =>
        new Promise((_, reject) => {
          signal.addEventListener('abort', () => { abortCalled = true; reject(new Error('aborted')); });
        })
    );
    const { rerender } = render(<FactDiffView state={makeState({ factPath: 'kb/a.md' })} dispatch={vi.fn()} />);
    rerender(<FactDiffView state={makeState({ factPath: 'kb/b.md' })} dispatch={vi.fn()} />);
    expect(abortCalled).toBe(true);
  });
});
