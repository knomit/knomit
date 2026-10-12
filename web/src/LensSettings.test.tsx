import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';

// `api` and the origin-session streams are stubbed; the module's other exports
// — the description byte caps — pass through from the real module, so a test
// asserting on a cap is asserting on the value the app actually ships.
vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  // The connect sub-page's backend. Only the commit-lock test drives these;
  // everywhere else they exist so the wizard cannot reach the network.
  createSession: vi.fn(),
  streamTest: vi.fn(() => () => {}),
  streamPreview: vi.fn(() => () => {}),
  streamApply: vi.fn(),
  streamCommit: vi.fn(),
  // Resolved, not bare: every caller does deleteSession(...).catch(...), so a
  // bare vi.fn() fails inside a click handler rather than at an assertion.
  deleteSession: vi.fn().mockResolvedValue(undefined),
  api: {
    listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
    // null = this server has no OAuth issuer (404), so no Authorizations tab.
    listOAuthPending: vi.fn().mockResolvedValue(null),
    listArchived: vi.fn().mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
    ]),
    listLenses: vi.fn().mockResolvedValue([
      { name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }] },
    ]),
    getLens: vi.fn().mockResolvedValue({ name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }] }),
    createLens: vi.fn().mockResolvedValue({ name: 'newlens', write: { uid: 'uid-core', name: 'core' }, reads: [] }),
    updateLens: vi.fn().mockResolvedValue({ name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }] }),
    deleteLens: vi.fn().mockResolvedValue(undefined),
    renameLens: vi.fn().mockResolvedValue({ name: 'dev' }),
    listBranchNames: vi.fn().mockResolvedValue([]),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    getRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    updateRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    renameRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    getOrigin: vi.fn().mockResolvedValue(null),
    deleteOrigin: vi.fn(),
    cancelIndex: vi.fn().mockResolvedValue({ name: 'core', uid: 'uid-core', index_state: 'error', index_reason: 'indexing cancelled' }),
    rebuild: vi.fn().mockResolvedValue({ id: 'job1', state: 'running' }),
    restoreRepo: vi.fn().mockResolvedValue({ name: 'old' }),
    purgeRepo: vi.fn().mockResolvedValue(undefined),
    archiveRepo: vi.fn().mockResolvedValue(undefined),
  },
}));

