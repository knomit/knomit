// Tests for the retracted-version badge in the RightPanel fact-detail header.
// The badge appears only when the History+scrubbed view loads a fact whose
// commit_hash differs from the URL's anchor commit (i.e. the backend's
// ?fallback=before walked back to a pre-retraction version).

import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';

// vi.mock is hoisted to the top of the file by vitest, so the factory cannot
// reference module-scope variables. Inline the fixtures.
vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    commitDetail: vi.fn().mockResolvedValue({
      commit: '416273e',
      date: '2026-05-01T00:00:00Z',
      message: 'retract obsolete fact',
      operation: 'retract',
      files: [{ path: 'kb/test/foo.md', action: 'deleted' }],
    }),
    explain: vi.fn().mockResolvedValue({ incoming: [], outgoing: [] }),
  },
}));

import { api } from './api';

function setup(state: AppState) {
  return render(<RightPanel state={state} dispatch={vi.fn()} navigate={vi.fn()} />);
}

const baseHistoryState: AppState = {
  ...init,
  repo: 'knomit',
  branch: 'machine/test',
  view: 'history',
  factPath: 'kb/test/foo.md',
  asOf: { mode: 'scrubbed', commit: '416273e' },
};

describe('RightPanel — retracted-version badge', () => {
  it('renders the badge with the retraction commit only — version hash is not duplicated', async () => {
    (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue({
      path: 'kb/test/foo.md',
      title: 'Foo',
      body: 'pre-retraction body',
      type: 'observation',
      confidence: 0.9,
      sources: 1,
      domain: [],
      entities: [],
      refs: [],
      // The fallback fired: this is the source commit, NOT the URL anchor.
      commit_hash: 'deadbee1234',
      commit_date: '2026-04-15T00:00:00Z',
    });

    setup(baseHistoryState);

    const badge = await screen.findByTestId('retracted-version-badge');
    expect(badge.textContent).toContain('retracted at 416273e');
    // The pre-retraction version hash already appears in the adjacent green
    // commit chip, so the yellow retracted-badge must not repeat it.
    expect(badge.textContent).not.toContain('showing version');
    expect(badge.textContent).not.toContain('deadbee');
  });

  it('does not render the badge when fact.commit_hash matches the anchor', async () => {
    (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue({
      path: 'kb/test/foo.md',
      title: 'Foo',
      body: 'body at this commit',
      type: 'observation',
      confidence: 0.9,
      sources: 1,
      domain: [],
      entities: [],
      refs: [],
      commit_hash: '416273e1234', // matches the URL anchor (7-char prefix)
      commit_date: '2026-05-01T00:00:00Z',
    });

    setup(baseHistoryState);

    // Wait for fact to load, then assert the badge is absent.
    await screen.findByTestId('fact-title');
    await waitFor(() => {
      expect(screen.queryByTestId('retracted-version-badge')).toBeNull();
    });
  });

  it('does not render the badge in live mode (no anchor)', async () => {
    (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue({
      path: 'kb/test/foo.md',
      title: 'Foo',
      body: 'live body',
      type: 'observation',
      confidence: 0.9,
      sources: 1,
      domain: [],
      entities: [],
      refs: [],
      commit_hash: 'aaa1111',
      commit_date: '2026-05-01T00:00:00Z',
    });

    setup({
      ...init,
      repo: 'knomit',
      branch: 'machine/test',
      factPath: 'kb/test/foo.md',
      asOf: { mode: 'live' },
    });

    await screen.findByTestId('fact-title');
    await waitFor(() => {
      expect(screen.queryByTestId('retracted-version-badge')).toBeNull();
    });
  });
});
