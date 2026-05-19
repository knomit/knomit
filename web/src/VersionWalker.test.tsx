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
  it('renders vN of M and a clickable commit chip', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="bbb2222" dispatch={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('version-walker').textContent).toContain('v2 of 3'));
    expect(screen.getByTestId('walker-commit-chip').textContent).toBe('bbb2222');
  });

  it('prev dispatches SET_AS_OF with previous commit', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    const dispatch = vi.fn();
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="bbb2222" dispatch={dispatch} />);
    await waitFor(() => screen.getByTestId('walker-prev'));
    fireEvent.click(screen.getByTestId('walker-prev'));
    // entries are newest-first, so prev (older) = aaa1111
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_AS_OF', asOf: { mode: 'scrubbed', commit: 'aaa1111' } });
  });

  it('next dispatches SET_AS_OF live when reaching HEAD (newest)', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    const dispatch = vi.fn();
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="bbb2222" dispatch={dispatch} />);
    await waitFor(() => screen.getByTestId('walker-next'));
    fireEvent.click(screen.getByTestId('walker-next'));
    // newer than bbb2222 is ccc3333 which is the head/latest → live
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_AS_OF', asOf: { mode: 'live' } });
  });

  it('clicking the commit chip opens the drawer without changing asOf', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    const dispatch = vi.fn();
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="bbb2222" dispatch={dispatch} />);
    await waitFor(() => screen.getByTestId('walker-commit-chip'));
    fireEvent.click(screen.getByTestId('walker-commit-chip'));
    expect(dispatch).toHaveBeenCalledWith({ type: 'OPEN_COMMIT_DRAWER', commit: 'bbb2222' });
    // No SET_AS_OF dispatch
    const setAsOfCalls = dispatch.mock.calls.filter(c => c[0].type === 'SET_AS_OF');
    expect(setAsOfCalls).toHaveLength(0);
  });

  it('disables prev when at oldest version', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="aaa1111" dispatch={vi.fn()} />);
    await waitFor(() => screen.getByTestId('walker-prev'));
    expect(screen.getByTestId('walker-prev')).toBeDisabled();
  });

  it('disables next when at newest (HEAD)', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="ccc3333" dispatch={vi.fn()} />);
    await waitFor(() => screen.getByTestId('walker-next'));
    expect(screen.getByTestId('walker-next')).toBeDisabled();
  });

  it('shows "v? of N" when currentCommit is not in the loaded versions', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValue({ entries: versions });
    render(<VersionWalker repo="r" branch="b" factPath="kb/x.md" currentCommit="not-loaded" dispatch={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('version-walker').textContent).toContain('v? of 3'));
  });
});
