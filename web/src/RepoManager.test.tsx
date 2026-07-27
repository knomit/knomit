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
    createLens: vi.fn().mockResolvedValue({ name: 'newlens', write: 'core', reads: [] }),
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
    // The Remote status card renders inline within the same dialog.
    await waitFor(() => expect(screen.getByText('Remote')).toBeInTheDocument());
    expect(screen.getByText('Rebuild index')).toBeInTheDocument();
  });

  // The detail pane leads with state (agent branch, remote) and pushes
  // reference material behind disclosures — Connect an agent must not render
  // its snippets until asked.
  it('collapses "Connect an agent" by default and expands on click', async () => {
    render(<RepoManager {...baseProps} />);
    const toggle = await screen.findByTestId('repo-connect-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByTestId('repo-copy')).not.toBeInTheDocument();

    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('repo-copy')).toBeInTheDocument();
  });

  it('orders the repo pane as branch → remote → description → connect', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: 'Root manifest.' });
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repo-description-toggle');

    // getOrigin resolves null here, so the Remote card shows its empty state.
    const order = ['repo-detail-branch', 'remote-connect', 'repo-description-toggle', 'repo-connect-toggle']
      .map(id => screen.getByTestId(id));
    // Remote must sit below the repo's own info, and both above the disclosures.
    for (let i = 1; i < order.length; i++) {
      expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it('disconnects a remote from the ⋯ menu, behind a confirm', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    (api.deleteOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<RepoManager {...baseProps} onChanged={onChanged} />);

    fireEvent.click(await screen.findByTestId('repo-menu'));
    fireEvent.click(screen.getByTestId('remote-disconnect'));
    // Selecting the item closes the menu and asks first — no request yet.
    expect(api.deleteOrigin).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId('disconnect-confirm'));
    await waitFor(() => expect(api.deleteOrigin).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('closes the ⋯ menu on an outside click', async () => {
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repo-menu'));
    expect(screen.getByTestId('repo-archive')).toBeInTheDocument();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByTestId('repo-archive')).not.toBeInTheDocument();
  });

  it('renders the kb.md description in the detail pane', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', description: '# Knowledge Base\n\nRoot manifest.',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    fireEvent.click(await screen.findByTestId('repo-description-toggle'));
    expect(screen.getByTestId('repo-description')).toHaveTextContent('Root manifest.');
  });

  it('renders GFM in the kb.md description — a table, not literal pipe text', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core',
      description: '# KB\n\n| Topic | Meaning |\n|---|---|\n| invariants | violate this and it breaks |',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));

    fireEvent.click(await screen.findByTestId('repo-description-toggle'));
    const desc = screen.getByTestId('repo-description');
    expect(desc.querySelector('table')).not.toBeNull();
    expect(desc.querySelectorAll('th')).toHaveLength(2);
    expect(desc.textContent).not.toContain('|---|');
    // The prose class sits on the inner scroll container that wraps the markdown.
    expect(desc.querySelector('.k-prose')).not.toBeNull();
  });

  it('omits the description block when the repo has no kb.md', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(screen.queryByTestId('repo-description-toggle')).not.toBeInTheDocument();
    expect(screen.queryByTestId('repo-description')).not.toBeInTheDocument();
  });

  it('collapses the description by default and expands it on click', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', description: 'line\n'.repeat(40),
    });
    render(<RepoManager {...baseProps} />);
    const toggle = await screen.findByTestId('repo-description-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByTestId('repo-description')).not.toBeInTheDocument();

    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('repo-description')).toBeInTheDocument();

    fireEvent.click(toggle);
    expect(screen.queryByTestId('repo-description')).not.toBeInTheDocument();
  });

  it('rebuild gives immediate feedback and a completion message', async () => {
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByText('Rebuild index')).toBeInTheDocument());

    fireEvent.click(screen.getByText('Rebuild index'));
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

    // Delete lives in the ⋯ menu and requires a confirm step.
    fireEvent.click(screen.getByTestId('lens-menu'));
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
    fireEvent.click(screen.getByTestId('lens-connect-toggle'));
    fireEvent.click(screen.getByTestId('lens-copy'));
    expect(writeText).toHaveBeenCalledWith('knomit-bridge claude init --lens dev');
  });

  it('renders the lens description behind the same disclosure as a repo', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: 'work', reads: [{ repo: 'core', branch: 'main' }, { repo: 'work' }],
      description: '# Dev lens\n\nEngineering read union.',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));

    const toggle = await screen.findByTestId('repo-description-toggle');
    expect(screen.queryByTestId('repo-description')).not.toBeInTheDocument();
    fireEvent.click(toggle);
    expect(screen.getByTestId('repo-description')).toHaveTextContent('Engineering read union.');
  });

  it('orders the lens pane as write → mounts → description → connect', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: 'work', reads: [{ repo: 'core', branch: 'main' }, { repo: 'work' }],
      description: 'Engineering read union.',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    await screen.findByTestId('repo-description-toggle');

    const order = ['lens-detail-write', 'lens-detail-read-core', 'repo-description-toggle', 'lens-connect-toggle']
      .map(id => screen.getByTestId(id));
    for (let i = 1; i < order.length; i++) {
      expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
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

  it('edit dropdown filters each read repo against its OWN agent branch, not the write repo', async () => {
    // docs' own agent branch is 'agent/x'; the write repo (work) resolves to 'agent/w'.
    (api.getAgentBranch as ReturnType<typeof vi.fn>).mockImplementation((repo: string) =>
      Promise.resolve(repo === 'docs' ? 'agent/x' : 'agent/w'));
    // docs carries a real branch literally named after the write repo's agent branch.
    (api.listBranchNames as ReturnType<typeof vi.fn>).mockImplementation((repo: string) =>
      Promise.resolve(repo === 'docs' ? ['agent/x', 'main', 'agent/w'] : []));
    render(<RepoManager {...baseProps} repos={[{ name: 'core' }, { name: 'work' }, { name: 'docs' }]} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));

    fireEvent.click(screen.getByTestId('lens-edit'));
    fireEvent.click(screen.getByTestId('lens-read-docs'));

    // Explicit-pin options exclude docs' own agent branch ('agent/x' is the
    // default option) but keep a real branch that merely shares the write
    // repo's agent-branch name ('agent/w').
    // Retry until both branch names and docs' agent branch have resolved.
    await waitFor(() => {
      const opts = Array.from(screen.getByTestId('lens-branch-docs').querySelectorAll('option')).map(o => o.getAttribute('value'));
      expect(opts).toContain('main');
      expect(opts).toContain('agent/w');
      expect(opts).not.toContain('agent/x');
    });
  });

  it('opens the New lens form from the Lenses section', async () => {
    render(<RepoManager {...baseProps} />);
    fireEvent.click(screen.getByTestId('repomgr-new-lens'));
    expect(screen.getByTestId('lens-name')).toBeInTheDocument();
  });

  // onChanged is the ONLY hook that refreshes App's lens list feeding the TopBar
  // switcher — the local refresh() only updates this dialog. All three lens
  // mutation paths (create/delete/save) must fire it so the switcher stays live.
  it('fires onChanged on lens create (so the app lens list / TopBar refreshes)', async () => {
    const onChanged = vi.fn();
    render(<RepoManager {...baseProps} onChanged={onChanged} />);
    fireEvent.click(screen.getByTestId('repomgr-new-lens'));
    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'newlens' } });
    fireEvent.click(screen.getByTestId('lens-create'));
    await waitFor(() => expect(api.createLens).toHaveBeenCalled());
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('fires onChanged on lens delete', async () => {
    const onChanged = vi.fn();
    render(<RepoManager {...baseProps} onChanged={onChanged} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(await screen.findByTestId('lens-menu'));
    fireEvent.click(await screen.findByTestId('lens-delete'));
    fireEvent.click(await screen.findByTestId('lens-delete-confirm'));
    await waitFor(() => expect(api.deleteLens).toHaveBeenCalled());
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('fires onChanged on lens edit-save', async () => {
    const onChanged = vi.fn();
    render(<RepoManager {...baseProps} onChanged={onChanged} repos={[{ name: 'core' }, { name: 'work' }, { name: 'docs' }]} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(await screen.findByTestId('lens-edit'));
    fireEvent.click(screen.getByTestId('lens-read-docs'));
    fireEvent.click(screen.getByTestId('lens-edit-save'));
    await waitFor(() => expect(api.updateLens).toHaveBeenCalled());
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
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
