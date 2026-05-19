import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, waitFor } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
    commitDetail: vi.fn().mockResolvedValue({
      commit: 'bbb2222',
      date: '2026-05-01T00:00:00Z',
      message: 'change two files',
      operation: 'learn',
      files: [
        { path: 'kb/test/foo.md', action: 'modified', title: 'Foo' },
        { path: 'kb/test/bar.md', action: 'modified', title: 'Bar' },
      ],
    }),
  },
}));

import { api } from './api';

function setupDiffHistory(overrides: Partial<AppState> = {}) {
  const state: AppState = {
    ...init,
    repo: 'knomit',
    branch: 'machine/test',
    view: 'library',
    factPath: 'kb/test/foo.md',
    asOf: { mode: 'diff', from: 'aaa1111', to: 'bbb2222' },
    headCommit: 'ccc3333',
    ...overrides,
  };
  const dispatch = vi.fn();
  const result = render(<RightPanel state={state} dispatch={dispatch} />);
  return { ...result, dispatch, state };
}

describe('RightPanel — diff mode UX', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('does not issue an api.fact request in diff mode', async () => {
    setupDiffHistory();
    // FactDiffView owns the fetching via api.factDiff. The outer panel must
    // not duplicate the request, which would otherwise flash a 404 error.
    await waitFor(() => expect(api.factDiff).toHaveBeenCalled());
    expect(api.fact).not.toHaveBeenCalled();
  });

  it('non-diff mode (scrubbed) still uses api.fact for single-sided render', async () => {
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      path: 'kb/test/foo.md',
      title: 'Foo',
      body: 'body',
      domain: [],
      confidence: 1,
      sources: 1,
      entities: [],
      refs: [],
      commit_hash: 'bbb2222',
    });
    setupDiffHistory({ asOf: { mode: 'scrubbed', commit: 'bbb2222' } });
    await waitFor(() => expect(api.fact).toHaveBeenCalled());
    expect(api.factDiff).not.toHaveBeenCalled();
  });
});