const ARCHIVED_FIXTURE = [
  { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
];

describe('RepoManager', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // clearAllMocks clears CALLS, not implementations, so a test that narrows
    // listArchived would otherwise leak its fixture into every test after it.
    // Re-assert the default so order cannot matter.
    vi.mocked(api.listArchived).mockResolvedValue(ARCHIVED_FIXTURE);
  });

  const baseProps = {
    open: true as const,
    repos: [{ name: 'core', uid: 'uid-core' }, { name: 'work', uid: 'uid-work' }],
    currentRepo: 'core',
    serverReadOnly: false,
    hideRemoteConfig: false,
    onChanged: () => {},
    onBrowse: () => {},
  };

  // Manage lands on Overview — the one screen that answers "which repository
  // needs something". Tests about a repo's settings PAGE therefore pick it out
  // of the rail first, which is the same single click a reader makes.
  async function selectRepo(name = 'core') {
    fireEvent.click(await screen.findByTestId(`repomgr-item-${name}`));
  }

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

    // Delete lives in the Danger zone block and requires a confirm step.
    fireEvent.click(within(screen.getByTestId('block-danger')).getByTestId('lens-delete'));
    fireEvent.click(screen.getByTestId('lens-delete-confirm'));
    await waitFor(() => expect(api.deleteLens).toHaveBeenCalledWith('dev'));
  });

  // Rename shares the Danger zone with Delete — same typed-confirmation
  // friction as the repo control, but the warning is written for a lens: it
  // has no history of its own to lose, and its identity (uid) survives the
  // rename, so mounts and in-flight agent queries are unaffected.
  describe('Lens rename', () => {
    async function selectLens(name = 'dev') {
      fireEvent.click(await screen.findByTestId(`repomgr-lens-${name}`));
    }

    it('puts Rename in the Danger zone block, beside Delete', async () => {
      render(<RepoManager {...baseProps} />);
      await selectLens();
      const danger = await screen.findByTestId('block-danger');
      expect(within(danger).getByTestId('lens-rename-submit')).toBeInTheDocument();
    });

    // The warning must be lens-specific: no "history/facts" language (a lens
    // has none of its own), and it must say the lens's IDENTITY survives the
    // rename — the MCP cursor pin is lens:<uid>, never the name.
    it('warns that MCP endpoint URLs break but the lens keeps its identity', async () => {
      render(<RepoManager {...baseProps} />);
      await selectLens();
      const warning = await screen.findByTestId('lens-rename-warning');
      expect(warning).toHaveTextContent(/MCP/i);
      expect(warning.textContent).toMatch(/lens keeps its identity/i);
      expect(warning.textContent).toMatch(/mounts.*unaffected/i);
    });

    it('keeps Rename disabled until the current name is typed exactly', async () => {
      render(<RepoManager {...baseProps} />);
      await selectLens();

      const submit = await screen.findByTestId('lens-rename-submit');
      expect(submit).toBeDisabled();

      fireEvent.change(screen.getByTestId('lens-rename-input'), { target: { value: 'newdev' } });
      expect(submit).toBeDisabled();

      fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'de' } });
      expect(submit).toBeDisabled();

      fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'dev' } });
      expect(submit).toBeEnabled();
    });

    it('disables Rename when the new name equals the current name', async () => {
      render(<RepoManager {...baseProps} />);
      await selectLens();

      fireEvent.change(screen.getByTestId('lens-rename-input'), { target: { value: 'dev' } });
      fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'dev' } });
      expect(screen.getByTestId('lens-rename-submit')).toBeDisabled();
    });

    it('calls renameLens with the new name and follows the pane to it', async () => {
      (api.renameLens as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'newdev' });
      render(<RepoManager {...baseProps} />);
      await selectLens();

      fireEvent.change(screen.getByTestId('lens-rename-input'), { target: { value: 'newdev' } });
      fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'dev' } });
      fireEvent.click(screen.getByTestId('lens-rename-submit'));

      await waitFor(() => expect(api.renameLens).toHaveBeenCalledWith('dev', 'newdev'));
      // The pane is addressed by name; it must follow the lens to its new one.
      await waitFor(() => expect(screen.getByRole('heading', { level: 3 })).toHaveTextContent('newdev'));
    });

    // The parent learns about the rename too — with BOTH names — so it can
    // re-point a stale "currently browsed lens" selection at the new one
    // instead of treating the old name as though the lens was deleted.
    it('reports the rename to the parent as {from, to}, not a bare refresh', async () => {
      (api.renameLens as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'newdev' });
      const onChanged = vi.fn();
      render(<RepoManager {...baseProps} onChanged={onChanged} />);
      await selectLens();

      fireEvent.change(screen.getByTestId('lens-rename-input'), { target: { value: 'newdev' } });
      fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'dev' } });
      fireEvent.click(screen.getByTestId('lens-rename-submit'));

      await waitFor(() => expect(onChanged).toHaveBeenCalledWith({ from: 'dev', to: 'newdev' }));
    });

    it('surfaces the server detail verbatim on a 409 conflict, without inventing new copy', async () => {
      (api.renameLens as ReturnType<typeof vi.fn>).mockRejectedValue(
        new Error('/api/v1/lenses/dev/rename → 409 name "newdev" is held by another lens or repository'),
      );
      render(<RepoManager {...baseProps} />);
      await selectLens();

      fireEvent.change(screen.getByTestId('lens-rename-input'), { target: { value: 'newdev' } });
      fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'dev' } });
      fireEvent.click(screen.getByTestId('lens-rename-submit'));

      expect(await screen.findByText(/held by another lens or repository/i)).toBeInTheDocument();
    });

    it('is disabled entirely when read-only', async () => {
      render(<RepoManager {...baseProps} serverReadOnly />);
      await selectLens();

      fireEvent.change(screen.getByTestId('lens-rename-input'), { target: { value: 'newdev' } });
      fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'dev' } });
      expect(screen.getByTestId('lens-rename-input')).toBeDisabled();
      expect(screen.getByTestId('lens-rename-confirm')).toBeDisabled();
      expect(screen.getByTestId('lens-rename-submit')).toBeDisabled();
    });
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
    await selectRepo();
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
    expect(writeText).toHaveBeenCalledWith('kb claude init --lens dev');
  });

  // The Agent access card is the only place in the product where host wiring is
  // discoverable, so a shipped host missing a row here is effectively invisible.
  it('offers the Antigravity init command alongside the Claude Code one', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(screen.getByTestId('lens-copy-agy'));
    expect(writeText).toHaveBeenCalledWith('kb antigravity init --lens dev');
  });

  // Without a registered workspace Antigravity silently loads nothing, and the
  // user gets no error — so the card has to say so.
  it('warns that Antigravity needs a registered workspace', async () => {
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    expect(await screen.findByTestId('lens-connect')).toHaveTextContent('--add-dir');
  });

  it('renders the lens note through the same block as a repo description', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }],
      description: '# Dev lens\n\nEngineering read union.',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));

    expect(await screen.findByTestId('repo-description')).toHaveTextContent('Engineering read union.');
  });

  it('orders the lens page note → write target → mounts → access → danger', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }],
      description: 'Engineering read union.',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    await screen.findByTestId('block-note');

    const order = ['block-note', 'block-write-target', 'block-read-mounts', 'block-agent-access', 'block-danger']
      .map(id => screen.getByTestId(id));
    for (let i = 1; i < order.length; i++) {
      expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it('edits mounts, saves via updateLens, and re-renders the new mounts', async () => {
    (api.updateLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' },
      reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }, { uid: 'uid-docs', name: 'docs' }],
    });
    render(<RepoManager {...baseProps} repos={[{ name: 'core', uid: 'uid-core' }, { name: 'work', uid: 'uid-work' }, { name: 'docs', uid: 'uid-docs' }]} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));

    // Enter edit mode, mount 'docs', and save.
    fireEvent.click(screen.getByTestId('lens-edit'));
    fireEvent.click(screen.getByTestId('lens-read-docs'));
    fireEvent.click(screen.getByTestId('lens-edit-save'));

    // Mounts go out as uids — the editor's rows are names, the wire is not.
    await waitFor(() => expect(api.updateLens).toHaveBeenCalledWith('dev', expect.objectContaining({
      reads: expect.arrayContaining([{ uid: 'uid-core', branch: 'main' }, { uid: 'uid-docs' }]),
    })));
    // The write repo is never sent in reads (it is read implicitly).
    const sentReads = (api.updateLens as ReturnType<typeof vi.fn>).mock.calls[0][1].reads;
    expect(sentReads.some((r: { uid: string }) => r.uid === 'uid-work')).toBe(false);
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
    render(<RepoManager {...baseProps} repos={[{ name: 'core', uid: 'uid-core' }, { name: 'work', uid: 'uid-work' }, { name: 'docs', uid: 'uid-docs' }]} />);
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

  // The mount editor lists every repo but the write one. A repo with no live
  // store cannot be mounted — the server answers 422 `repo not found:
  // "<ksuid>"`, naming a uid the reader was never shown — so its checkbox is
  // disabled while it is OFF.
  //
  // It must still be RENDERED, and still toggleable while it is ON. beginEdit
  // seeds editReads from the lens's current reads BY NAME and save re-sends the
  // whole set, so filtering a broken repo out of the list hides the one row
  // that has to be unchecked: the mount that is breaking the lens. Every save
  // would then re-send it and 422 forever, with no control on screen able to
  // repair it.
  describe('the mount editor and a repo with no live store', () => {
    const withBroken = [
      { name: 'core', uid: 'uid-core' },
      { name: 'work', uid: 'uid-work' },
      { name: 'docs', uid: 'uid-docs', state: 'missing', detail: 'database file not found' },
    ];

    it('offers an unmounted broken repo as a chipped, disabled row', async () => {
      render(<RepoManager {...baseProps} repos={withBroken} />);
      await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
      fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
      fireEvent.click(await screen.findByTestId('lens-edit'));

      const box = screen.getByTestId('lens-read-docs');
      expect(box).toBeDisabled();
      // Scoped to the editor row: the repo rail behind the dialog chips it too.
      expect(within(box.parentElement as HTMLElement).getByTestId('repo-state-missing'))
        .toHaveTextContent('missing');

      fireEvent.click(box);
      expect(box).toHaveAttribute('aria-pressed', 'false');
    });

    it('lets an ALREADY-MOUNTED broken repo be unchecked, which is the only repair', async () => {
      (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
        name: 'dev', write: { uid: 'uid-work', name: 'work' },
        reads: [{ uid: 'uid-work', name: 'work' }, { uid: 'uid-docs', name: 'docs' }],
      });
      (api.updateLens as ReturnType<typeof vi.fn>).mockResolvedValue({
        name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }],
      });
      render(<RepoManager {...baseProps} repos={withBroken} />);
      await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
      fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
      fireEvent.click(await screen.findByTestId('lens-edit'));

      const box = await screen.findByTestId('lens-read-docs');
      expect(box).not.toBeDisabled();
      expect(box).toHaveAttribute('aria-pressed', 'true');
      fireEvent.click(box);
      fireEvent.click(screen.getByTestId('lens-edit-save'));

      await waitFor(() => expect(api.updateLens).toHaveBeenCalledWith('dev', expect.objectContaining({ reads: [] })));
    });

    it('refuses Browse into a lens one of whose mounts has no store', async () => {
      (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
        name: 'dev', write: { uid: 'uid-work', name: 'work' },
        reads: [{ uid: 'uid-work', name: 'work' }, { uid: 'uid-docs', name: 'docs' }],
      });
      render(<RepoManager {...baseProps} repos={withBroken} />);
      await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
      fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
      await waitFor(() => expect(screen.getByTestId('lens-browse')).toBeDisabled());
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
    fireEvent.click(await screen.findByTestId('lens-delete'));
    fireEvent.click(await screen.findByTestId('lens-delete-confirm'));
    await waitFor(() => expect(api.deleteLens).toHaveBeenCalled());
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('fires onChanged on lens edit-save', async () => {
    const onChanged = vi.fn();
    render(<RepoManager {...baseProps} onChanged={onChanged} repos={[{ name: 'core', uid: 'uid-core' }, { name: 'work', uid: 'uid-work' }, { name: 'docs', uid: 'uid-docs' }]} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(await screen.findByTestId('lens-edit'));
    fireEvent.click(screen.getByTestId('lens-read-docs'));
    fireEvent.click(screen.getByTestId('lens-edit-save'));
    await waitFor(() => expect(api.updateLens).toHaveBeenCalled());
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });
});
