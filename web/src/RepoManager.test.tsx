import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api, MAX_LENS_DESCRIPTION_BYTES } from './api';

// Only `api` is stubbed. The module's other exports — the description byte
// caps — pass through from the real module, so a test asserting on a cap is
// asserting on the value the app actually ships.
vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
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
    listBranchNames: vi.fn().mockResolvedValue([]),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    getRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    updateRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    getOrigin: vi.fn().mockResolvedValue(null),
    deleteOrigin: vi.fn(),
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
    readOnly: false,
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

  it('lists active repos, with the archive as one row beneath them', async () => {
    render(<RepoManager {...baseProps} />);
    expect(screen.getByTestId('repomgr-item-core')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-item-work')).toBeInTheDocument();
    await waitFor(() => expect(api.listArchived).toHaveBeenCalled());

    // One row carrying a count, at the foot of the repositories — an archived
    // repo is a repository in a state, not a third kind of thing. No children:
    // they all share one page.
    const row = await screen.findByTestId('repomgr-archived');
    expect(row.textContent).toContain('Archived');
    expect(row.textContent).toContain('1');
    expect(screen.queryByText('old')).not.toBeInTheDocument();
  });

  it('renders no Archived row at all when nothing is archived', async () => {
    vi.mocked(api.listArchived).mockResolvedValue([]);
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.listArchived).toHaveBeenCalled());
    // Not "Archived 0", not a "None" line — there is nothing to open, and a
    // dead control is worse than an absent one.
    expect(screen.queryByTestId('repomgr-archived')).not.toBeInTheDocument();
    expect(screen.queryByText('Archived')).not.toBeInTheDocument();
  });

  // An archived repo carries three lines — a date, an origin and two buttons —
  // so one page holds all of them and the contents rail is the per-repo index.
  // A rail entry each would have been a click that buys almost nothing.
  it('puts every archived repo on one page, indexed by the contents rail', async () => {
    vi.mocked(api.listArchived).mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
      { id: 'older.2', name: 'older', origin: 'git@example.com:me/older.git', archivedAt: '2026-05-01T00:00:00Z' },
    ]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-archived'));

    // Both on screen at once, each with its own actions.
    expect(await screen.findByTestId('block-archived-old.1')).toBeInTheDocument();
    expect(screen.getByTestId('block-archived-older.2')).toBeInTheDocument();
    expect(screen.getByTestId('archived-restore-old.1')).toBeInTheDocument();
    expect(screen.getByTestId('archived-purge-older.2')).toBeInTheDocument();
    // …and the rail indexes them by name.
    expect(screen.getByTestId('toc-archived-old.1').textContent).toContain('old');
    expect(screen.getByTestId('toc-archived-older.2').textContent).toContain('older');
  });

  // Purging the last one takes the Archived rail row away with it, so leaving
  // the selection where it was left you on an "Archived · 0 repositories" page
  // that nothing in the rail was highlighting.
  // An archived database keeps its full size on disk under a filename derived
  // from its uid, so there is no directory a user could open to work out what
  // archiving is costing them. This figure is the only place it is visible, and
  // it belongs beside the button that reclaims it.
  it('shows what purging an archived repo would give back', async () => {
    vi.mocked(api.listArchived).mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z', sizeBytes: 3_145_728 },
      { id: 'older.2', name: 'older', origin: '', archivedAt: '2026-05-01T00:00:00Z' },
    ]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-archived'));

    expect(await screen.findByTestId('archived-size-old.1')).toHaveTextContent('3.0 MB');
    // An older server sends no size. Nothing is rendered rather than a
    // confident "0 B", which would read as "purging this frees nothing".
    expect(screen.queryByTestId('archived-size-older.2')).toBeNull();
  });

  it('leaves the archive page when the last archived repo is purged', async () => {
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-archived'));
    fireEvent.click(await screen.findByTestId('archived-purge-old.1'));

    fireEvent.change(screen.getByTestId('purge-confirm-input-old.1'), { target: { value: 'old' } });
    vi.mocked(api.listArchived).mockResolvedValue([]);
    fireEvent.click(screen.getByTestId('purge-confirm-old.1'));

    await waitFor(() => expect(api.purgeRepo).toHaveBeenCalledWith('old.1'));
    // The row is gone, and so is the page it opened — we are back on Overview,
    // the fallback selection.
    await waitFor(() => expect(screen.queryByTestId('repomgr-archived')).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByTestId('manage-overview')).toBeInTheDocument());
  });

  it('restores from the archive page and lands on the restored repo', async () => {
    const onChanged = vi.fn();
    vi.mocked(api.restoreRepo).mockResolvedValue({ name: 'old' });
    render(<RepoManager {...baseProps} onChanged={onChanged} />);
    fireEvent.click(await screen.findByTestId('repomgr-archived'));
    fireEvent.click(await screen.findByTestId('archived-restore-old.1'));

    await waitFor(() => expect(api.restoreRepo).toHaveBeenCalledWith('old.1', ''));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  // Caught in the browser, not by a test: the settings PAGE is routinely taller
  // than its column, so without this you land halfway down the next entity, at
  // whatever offset the last one happened to leave behind. The old boxed pane
  // was rarely tall enough for anyone to notice.
  it('returns the detail column to the top when the selection changes', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await screen.findByTestId('repo-detail-branch');

    // The detail COLUMN, by test id — closest('section') would find the nearest
    // block section instead, since every block is a <section> too.
    const column = screen.getByTestId('manage-detail');
    column.scrollTop = 400;
    fireEvent.click(screen.getByTestId('repomgr-item-work'));

    await waitFor(() => expect(column.scrollTop).toBe(0));
  });

  // Reported from the browser: click "Index" in the contents rail, Index scrolls
  // into view, and then whatever sits at the TOP of the page takes the
  // highlight. Two writers were racing for one value and the scroll-spy won —
  // an inference overruling an instruction. Worse, the blocks near the end can
  // never reach the top of the pane, so no amount of scrolling could ever have
  // agreed with the click.
  //
  // NOTE these two pin the CONTRACT, not the original symptom: jsdom has no
  // layout and stubs IntersectionObserver to a no-op, so the old code passed
  // them too. The regression itself is only observable in a real browser.
  it('keeps a contents-rail entry selected after you click it', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await screen.findByTestId('toc-index');

    fireEvent.click(screen.getByTestId('toc-index'));
    expect(screen.getByTestId('toc-index')).toHaveAttribute('aria-current', 'true');
    // And nothing else claims it.
    expect(screen.getByTestId('toc-agent-branch')).not.toHaveAttribute('aria-current');
    expect(screen.getByTestId('toc-danger')).not.toHaveAttribute('aria-current');

    // The pin survives the scroll its own smooth animation fires. Listening for
    // `scroll` to release it would have cancelled it a frame after it was set.
    fireEvent.scroll(screen.getByTestId('manage-detail'));
    expect(screen.getByTestId('toc-index')).toHaveAttribute('aria-current', 'true');
  });

  // The spy used to require the column to be overflowing ALREADY before it
  // attached anything, so a page that happened to fit at mount got no listeners
  // for the rest of its life — and a lens page that then grew (opening the read
  // mounts editor, a long note arriving) was left with a rail frozen on its
  // first block. The scroll container is the nearest auto/scroll ancestor
  // whether or not it has anything to scroll yet.
  //
  // NOTE jsdom has no layout, so every rect is zero and `compute` reads that as
  // "scrolled to the end" — which is why the LAST entry is the one that ends up
  // current. What this pins is that a listener is attached at all.
  it('tracks scrolling on a page that does not overflow yet', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await screen.findByTestId('toc-danger');

    fireEvent.scroll(screen.getByTestId('manage-detail'));
    await waitFor(() => expect(screen.getByTestId('toc-danger')).toHaveAttribute('aria-current', 'true'));
  });

  it('releases the selection when you scroll under your own steam', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    fireEvent.click(await screen.findByTestId('toc-danger'));
    expect(screen.getByTestId('toc-danger')).toHaveAttribute('aria-current', 'true');

    // A wheel is an instruction too — the pin holds against the smooth scroll
    // it triggered itself, not against the reader taking over.
    fireEvent.wheel(window);
    await waitFor(() => expect(screen.getByTestId('toc-danger')).not.toHaveAttribute('aria-current'));
  });

  // With zero repositories the create form is the FALLBACK selection, not
  // something you clicked, so the rail's disabled `+` never gets a chance to
  // stop you. Read-only used to land straight on a live form whose submit would
  // be refused, with nothing on screen saying why.
  it('explains itself instead of offering a create form it cannot submit', async () => {
    render(<RepoManager {...baseProps} repos={[]} currentRepo="" readOnly />);

    expect(await screen.findByTestId('create-blocked-repository')).toBeInTheDocument();
    expect(screen.queryByTestId('create-name')).not.toBeInTheDocument();
  });

  // The same hole from the other direction: a selection made while live
  // survives into a history excursion, which is also read-only.
  it('blocks a create form the selection carried into a read-only state', async () => {
    const { rerender } = render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-new'));
    expect(await screen.findByTestId('create-name')).toBeInTheDocument();

    rerender(<RepoManager {...baseProps} readOnly />);
    expect(screen.queryByTestId('create-name')).not.toBeInTheDocument();
    expect(screen.getByTestId('create-blocked-repository')).toBeInTheDocument();
  });

  it('shows a repo detail pane when one is picked from the rail', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    // RepoDetail for core loads its agent branch and its origin.
    await waitFor(() => expect(api.getAgentBranch).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(api.getOrigin).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(screen.getByTestId('repo-detail-branch')).toBeInTheDocument());
    // The ⋯ overflow is gone: each of its items now lives in the block that
    // owns it, visible without opening anything.
    expect(screen.queryByTestId('repo-menu')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-rebuild')).toBeInTheDocument();
    expect(screen.getByTestId('repo-archive')).toBeInTheDocument();
  });

  // A registered repo with no live store. Manage is the ONLY surface that still
  // shows it — the browse side refuses to open it — so the rail must keep it,
  // and its page must explain rather than fail.
  describe('a repo with no live store', () => {
    const withBroken = [
      { name: 'core', uid: 'uid-core' },
      { name: 'ghost', uid: 'uid-ghost', state: 'missing', detail: 'database file not found' },
    ];

    it('keeps it in the rail, chipped with the reason', async () => {
      render(<RepoManager {...baseProps} repos={withBroken} />);
      const row = await screen.findByTestId('repomgr-item-ghost');
      const chip = within(row).getByTestId('repo-state-missing');
      expect(chip).toHaveTextContent('missing');
      expect(chip).toHaveAttribute('title', 'database file not found');
      // A healthy repo carries no chip — a badge on every row saying "fine"
      // would drown the one row where the answer matters.
      expect(within(screen.getByTestId('repomgr-item-core')).queryByTestId('repo-state-missing')).toBeNull();
    });

    it('explains itself instead of loading a settings page that would 409', async () => {
      render(<RepoManager {...baseProps} repos={withBroken} />);
      fireEvent.click(await screen.findByTestId('repomgr-item-ghost'));

      const pane = await screen.findByTestId('repo-unavailable-ghost');
      expect(within(pane).getByTestId('repo-unavailable-detail')).toHaveTextContent('database file not found');
      // Advice must name something the product can actually do. Putting the
      // file back and restarting is the whole of it — there is no supported
      // route to remove the registration, and the pane says so rather than
      // sending the reader hunting for a button that does not exist.
      expect(pane.textContent).toContain('Put the file back');
      expect(pane.textContent).not.toContain('purge the registration');
      expect(within(pane).getByTestId('repo-unavailable-no-removal').textContent)
        .toContain('not supported yet');
      // No settings page, and above all no Archive/Rebuild buttons: every one
      // of them resolves through the store this repo does not have.
      expect(screen.queryByTestId('repo-detail-branch')).not.toBeInTheDocument();
      expect(screen.queryByTestId('repo-archive')).not.toBeInTheDocument();
      expect(api.getAgentBranch).not.toHaveBeenCalledWith('ghost');
      expect(api.getOrigin).not.toHaveBeenCalledWith('ghost');
    });

    it('reports its state in the Overview table without fetching it', async () => {
      render(<RepoManager {...baseProps} repos={withBroken} />);
      const row = await screen.findByTestId('fleet-row-ghost');
      expect(within(row).getByTestId('repo-state-missing')).toBeInTheDocument();
      expect(row.textContent).toContain('no store open');
      // "no remote configured" would be a claim about a repo we never opened,
      // and its Connect button would send the reader somewhere useless.
      expect(screen.queryByTestId('attention-ghost')).not.toBeInTheDocument();
      await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
      expect(api.getRepo).not.toHaveBeenCalledWith('ghost');
    });
  });

  // Zero repos is an ordinary state (fresh install, or the last repo was
  // archived). currentRepo is "" then, so falling back to the repo detail pane
  // would query a nameless repo; the create form is the only useful landing.
  it('opens on the create form when there are no repos', async () => {
    render(<RepoManager {...baseProps} repos={[]} currentRepo="" />);
    expect(screen.getByTestId('create-name')).toBeInTheDocument();
    await waitFor(() => expect(api.listArchived).toHaveBeenCalled());
    expect(api.getAgentBranch).not.toHaveBeenCalled();
    expect(api.getRepo).not.toHaveBeenCalled();
  });

  // "Not connected" is STATE, so the Remote block renders it and carries the
  // action that changes it — rather than the block vanishing and the offer
  // hiding in an overflow menu, which is what a boxed pane forced.
  it('says so in the Remote block when unconnected, with Connect on it', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await waitFor(() => expect(api.getOrigin).toHaveBeenCalledWith('core'));
    const block = await screen.findByTestId('block-remote');
    expect(block.textContent).toContain('Not connected');
    expect(within(block).getByTestId('remote-connect')).toBeInTheDocument();
  });

  it('shows the remote state when connected, and drops the Connect offer', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await waitFor(() => expect(screen.getByTestId('sync-line')).toBeInTheDocument());
    expect(screen.queryByTestId('remote-connect')).not.toBeInTheDocument();
  });

  // Nothing folds any more. In a boxed dialog the connect snippets had to hide
  // behind a disclosure to leave room for state; the mode has the room, and a
  // fold is a click on the way to the thing you came for.
  it('renders the agent-access snippets without anything to expand', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    expect(await screen.findByTestId('repo-copy')).toBeInTheDocument();
    expect(screen.getByTestId('repo-copy-mcp')).toBeInTheDocument();
    expect(screen.queryByTestId('repo-connect-toggle')).not.toBeInTheDocument();
  });

  // Blocks run identity → wiring → operations → danger, which is also the rule
  // for where a NEW setting goes on a page that has no tabs to reorganise.
  it('orders the repo page identity → wiring → operations → danger', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: 'Root manifest.', license: 'MIT License' });
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await screen.findByTestId('block-license');

    const order = ['block-description', 'block-license', 'block-agent-branch', 'block-remote',
      'block-agent-access', 'block-index', 'block-danger'].map(id => screen.getByTestId(id));
    for (let i = 1; i < order.length; i++) {
      expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  // The contents rail is an index, not a nav: every block is already on the
  // page, so it lists them all rather than gating any behind a selection.
  it('lists every block in the contents rail', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: 'Root manifest.' });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await screen.findByTestId('toc-description');
    for (const id of ['description', 'agent-branch', 'remote', 'agent-access', 'index', 'danger']) {
      expect(screen.getByTestId(`toc-${id}`)).toBeInTheDocument();
    }
  });

  // Disconnect is a card-local action: it edits the connection the Remote card
  // describes, so it is an icon button ON that card and NOT a ⋯ menu item.
  // Asserted by scoping the query to the card — a document-wide getByTestId
  // would pass against either placement and so could not tell them apart.
  it('disconnects a remote from the Remote card, behind a confirm', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    (api.deleteOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<RepoManager {...baseProps} onChanged={onChanged} />);
    await selectRepo();

    const remoteCard = await screen.findByTestId('remote-card');
    fireEvent.click(within(remoteCard).getByTestId('remote-disconnect'));
    // Disconnecting asks first — no request until the confirm is accepted.
    expect(api.deleteOrigin).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId('disconnect-confirm'));
    await waitFor(() => expect(api.deleteOrigin).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  // A failed origin load is a THIRD state, not "unconnected": the block carries
  // the error and withholds "Connect a remote…", so the user is not invited to
  // overwrite a remote that is merely unreadable.
  it('surfaces a failed remote load instead of rendering it as unconnected', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('boom'));
    render(<RepoManager {...baseProps} />);
    await selectRepo();

    await waitFor(() => expect(screen.getByTestId('remote-error')).toHaveTextContent(/could not load remote status/i));
    const block = screen.getByTestId('block-remote');
    expect(block.textContent).not.toContain('Not connected');
    expect(within(block).queryByTestId('remote-connect')).not.toBeInTheDocument();
  });

  it('retries a failed remote load', async () => {
    // Driven by an explicit flag rather than mockRejectedValueOnce: Overview
    // fans getOrigin out across EVERY repo on the way in, so a one-shot mock is
    // consumed by the landing page before the repo's own pane ever asks.
    let failing = true;
    const getOrigin = api.getOrigin as ReturnType<typeof vi.fn>;
    getOrigin.mockImplementation((repo: string) => {
      if (repo !== 'core') return Promise.resolve(null);
      if (failing) return Promise.reject(new Error('boom'));
      return Promise.resolve({
        name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
        last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
      });
    });
    render(<RepoManager {...baseProps} />);
    await selectRepo();

    const retry = await screen.findByTestId('remote-retry');
    failing = false;
    fireEvent.click(retry);
    await waitFor(() => expect(screen.getByText('https://github.com/knomit/kb.git')).toBeInTheDocument());
    expect(screen.queryByTestId('remote-error')).not.toBeInTheDocument();
  });

  // Archive was the ⋯ menu's last item. It now sits in the Danger zone block,
  // fenced off by its own tint rather than by an overflow that had to be opened.
  it('puts Archive in the Danger zone block, not behind an overflow', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    const danger = await screen.findByTestId('block-danger');
    expect(within(danger).getByTestId('repo-archive')).toBeInTheDocument();
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('renders the README.md description in the detail pane', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', description: '# Knowledge Base\n\nRoot manifest.',
    });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(await screen.findByTestId('repo-description')).toHaveTextContent('Root manifest.');
  });

  it('renders GFM in the README.md description — a table, not literal pipe text', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core',
      description: '# KB\n\n| Topic | Meaning |\n|---|---|\n| invariants | violate this and it breaks |',
    });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));

    const desc = await screen.findByTestId('repo-description');
    expect(desc.querySelector('table')).not.toBeNull();
    expect(desc.querySelectorAll('th')).toHaveLength(2);
    expect(desc.textContent).not.toContain('|---|');
    // The prose class sits on the inner scroll container that wraps the markdown.
    expect(desc.querySelector('.k-prose')).not.toBeNull();
  });

  // A licence rendered through the markdown pipeline loses every single
  // newline — markdown reflows them. This asserts the raw text survives
  // byte-for-byte, which is the one thing that distinguishes a correct
  // implementation from a plausible-looking one.
  it('renders LICENSE as preformatted text, preserving line breaks', async () => {
    const mit = 'MIT License\n\nPermission is hereby granted, free of charge,\nto any person obtaining a copy';
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', license: mit,
    });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));

    expect((await screen.findByTestId('repo-license')).textContent).toBe(mit);
  });

  // No LICENSE ⇒ no block at all. Unlike the description there is nothing to
  // write (manifest.go has no write path for LicensePath), so an empty block
  // would head a section that offers an action which does not exist.
  it('omits the license block when the repo has no LICENSE', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(screen.queryByTestId('block-license')).toBeNull();
    expect(screen.queryByTestId('toc-license')).toBeNull();
  });

  // With no README.md the block is still offered so a description can be
  // written — but only when the user could actually write one.
  it('offers an empty description block when the repo has no README.md', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(await screen.findByTestId('repo-description')).toHaveTextContent(/No description yet/i);
  });

  it('hides the description block entirely when read-only and empty', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<RepoManager {...baseProps} readOnly />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(screen.queryByTestId('block-description')).not.toBeInTheDocument();
  });

  // Editing a repo description writes README.md through PATCH /repos/{repo},
  // and the pane adopts the SERVER's re-read value, not the local draft.
  it('edits a repo description and saves it to README.md', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    (api.updateRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# New\n\nBody.' });
    render(<RepoManager {...baseProps} />);
    await selectRepo();

    // The pencil opens the card AND enters edit mode in one click.
    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    const input = screen.getByTestId('repo-description-input') as HTMLTextAreaElement;
    expect(input.value).toBe('# Old'); // seeded with the current markdown

    fireEvent.change(input, { target: { value: '# New\n\nBody.' } });
    fireEvent.click(screen.getByTestId('repo-description-save'));

    await waitFor(() => expect(api.updateRepo).toHaveBeenCalledWith('core', { description: '# New\n\nBody.' }));
    // Back to the rendered view, showing what the server returned.
    await waitFor(() => expect(screen.queryByTestId('repo-description-input')).not.toBeInTheDocument());
    expect(screen.getByTestId('repo-description')).toHaveTextContent('Body.');
  });

  it('keeps the editor open and shows the error when a description save fails', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    (api.updateRepo as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('description too long'));
    render(<RepoManager {...baseProps} />);
    await selectRepo();

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    fireEvent.change(screen.getByTestId('repo-description-input'), { target: { value: 'x' } });
    fireEvent.click(screen.getByTestId('repo-description-save'));

    await waitFor(() => expect(screen.getByText(/description too long/)).toBeInTheDocument());
    // The draft is not lost — the user can fix it and retry.
    expect((screen.getByTestId('repo-description-input') as HTMLTextAreaElement).value).toBe('x');
  });

  it('cancelling a description edit discards the draft', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<RepoManager {...baseProps} />);
    await selectRepo();

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    fireEvent.change(screen.getByTestId('repo-description-input'), { target: { value: 'scratch' } });
    fireEvent.click(screen.getByTestId('repo-description-cancel'));

    expect(api.updateRepo).not.toHaveBeenCalled();
    expect(screen.queryByTestId('repo-description-input')).not.toBeInTheDocument();
    // Re-opening starts from the persisted value again, not the discarded draft.
    fireEvent.click(screen.getByTestId('repo-description-edit'));
    expect((screen.getByTestId('repo-description-input') as HTMLTextAreaElement).value).toBe('# Old');
  });

  // A lens description is capped an order of magnitude below a repo's, and the
  // two share one editor — so the editor has to name the cap it is holding
  // rather than let the difference surface as a 422 on Save.
  it('counts bytes against the lens cap and blocks Save when over', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }], description: 'note',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(await screen.findByTestId('repo-description-edit'));

    // A short draft is nowhere near the cap: no counter, Save enabled.
    expect(screen.queryByTestId('repo-description-count')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-description-save')).not.toBeDisabled();

    fireEvent.change(screen.getByTestId('repo-description-input'),
      { target: { value: 'x'.repeat(MAX_LENS_DESCRIPTION_BYTES + 1) } });
    expect(screen.getByTestId('repo-description-count')).toHaveTextContent('4,097 / 4,096 bytes');
    expect(screen.getByTestId('repo-description-save')).toBeDisabled();

    fireEvent.click(screen.getByTestId('repo-description-save'));
    expect(api.updateLens).not.toHaveBeenCalled();
  });

  // Bytes, not characters — the server caps len(string), so multi-byte text
  // reaches the limit well before the character count suggests.
  it('measures the draft in bytes, not characters', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }], description: 'note',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));
    fireEvent.click(await screen.findByTestId('repo-description-edit'));

    // 3-byte characters: under the cap by count, over it by size.
    fireEvent.change(screen.getByTestId('repo-description-input'),
      { target: { value: '—'.repeat(2000) } });
    expect(screen.getByTestId('repo-description-count')).toHaveTextContent('6,000 / 4,096 bytes');
    expect(screen.getByTestId('repo-description-save')).toBeDisabled();
  });

  it('uses the much larger repo cap for a repo description', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    fireEvent.click(await screen.findByTestId('repo-description-edit'));

    // Over a lens's cap, nowhere near a repo's: no counter, Save enabled.
    fireEvent.change(screen.getByTestId('repo-description-input'),
      { target: { value: 'x'.repeat(MAX_LENS_DESCRIPTION_BYTES + 1) } });
    expect(screen.queryByTestId('repo-description-count')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-description-save')).not.toBeDisabled();
  });

  it('hides the description edit affordance in read-only mode', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<RepoManager {...baseProps} readOnly />);
    await selectRepo();
    await screen.findByTestId('repo-description');
    expect(screen.queryByTestId('repo-description-edit')).not.toBeInTheDocument();
  });

  // A lens description goes to the lens registry via PATCH /lenses/{name} —
  // same editor, different destination.
  it('edits a lens description and saves it via updateLens', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }],
      description: 'old lens note',
    });
    (api.updateLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }],
      description: '# New lens note',
    });
    render(<RepoManager {...baseProps} />);
    await waitFor(() => expect(screen.getByTestId('repomgr-lens-dev')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('repomgr-lens-dev'));

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    fireEvent.change(screen.getByTestId('repo-description-input'), { target: { value: '# New lens note' } });
    fireEvent.click(screen.getByTestId('repo-description-save'));

    await waitFor(() => expect(api.updateLens).toHaveBeenCalledWith('dev', { description: '# New lens note' }));
    await waitFor(() => expect(screen.getByTestId('repo-description')).toHaveTextContent('New lens note'));
  });

  // A long README renders open — the fold is gone — but it still scrolls
  // within a bounded height, so it cannot push the wiring blocks off the page.
  it('renders a long description open, in its own bounded scroll', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', description: 'line\n'.repeat(40),
    });
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    const body = await screen.findByTestId('repo-description');
    expect(screen.queryByTestId('repo-description-toggle')).not.toBeInTheDocument();
    const prose = body.querySelector('.k-prose') as HTMLElement;
    expect(prose).not.toBeNull();
    expect(prose.style.overflowY).toBe('auto');
    expect(prose.style.maxHeight).toBe('360px');
  });

  it('rebuild gives immediate feedback and a completion message', async () => {
    render(<RepoManager {...baseProps} />);
    await selectRepo();
    fireEvent.click(await screen.findByTestId('repo-rebuild'));
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

    // Delete lives in the Danger zone block and requires a confirm step.
    fireEvent.click(within(screen.getByTestId('block-danger')).getByTestId('lens-delete'));
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
    expect(writeText).toHaveBeenCalledWith('knomit-bridge claude init --lens dev');
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

  it('hideRemoteConfig hides the remote block entirely', async () => {
    // First verify the block IS present when hideRemoteConfig is false (non-vacuity check).
    const { unmount } = render(<RepoManager {...baseProps} hideRemoteConfig={false} />);
    await selectRepo();
    await waitFor(() => expect(screen.getByTestId('block-remote')).toBeInTheDocument());
    unmount();

    // Now render with hideRemoteConfig=true and assert the panel is absent.
    render(
      <RepoManager
        open
        repos={[{ name: 'core', uid: 'uid-core' }]}
        currentRepo="core"
        readOnly
        hideRemoteConfig
        onChanged={() => {}}
        onBrowse={() => {}}
      />,
    );
    // No block, and nothing in the contents rail pointing at one.
    expect(screen.queryByTestId('block-remote')).toBeNull();
    expect(screen.queryByTestId('toc-remote')).toBeNull();
  });
});
