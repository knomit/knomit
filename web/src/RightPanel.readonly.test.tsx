import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init, READ_ONLY_TITLE } from './state';
import type { AppState, AsOf } from './state';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn().mockResolvedValue({
      path: 'kb/test/foo.md',
      title: 'Foo',
      body: 'body',
      type: 'observation',
      confidence: 0.9,
      sources: 1,
      domain: [],
      entities: [],
      refs: [],
      commit_hash: 'aaa1111',
      commit_date: '2026-05-01T00:00:00Z',
    }),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    commitDetail: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
    explain: vi.fn().mockResolvedValue({ incoming: [], outgoing: [] }),
  },
}));

function setup(asOf: AsOf, overrides: Partial<AppState> = {}) {
  const state: AppState = {
    ...init,
    repo: 'knomit',
    branch: 'machine/test',
    factPath: 'kb/test/foo.md',
    asOf,
    ...overrides,
  };
  return render(<RightPanel state={state} dispatch={vi.fn()} />);
}

describe('RightPanel — read-only retract gate', () => {
  it('renders retract button enabled when live', async () => {
    setup({ mode: 'live' });
    const btn = await screen.findByTestId('retract-btn');
    expect(btn).not.toBeDisabled();
    expect(btn).toHaveAttribute('title', 'Retract fact');
  });

  it('renders retract button disabled with read-only tooltip when history', async () => {
    setup({ mode: 'history', commit: 'b812d40' });
    const btn = await screen.findByTestId('retract-btn');
    expect(btn).toBeDisabled();
    expect(btn).toHaveAttribute('title', READ_ONLY_TITLE);
  });

  it('routes to FactDiffView (no retract button) when in diff mode with a fact', async () => {
    setup({ mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
    // RightPanel routes to FactDiffView in diff mode, which has no retract button.
    await waitFor(() => {
      expect(screen.queryByTestId('retract-btn')).toBeNull();
    });
  });

  it('renders retract button disabled (read-only) when history', async () => {
    setup({ mode: 'history', commit: 'b812d40' });
    const btn = await screen.findByTestId('retract-btn');
    expect(btn).toBeDisabled();
    expect(btn).toHaveAttribute('title', READ_ONLY_TITLE);
  });
});

