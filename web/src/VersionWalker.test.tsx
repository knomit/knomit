import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { VersionWalker } from './VersionWalker';

vi.mock('./api', () => ({
  api: {
    factCommits: vi.fn(),
  },
}));

const versions = [
  { commit: 'ccc3333', date: '2026-05-15T00:00:00Z', message: 'v3', operation: 'modify', files: {} },
  { commit: 'bbb2222', date: '2026-05-10T00:00:00Z', message: 'v2', operation: 'modify', files: {} },
  { commit: 'aaa1111', date: '2026-05-01T00:00:00Z', message: 'v1', operation: 'add',    files: {} },
];

beforeEach(() => { vi.clearAllMocks(); });

describe('VersionWalker', () => {
  it('renders the 7-char commit chip', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="bbb2222" dispatch={vi.fn()} />);
    expect(screen.getByTestId('walker-commit-chip').textContent).toBe('bbb2222');
  });

  it('renders the total version count when there are multiple versions', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="bbb2222" dispatch={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('walker-version-count').textContent).toContain('3v'));
  });

  it('hides the version count when the fact has only one version', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: [versions[0]] });
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="ccc3333" dispatch={vi.fn()} />);
    // Wait for the fetch to settle (count=1 means the count badge is hidden).
    await waitFor(() => {
      const walker = screen.getByTestId('version-walker');
      expect(walker.textContent).not.toContain('v');  // no Nv suffix
    });
    expect(screen.queryByTestId('walker-version-count')).toBeNull();
  });

  it('clicking the commit chip opens Explain at the fact + commit, no asOf change', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    const dispatch = vi.fn();
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="bbb2222" dispatch={dispatch} />);
    fireEvent.click(screen.getByTestId('walker-commit-chip'));
    expect(dispatch).toHaveBeenCalledWith({ type: 'OPEN_EXPLAIN', path: 'kb/x.md', commit: 'bbb2222' });
    const setAsOfCalls = dispatch.mock.calls.filter(c => c[0].type === 'SET_AS_OF');
    expect(setAsOfCalls).toHaveLength(0);
  });
});
