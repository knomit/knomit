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
      onClose={vi.fn()}
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
      onClose={vi.fn()}
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
      onClose={vi.fn()}
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
      onClose={vi.fn()}
    />);
    await waitFor(() => expect(screen.getAllByTestId('history-fact-version').length).toBe(2));
    fireEvent.click(screen.getAllByTestId('history-fact-version')[1]);
    expect(onNavigateToCommit).toHaveBeenCalledWith('zzz9999');
  });

  it('clicking the close button calls onClose', async () => {
    const onClose = vi.fn();
    render(<FactHistoryPanel
      repo="r" branch="b" factPath="kb/x.md"
      currentCommit="a1b2c3d"
      onNavigateToCommit={vi.fn()}
      onClose={onClose}
    />);
    fireEvent.click(screen.getByTestId('history-panel-close'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
