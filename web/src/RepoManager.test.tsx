import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    listArchived: vi.fn().mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
    ]),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    getOrigin: vi.fn().mockResolvedValue(null),
  },
}));

describe('RepoManager', () => {
  beforeEach(() => vi.clearAllMocks());

  const baseProps = {
    open: true as const,
    repos: [{ name: 'trunk' }, { name: 'work' }],
    currentRepo: 'trunk',
    readOnly: false,
    onClose: () => {},
    onChanged: () => {},
    onSelect: () => {},
    onConnectAdvanced: () => {},
  };

  it('lists active repos and the archived list', async () => {
    render(<RepoManager {...baseProps} />);
    expect(screen.getByTestId('repomgr-item-trunk')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-item-work')).toBeInTheDocument();
    await waitFor(() => expect(api.listArchived).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText('old')).toBeInTheDocument());
  });

  it('auto-selects the current repo and shows its detail pane (origin inline, no modal)', async () => {
    render(<RepoManager {...baseProps} />);
    // RepoDetail for trunk loads its agent branch and inline origin form.
    await waitFor(() => expect(api.getAgentBranch).toHaveBeenCalledWith('trunk'));
    await waitFor(() => expect(api.getOrigin).toHaveBeenCalledWith('trunk'));
    // The Origin section renders inline within the same dialog.
    await waitFor(() => expect(screen.getByText('Origin')).toBeInTheDocument());
    expect(screen.getByText('⟳ Rebuild index')).toBeInTheDocument();
  });
});
