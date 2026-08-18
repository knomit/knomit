import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';
import type { Fact } from './api';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
    explain: vi.fn().mockResolvedValue({ incoming: [], outgoing: [] }),
  },
}));

import { api } from './api';

const baseFact: Fact = {
  path: 'kb/test/foo.md',
  title: 'Foo',
  body: 'body',
  domain: [],
  confidence: 1,
  sources: 1,
  entities: [],
  refs: [],
  commit_hash: 'bbb2222',
};

function renderPanel({ fact, anchorCommit }: { fact: Fact; anchorCommit?: string }) {
  (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce(fact);
  const state: AppState = {
    ...init,
    repo: 'knomit',
    branch: 'machine/test',
    view: 'library',
    factPath: fact.path,
    asOf: anchorCommit ? { mode: 'history', commit: anchorCommit } : { mode: 'live' },
    headCommit: 'ccc3333',
  };
  const dispatch = vi.fn();
  const result = render(<RightPanel state={state} dispatch={dispatch} />);
  return { ...result, dispatch, state };
}

describe('RightPanel — fact version date', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // The fact band shows the fact's OWN version date. Live: relative. History:
  // an absolute date in amber, because "3d ago" is meaningless once the reader
  // has deliberately travelled to a fixed point.
  it('renders a relative time in live mode', async () => {
    const threeDaysAgo = new Date(Date.now() - 3 * 86400_000).toISOString();
    renderPanel({ fact: { ...baseFact, commit_date: threeDaysAgo }, anchorCommit: undefined });

    const el = await screen.findByTestId('fact-version-date');
    expect(el).toHaveTextContent('3d ago');
    expect(el).toHaveAttribute('title', new Date(threeDaysAgo).toLocaleString());
  });

  it('renders an absolute amber date when anchored in history', async () => {
    const when = '2026-01-14T09:30:00Z';
    renderPanel({ fact: { ...baseFact, commit_date: when }, anchorCommit: 'c4f9e21' });

    const el = await screen.findByTestId('fact-version-date');
    expect(el).not.toHaveTextContent('ago');
    expect(el).toHaveTextContent('2026');
    expect(el).toHaveStyle({ color: '#e5a23c' });
  });

  it('renders nothing when the server sent no date', async () => {
    renderPanel({ fact: { ...baseFact, commit_date: undefined }, anchorCommit: undefined });

    await waitFor(() => expect(screen.getByTestId('fact-title')).toBeInTheDocument());
    expect(screen.queryByTestId('fact-version-date')).toBeNull();
  });
});
