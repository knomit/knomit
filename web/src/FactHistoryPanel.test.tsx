import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FactHistoryPanel } from './FactHistoryPanel';

vi.mock('./api', () => ({
  api: {
    commitDetail: vi.fn().mockResolvedValue({
      commit: 'a1b2c3d', date: '2026-05-01T00:00:00Z', message: 'Test commit', operation: 'modify',
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
    expect(screen.getAllByTestId('history-file-row').length).toBe(1);
  });

  it('omits commit detail when currentCommit is null', async () => {
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

  it('renders fact versions and marks the current one with the amber dot', async () => {
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onFileClick={vi.fn()}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-fact-version').length).toBe(2));
    const rows = screen.getAllByTestId('history-fact-version');
    expect(rows[0].querySelector('[data-testid="history-current-dot"]')).not.toBeNull();
    expect(rows[1].querySelector('[data-testid="history-current-dot"]')).toBeNull();
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
    expect(onFileClick).toHaveBeenCalledWith('kb/other.md');
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
