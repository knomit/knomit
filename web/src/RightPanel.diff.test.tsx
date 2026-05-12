import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
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
    view: 'history',
    factPath: 'kb/test/foo.md',
    asOf: { mode: 'diff', from: 'aaa1111', to: 'bbb2222' },
    headCommit: 'ccc3333',
    ...overrides,
  };
  const dispatch = vi.fn();
  const navigate = vi.fn();
  const result = render(<RightPanel state={state} dispatch={dispatch} navigate={navigate} />);
  return { ...result, dispatch, navigate, state };
}

describe('RightPanel — diff mode UX', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders CommitPanel above FactDiffView in history+diff', async () => {
    setupDiffHistory();
    // CommitPanel renders one row per file in the `to` commit detail.
    const files = await screen.findAllByTestId('commit-file');
    expect(files.length).toBe(2);
    // Both file paths from the commit are visible.
    expect(files.some(el => el.getAttribute('data-path') === 'kb/test/foo.md')).toBe(true);
    expect(files.some(el => el.getAttribute('data-path') === 'kb/test/bar.md')).toBe(true);
  });

  it('clicking a sibling file preserves diff asOf (does not collapse to scrubbed)', async () => {
    const { navigate } = setupDiffHistory();
    const files = await screen.findAllByTestId('commit-file');
    const sibling = files.find(el => el.getAttribute('data-path') === 'kb/test/bar.md')!;
    fireEvent.click(sibling);
    expect(navigate).toHaveBeenCalledWith({
      view: 'history',
      factPath: 'kb/test/bar.md',
      asOf: { mode: 'diff', from: 'aaa1111', to: 'bbb2222' },
    });
  });

  it('does not issue an api.fact request in diff mode', async () => {
    setupDiffHistory();
    await screen.findAllByTestId('commit-file');
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
