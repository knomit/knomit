import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    listArchived: vi.fn().mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
    ]),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    getRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    getOrigin: vi.fn().mockResolvedValue(null),
    deleteOrigin: vi.fn(),
    rebuild: vi.fn().mockResolvedValue({ id: 'job1', state: 'running' }),
  },
}));

describe('RepoManager', () => {
  beforeEach(() => vi.clearAllMocks());

  const baseProps = {
    open: true as const,
    repos: [{ name: 'core' }, { name: 'work' }],
    currentRepo: 'core',
    readOnly: false,
    onClose: () => {},
    onChanged: () => {},
  };

  it('lists active repos and the archived list', async () => {
    render(<RepoManager {...baseProps} />);
    expect(screen.getByTestId('repomgr-item-core')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-item-work')).toBeInTheDocument();
    await waitFor(() => expect(api.listArchived).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText('old')).toBeInTheDocument());
  });

  it('auto-selects the current repo and shows its detail pane (origin inline, no modal)', async () => {
    render(<RepoManager {...baseProps} />);
    // RepoDetail for core loads its agent branch and inline origin form.
    await waitFor(() => expect(api.getAgentBranch).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(api.getOrigin).toHaveBeenCalledWith('core'));
    // The Remote status section renders inline within the same dialog.
    await waitFor(() => expect(screen.getByText('Remote')).toBeInTheDocument());
    expect(screen.getByText('⟳ Rebuild index')).toBeInTheDocument();
  });

  it('renders the kb.md description in the detail pane', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', description: '# Knowledge Base\n\nRoot manifest.',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(screen.getByTestId('repo-description')).toHaveTextContent('Root manifest.'));
  });

  it('omits the description block when the repo has no kb.md', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(screen.queryByTestId('repo-description')).not.toBeInTheDocument();
  });

  it('expands a long description via the Show more toggle', async () => {
    // jsdom does no layout, so fake the clamp overflow: scrollHeight > clientHeight.
    const sh = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollHeight');
    const ch = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientHeight');
    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', { configurable: true, get: () => 500 });
    Object.defineProperty(HTMLElement.prototype, 'clientHeight', { configurable: true, get: () => 132 });
    try {
      (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
        name: 'core', description: 'line\n'.repeat(40),
      });
      render(<RepoManager {...baseProps} />);
      const toggle = await screen.findByTestId('repo-description-toggle');
      expect(toggle).toHaveTextContent('Show more');
      fireEvent.click(toggle);
      expect(toggle).toHaveTextContent('Show less');
    } finally {
      if (sh) Object.defineProperty(HTMLElement.prototype, 'scrollHeight', sh);
      if (ch) Object.defineProperty(HTMLElement.prototype, 'clientHeight', ch);
    }
  });

  it('rebuild gives immediate feedback and a completion message', async () => {
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByText('⟳ Rebuild index')).toBeInTheDocument());

    fireEvent.click(screen.getByText('⟳ Rebuild index'));
    await waitFor(() => expect(api.rebuild).toHaveBeenCalledWith('core', 'agent/test'));
    // Visible confirmation that the background rebuild kicked off (the bug: none).
    await waitFor(() => expect(screen.getByTestId('rebuild-status')).toHaveTextContent('Rebuild started'));
  });

  it('hideRemoteConfig hides the remote status panel', async () => {
    // First verify the panel IS present when hideRemoteConfig is false (non-vacuity check).
    const { unmount } = render(<RepoManager {...baseProps} hideRemoteConfig={false} />);
    await waitFor(() => expect(screen.getByText('Remote')).toBeInTheDocument());
    unmount();

    // Now render with hideRemoteConfig=true and assert the panel is absent.
    render(
      <RepoManager
        open
        repos={[{ name: 'core' }]}
        currentRepo="core"
        readOnly
        hideRemoteConfig
        onClose={() => {}}
        onChanged={() => {}}
      />,
    );
    // RemoteStatus renders a "Remote" section label; assert it is absent.
    expect(screen.queryByText('Remote')).toBeNull();
  });
});
