import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FactHistoryPanel } from './FactHistoryPanel';

vi.mock('./api', () => ({
  api: {
    commitDetail: vi.fn().mockResolvedValue({
      commit: 'a1b2c3d', date: '2026-05-01T00:00:00Z', message: 'Test commit', operation: 'modify',
      author: { name: 'agent-7', email: 'agent-7+learn@agents.knomit.io' },
      files: [{ path: 'kb/x.md', action: 'modified', title: 'X' }],
    }),
    factCommits: vi.fn().mockResolvedValue({
      entries: [
        { commit: 'a1b2c3d', date: '2026-05-01T00:00:00Z', message: 'Test commit', operation: 'modify' },
        { commit: 'zzz9999', date: '2026-04-01T00:00:00Z', message: 'older',       operation: 'add' },
      ],
    }),
  },
}));

beforeEach(() => { vi.clearAllMocks(); });

describe('FactHistoryPanel', () => {
  it('renders commit detail when currentCommit is set', async () => {
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getByTestId('history-op-chip').textContent).toContain('modify'));
    expect(screen.getByTestId('history-message').textContent).toBe('Test commit');
    const author = screen.getByTestId('history-author');
    expect(author.getAttribute('data-kind')).toBe('agent');
    expect(author.textContent).toBe('agent-7');
    expect(author.querySelector('svg')).not.toBeNull();
    expect(screen.getAllByTestId('history-file-row').length).toBe(1);
  });

  it('shows a human kind + name for a non-agent author', async () => {
    const { api } = await import('./api');
    (api.commitDetail as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      commit: 'a1b2c3d', date: '2026-05-01T00:00:00Z', message: 'Test commit', operation: 'modify',
      author: { name: 'knomit', email: 'k@knomit.io' },
      files: [{ path: 'kb/x.md', action: 'modified', title: 'X' }],
    });
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getByTestId('history-author')).not.toBeNull());
    const author = screen.getByTestId('history-author');
    expect(author.getAttribute('data-kind')).toBe('human');
    expect(author.textContent).toBe('knomit');
  });

  it('falls back to the email when the author name is missing', async () => {
    const { api } = await import('./api');
    (api.commitDetail as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      commit: 'a1b2c3d', date: '2026-05-01T00:00:00Z', message: 'Test commit', operation: 'modify',
      author: { name: '', email: 'agent-7+learn@agents.knomit.io' },
      files: [{ path: 'kb/x.md', action: 'modified', title: 'X' }],
    });
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getByTestId('history-author')).not.toBeNull());
    const author = screen.getByTestId('history-author');
    expect(author.getAttribute('data-kind')).toBe('agent');
    expect(author.textContent).toBe('agent-7+learn@agents.knomit.io');
  });

  it('omits the author line when the commit has no author', async () => {
    const { api } = await import('./api');
    (api.commitDetail as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      commit: 'a1b2c3d', date: '2026-05-01T00:00:00Z', message: 'Test commit', operation: 'modify',
      author: { name: '', email: '' },
      files: [{ path: 'kb/x.md', action: 'modified', title: 'X' }],
    });
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getByTestId('history-message').textContent).toBe('Test commit'));
    expect(screen.queryByTestId('history-author')).toBeNull();
  });

  it('omits commit detail when currentCommit is null and the fact has no versions', async () => {
    const { api } = await import('./api');
    (api.factCommits as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ entries: [] });
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit={null}
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => screen.getByTestId('fact-history-panel'));
    expect(screen.queryByTestId('history-op-chip')).toBeNull();
    expect(screen.queryByTestId('history-message')).toBeNull();
  });

  it('falls back to the latest version when the open commit is not one of the fact versions', async () => {
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    // The latest version (a1b2c3d) is selected and its detail is shown,
    // even though the open commit matches no version.
    await waitFor(() => expect(screen.getAllByTestId('history-fact-version').length).toBe(2));
    const rows = screen.getAllByTestId('history-fact-version');
    expect(rows[0].getAttribute('data-current')).toBe('true');
    expect(rows[1].getAttribute('data-current')).toBeNull();
    await waitFor(() => expect(screen.getByTestId('history-op-chip')).not.toBeNull());
  });

  it('renders fact versions and marks the current one as selected', async () => {
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-fact-version').length).toBe(2));
    const rows = screen.getAllByTestId('history-fact-version');
    expect(rows[0].getAttribute('data-current')).toBe('true');
    expect(rows[1].getAttribute('data-current')).toBeNull();
  });

  it('marks the current version when the open commit is a full hash and the row is abbreviated', async () => {
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-fact-version').length).toBe(2));
    const rows = screen.getAllByTestId('history-fact-version');
    expect(rows[0].getAttribute('data-current')).toBe('true');
    expect(rows[1].getAttribute('data-current')).toBeNull();
  });

  it('clicking a version row calls onNavigateToCommit', async () => {
    const onNavigateToCommit = vi.fn();
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={onNavigateToCommit}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-fact-version').length).toBe(2));
    fireEvent.click(screen.getAllByTestId('history-fact-version')[1]);
    expect(onNavigateToCommit).toHaveBeenCalledWith('zzz9999');
  });

  it('clicking a file row whose path is NOT the open fact calls onFileClick', async () => {
    const { api } = await import('./api');
    (api.commitDetail as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      commit: 'a1b2c3d', date: '', message: '', operation: 'modify',
      files: [{ path: 'kb/other.md', action: 'modified', title: 'Other' }],
    });
    const onFileClick = vi.fn();
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={onFileClick}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-file-row').length).toBe(1));
    fireEvent.click(screen.getByTestId('history-file-row'));
    // Navigates to the file AT the commit whose changeset is shown (the
    // viewed/active commit), so the fact provably exists there.
    expect(onFileClick).toHaveBeenCalledWith('kb/other.md', 'a1b2c3d');
  });

  it('navigates a file click to the viewed commit, not the open commit, when they differ', async () => {
    const { api } = await import('./api');
    // The fact's only version is a1b2c3d, but we open at an unrelated commit
    // (deadbeef…). The panel falls back to a1b2c3d and shows ITS changeset, so
    // a file click must navigate to a1b2c3d — navigating to deadbeef… would
    // 404 because the sibling file does not exist at that commit.
    const siblingChangeset = {
      commit: 'a1b2c3d', date: '', message: '', operation: 'add',
      files: [{ path: 'kb/sibling.md', action: 'added', title: 'Sibling' }],
    };
    // Two fetches: the open commit, then the fallback to the latest version.
    (api.commitDetail as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce(siblingChangeset)
      .mockResolvedValueOnce(siblingChangeset);
    const onFileClick = vi.fn();
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
      onNavigateToCommit={vi.fn()}
      onFileClick={onFileClick}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-file-row').length).toBe(1));
    fireEvent.click(screen.getByTestId('history-file-row'));
    expect(onFileClick).toHaveBeenCalledWith('kb/sibling.md', 'a1b2c3d');
  });

  it('disables the file row whose path matches the open fact (no self-navigation)', async () => {
    const onFileClick = vi.fn();
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={onFileClick}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-file-row').length).toBe(1));
    const row = screen.getByTestId('history-file-row');
    expect(row).toBeDisabled();
    expect(row.getAttribute('data-self')).toBe('true');
    fireEvent.click(row);
    expect(onFileClick).not.toHaveBeenCalled();
  });
});
