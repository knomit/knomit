import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    listArchived: vi.fn().mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
    ]),
    listLenses: vi.fn().mockResolvedValue([
      { name: 'dev', write: 'work', reads: [{ repo: 'core', branch: 'main' }, { repo: 'work' }] },
    ]),
    getLens: vi.fn().mockResolvedValue({ name: 'dev', write: 'work', reads: [{ repo: 'core', branch: 'main' }, { repo: 'work' }] }),
    updateLens: vi.fn().mockResolvedValue({ name: 'dev', write: 'work', reads: [{ repo: 'core', branch: 'main' }, { repo: 'work' }] }),
    deleteLens: vi.fn().mockResolvedValue(undefined),
    listBranchNames: vi.fn().mockResolvedValue([]),
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
    hideRemoteConfig: false,
    onClose: () => {},
    onChanged: () => {},
    onBrowse: () => {},
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

  it('lists lenses fetched via api.listLenses', async () => {
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.listLenses).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
  });

  it('shows a lens detail with write/reads and deletes it', async () => {
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());

    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    // Detail pane shows the write target and read mounts (with branch pins).
    expect(screen.getByTestId('lens-detail-write')).toHaveTextContent('work');
    expect(screen.getByTestId('lens-detail-read-core')).toHaveTextContent('main');
    expect(screen.getByTestId('lens-detail-read-work')).toBeInTheDocument();

    // Delete requires a confirm step, then calls api.deleteLens.
    fireEvent.click(screen.getByTestId('lens-delete'));
    fireEvent.click(screen.getByTestId('lens-delete-confirm'));
    await waitFor(() => expect(api.deleteLens).toHaveBeenCalledWith('dev'));
  });

  it('lens Browse button fires onBrowse with the lens context', async () => {
    const onBrowse = vi.fn();
    render(<RepoManager {...baseProps} onBrowse={onBrowse} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(screen.getByTestId('lens-browse'));
    expect(onBrowse).toHaveBeenCalledWith({ kind: 'lens', name: 'dev' });
  });

  it('repo Browse button fires onBrowse with the repo context', async () => {
    const onBrowse = vi.fn();
    render(<RepoManager {...baseProps} onBrowse={onBrowse} />);
    // Detail pane defaults to the current repo (core).
    await waitFor(() => expect(screen.getByTestId('repo-browse')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repo-browse'));
    expect(onBrowse).toHaveBeenCalledWith({ kind: 'repo', repo: 'core' });
  });

  it('does not double-list the write repo in the read mounts', async () => {
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    // The write repo (work) shows exactly once in the mount list, tagged write · read.
    const workRows = screen.getAllByTestId('lens-detail-read-work');
    expect(workRows).toHaveLength(1);
    expect(workRows[0]).toHaveTextContent(/write.*read/i);
  });

  it('copies the init command to the clipboard', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(screen.getByTestId('lens-copy'));
    expect(writeText).toHaveBeenCalledWith('knomit init --lens dev');
  });

  it('renders the lens description markdown when present', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: 'work', reads: [{ repo: 'core', branch: 'main' }, { repo: 'work' }],
      description: '# Dev lens\n\nEngineering read union.',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    await waitFor(() => expect(screen.getByTestId('lens-description')).toHaveTextContent('Engineering read union.'));
  });

  it('edits mounts, saves via updateLens, and re-renders the new mounts', async () => {
    (api.updateLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: 'work',
      reads: [{ repo: 'core', branch: 'main' }, { repo: 'work' }, { repo: 'docs' }],
    });
    render(<RepoManager {...baseProps} repos={[{ name: 'core' }, { name: 'work' }, { name: 'docs' }]} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));

    // Enter edit mode, mount 'docs', and save.
    fireEvent.click(screen.getByTestId('lens-edit'));
    fireEvent.click(screen.getByTestId('lens-read-docs'));
    fireEvent.click(screen.getByTestId('lens-edit-save'));

    await waitFor(() => expect(api.updateLens).toHaveBeenCalledWith('dev', expect.objectContaining({
      reads: expect.arrayContaining([{ repo: 'core', branch: 'main' }, { repo: 'docs' }]),
    })));
    // The write repo is never sent in reads (it is read implicitly).
    const sentReads = (api.updateLens as ReturnType<typeof vi.fn>).mock.calls[0][1].reads;
    expect(sentReads.some((r: { repo: string }) => r.repo === 'work')).toBe(false);
    // Detail re-renders with the returned mount set.
    await waitFor(() => expect(screen.getByTestId('lens-detail-read-docs')).toBeInTheDocument());
  });

  it('opens the New lens form from the Lenses section', async () => {
    render(<RepoManager {...baseProps} />);
    fireEvent.click(screen.getByTestId('repomgr-new-lens'));
    expect(screen.getByTestId('lens-name')).toBeInTheDocument();
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
        onBrowse={() => {}}
      />,
    );
    // RemoteStatus renders a "Remote" section label; assert it is absent.
    expect(screen.queryByText('Remote')).toBeNull();
  });
});
