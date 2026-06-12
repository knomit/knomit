import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    listArchived: vi.fn().mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
    ]),
  },
}));

describe('RepoManager', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders active repos and archived list', async () => {
    render(
      <RepoManager
        open
        repos={[{ name: 'trunk' }, { name: 'work' }]}
        currentRepo="trunk"
        readOnly={false}
        onClose={() => {}}
        onChanged={() => {}}
        onSelect={() => {}}
      />,
    );
    expect(screen.getByText('trunk')).toBeInTheDocument();
    expect(screen.getByText('work')).toBeInTheDocument();
    await waitFor(() => expect(api.listArchived).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText('old')).toBeInTheDocument());
  });
});
