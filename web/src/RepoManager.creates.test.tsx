import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';
import { __resetRepoCreatesForTest } from './useRepoCreates';
import type { RepoCreateStatus } from './api';

// A create in flight produces NOTHING to list until it finishes, so every
// surface that shows repositories showed nothing about one running. These pin
// the repair: the work is a row, it sits where its name sorts, it can be
// opened and dismissed, and it gives way to the repo it made.

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
    listArchived: vi.fn().mockResolvedValue([]),
    listLenses: vi.fn().mockResolvedValue([]),
    getRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    getOrigin: vi.fn().mockResolvedValue(null),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    listBranchNames: vi.fn().mockResolvedValue([]),
    listRepoCreates: vi.fn().mockResolvedValue([]),
    dismissRepoCreate: vi.fn().mockResolvedValue(undefined),
    cancelRepoCreate: vi.fn().mockResolvedValue(undefined),
    getRepoCreate: vi.fn().mockRejectedValue(new Error('not found')),
  },
}));

function job(over: Partial<RepoCreateStatus> = {}): RepoCreateStatus {
  return { create_id: 'c1', name: 'newkb', mode: 'subscribe', state: 'running', ...over };
}

const baseProps = {
  open: true as const,
  repos: [{ name: 'core', uid: 'uid-core' }],
  currentRepo: 'core',
  readOnly: false,
  hideRemoteConfig: false,
  onChanged: () => {},
  onBrowse: () => {},
};

beforeEach(() => { __resetRepoCreatesForTest(); vi.clearAllMocks(); });
afterEach(() => { __resetRepoCreatesForTest(); });

describe('creates in the repo manager', () => {
  it('shows a running create as a repo-shaped rail row and in the overview', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({
      step: 'subscribe', phase: 'transfer', indeterminate: true, message: 'knomit: sent 5 MiB', pct: 40,
    })]);
    render(<RepoManager {...baseProps} />);

    // The rail: the create sits alongside the repositories, not hidden until
    // it succeeds, and drawn as one of them — name, flag, nothing else.
    const railRow = await screen.findByTestId('pending-create-rail-newkb');
    expect(railRow).toHaveAttribute('data-create-state', 'creating');
    expect(screen.getByTestId('pending-create-chip-rail-newkb')).toHaveTextContent('creating');
    expect(screen.getByTestId('repomgr-item-core')).toBeInTheDocument();

    // NO inline controls in the rail. The progress line and the operations
    // live on the create's own page; a row with buttons on it is what read as
    // "all jumbled up and squished".
    expect(screen.queryByTestId('pending-create-open-rail-newkb')).toBeNull();
    expect(screen.queryByTestId('pending-create-cancel-rail-newkb')).toBeNull();
    expect(screen.queryByTestId('pending-create-dismiss-rail-newkb')).toBeNull();
    expect(screen.queryByTestId('pending-create-message-rail-newkb')).toBeNull();
    expect(screen.queryByTestId('create-bar-indeterminate-rail-newkb')).toBeNull();

    // The overview lists it the same way.
    expect(await screen.findByTestId('pending-creates')).toBeInTheDocument();
    expect(screen.getByTestId('pending-create-chip-overview-newkb')).toHaveTextContent('creating');
  });

  // A create whose repo has ALREADY landed leaves the rail. The job record
  // outlives the create on purpose (a client that lost the id must still find
  // the outcome), so the rail has to end the row's life itself rather than
  // wait for the server to forget it — two rows for one name would read as two
  // repositories.
  it('drops the rail row once the repo it made is in the list', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'core', state: 'done', repo: { name: 'core' } }),
    ]);
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('manage-overview');
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(screen.queryByTestId('pending-create-rail-core')).not.toBeInTheDocument();
    expect(screen.getByTestId('repomgr-item-core')).toBeInTheDocument();
  });

  // CLICKING THE ROW OPENS THE PAGE — the row IS the control, exactly as a
  // repository row is. The user: clicking the row MUST open details.
  it('opens a running create in its own page when the row is clicked', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ message: 'cloning' })]);
    render(<RepoManager {...baseProps} />);

    fireEvent.click(await screen.findByTestId('pending-create-rail-newkb'));

    expect(await screen.findByTestId('create-watch')).toBeInTheDocument();
    // The WATCH page, not the wizard: re-entering the form would ask the user
    // to re-answer questions the running create already has answers for.
    expect(screen.queryByTestId('create-repo-wizard')).not.toBeInTheDocument();
    expect(screen.getByTestId('create-progress')).toBeInTheDocument();
    // Shaped like a repository page: the name is the heading, and the flag
    // sits where a repository's subtitle does.
    expect(screen.getByTestId('create-watch-flag')).toHaveTextContent('creating');
  });

  // DISMISS MOVED TO THE PAGE, with every other operation on a create. The
  // rail row flags the failure; the page is where the reason is read and the
  // row is disposed of.
  it('dismisses a failed create from its page and refreshes the list', async () => {
    vi.mocked(api.listRepoCreates)
      .mockResolvedValue([job({ state: 'failed', error: 'remote is not a knowledge base' })]);
    render(<RepoManager {...baseProps} />);

    const row = await screen.findByTestId('pending-create-rail-newkb');
    expect(row).toHaveAttribute('data-create-state', 'failed');
    // The error is NOT on the row.
    expect(screen.queryByTestId('pending-create-error-rail-newkb')).toBeNull();

    fireEvent.click(row);
    await screen.findByTestId('create-watch');
    expect(screen.getByTestId('create-watch-flag')).toHaveTextContent('create failed');
    expect(screen.getByTestId('create-progress')).toHaveTextContent('remote is not a knowledge base');
    // A failed create offers no cancel: there is nothing left to stop.
    expect(screen.queryByTestId('create-cancel-button')).toBeNull();

    vi.mocked(api.listRepoCreates).mockResolvedValue([]);
    await act(async () => {
      fireEvent.click(screen.getByTestId('create-watch-dismiss'));
    });
    expect(api.dismissRepoCreate).toHaveBeenCalledWith('c1');
  });

  // CANCEL LIVES ON THE PAGE TOO, and says "Cancelling…" while the server is
  // honouring it rather than leaving the last progress line up — the
  // "everything is frozen, stuck in the current stage" the user reported.
  it('cancels from the create page and reports cancelling', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job()]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('pending-create-rail-newkb'));
    await screen.findByTestId('create-watch');

    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ state: 'cancelling' })]);
    await act(async () => {
      fireEvent.click(screen.getByTestId('create-cancel-button'));
    });
    expect(api.cancelRepoCreate).toHaveBeenCalledWith('c1');

    await waitFor(() =>
      expect(screen.getByTestId('create-cancel-button')).toBeDisabled());
    expect(screen.getByTestId('create-cancel-button')).toHaveTextContent('Cancelling…');
    expect(screen.getByTestId('create-cancelling-note'))
      .toHaveTextContent('Waiting for the current step to finish, then rolling back.');
    expect(screen.getByTestId('create-watch-flag')).toHaveTextContent('cancelling');
    // The rail says the same word, and still has no controls on it.
    expect(screen.getByTestId('pending-create-chip-rail-newkb')).toHaveTextContent('cancelling');
  });

  // A CANCELLING CREATE IS STILL A ROW. It is non-terminal — the repo is not
  // gone yet — so the rail must keep showing it, flagged 'cancelling'. In the
  // browser run that caught the navigation bug the rail showed NO row at all
  // after the click, and this is the regression test for the row itself,
  // independent of whatever made it vanish.
  it('draws a cancelling create in the rail and in the overview', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ state: 'cancelling', step: 'subscribe' })]);
    render(<RepoManager {...baseProps} />);

    const row = await screen.findByTestId('pending-create-rail-newkb');
    expect(row).toHaveAttribute('data-create-state', 'cancelling');
    expect(screen.getByTestId('pending-create-name-rail-newkb')).toHaveTextContent('newkb');
    expect(screen.getByTestId('pending-create-chip-rail-newkb')).toHaveTextContent('cancelling');
    // Still no controls on the row, and still openable by clicking it.
    expect(screen.queryByTestId('pending-create-cancel-rail-newkb')).toBeNull();

    expect(await screen.findByTestId('pending-creates')).toBeInTheDocument();
    expect(screen.getByTestId('pending-create-chip-overview-newkb')).toHaveTextContent('cancelling');

    fireEvent.click(row);
    expect(await screen.findByTestId('create-watch')).toBeInTheDocument();
    expect(screen.getByTestId('create-watch-flag')).toHaveTextContent('cancelling');
    expect(screen.getByTestId('create-cancel-button')).toBeDisabled();
    expect(screen.getByTestId('create-cancel-button')).toHaveTextContent('Cancelling…');
  });

  // ONCE THE REPO EXISTS, FLAG THE REPO — do not hide the job.
  //
  // A create's repo is registered (m.Add) well before the job finishes, so
  // from that moment every repository surface listed it as an ordinary repo.
  // pendingCreates drops a create whose name matches a repo, which is right
  // for the ROW — two rows for one name read as two repositories — but it left
  // the repo itself unflagged for the whole register/index/sync window, and
  // for the entire late-cancel window after that. The reader saw a normal
  // repository with a normal index chip while it was being deleted underneath
  // them.
  it('flags the repo row itself when a create is still working on it', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'core', state: 'running', step: 'index' }),
    ]);
    render(<RepoManager {...baseProps} />);

    const row = await screen.findByTestId('repomgr-item-core');
    await waitFor(() => expect(row).toHaveAttribute('data-create-state', 'creating'));
    expect(screen.getByTestId('repomgr-create-chip-core')).toHaveTextContent('creating');

    // ONE row, not two: the repo's row carries the flag and no separate
    // pending-create row appears beside it.
    expect(screen.queryByTestId('pending-create-rail-core')).not.toBeInTheDocument();
    expect(screen.getAllByTestId('repomgr-item-core')).toHaveLength(1);
    // The create flag WINS: an index chip beside it would report a detail of
    // work whose outcome is not settled.
    expect(screen.queryByTestId('repo-index-indexing')).not.toBeInTheDocument();
  });

  // ONE PAGE, GATED BY STATE. A repository being created IS a repository, so
  // the rail row opens the repository's own details page — rendered in
  // creating mode — and not a bespoke create view beside it.
  it('opens the merged repo page from the rail, flagged and with the parts that cannot be offered withheld', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'core', state: 'cancelling', step: 'index', index_state: 'indexing' }),
    ]);
    render(<RepoManager {...baseProps} />);

    const row = await screen.findByTestId('repomgr-item-core');
    await waitFor(() => expect(screen.getByTestId('repomgr-create-chip-core')).toHaveTextContent('cancelling'));
    fireEvent.click(row);

    // The REPOSITORY page, not a create page.
    expect(await screen.findByTestId('repo-settings')).toBeInTheDocument();
    expect(screen.queryByTestId('create-watch')).not.toBeInTheDocument();
    // Flagged where the subtitle goes — the only difference from an ordinary
    // repository's header.
    expect(screen.getByTestId('repo-detail-subtitle')).toHaveTextContent('cancelling');

    // The progress block, with the one action this state allows.
    expect(screen.getByTestId('create-progress')).toBeInTheDocument();
    const cancel = screen.getByTestId('create-cancel-button');
    expect(cancel).toBeDisabled();
    expect(cancel).toHaveTextContent('Cancelling…');

    // WITHHELD: nothing here can honestly be offered on a half-made repo.
    expect(screen.queryAllByText('Danger zone')).toHaveLength(0);
    expect(screen.queryAllByText('Remote')).toHaveLength(0);
    expect(screen.queryByTestId('repo-rebuild')).not.toBeInTheDocument();
    // SHOWN, read-only: the repo is readable from the index phase on.
    expect(screen.getByTestId('repo-browse')).toBeInTheDocument();
  });

  it('offers Cancel create on the merged page while the job is running', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'core', state: 'running', step: 'index', index_state: 'indexing' }),
    ]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-item-core'));
    await screen.findByTestId('repo-settings');

    expect(screen.getByTestId('repo-detail-subtitle')).toHaveTextContent('creating');
    const cancel = screen.getByTestId('create-cancel-button');
    expect(cancel).toBeEnabled();
    expect(cancel).toHaveTextContent('Cancel create');
    await act(async () => { fireEvent.click(cancel); });
    expect(api.cancelRepoCreate).toHaveBeenCalledWith('c1');
  });

  // THE BLOCK DISAPPEARS IN PLACE. No navigation: the reader is already on the
  // repository's page, and it simply becomes the ordinary one.
  it('turns into the normal settings page when the job finishes, without navigating', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'core', state: 'running', step: 'index', index_state: 'indexing' }),
    ]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-item-core'));
    await screen.findByTestId('repo-settings');
    expect(screen.getByTestId('create-progress')).toBeInTheDocument();

    // The job ends. Same page, same scroll, no route change.
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'core', state: 'done', repo: { name: 'core' } }),
    ]);
    await waitFor(() => expect(screen.queryByTestId('create-progress')).not.toBeInTheDocument(), { timeout: 5000 });
    expect(screen.getByTestId('repo-settings')).toBeInTheDocument();
    expect(screen.getByTestId('repo-detail-subtitle')).toHaveTextContent('repository settings');
    expect(screen.queryAllByText('Danger zone').length).toBeGreaterThan(0);
    expect(screen.getByTestId('repo-rebuild')).toBeInTheDocument();
  });

  // A FINISHED job stops flagging the repo: it now stands on its own.
  it('leaves the repo row alone once its create is done', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'core', state: 'done', repo: { name: 'core' } }),
    ]);
    render(<RepoManager {...baseProps} />);
    const row = await screen.findByTestId('repomgr-item-core');
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(row).not.toHaveAttribute('data-create-state');
    expect(screen.queryByTestId('repomgr-create-chip-core')).not.toBeInTheDocument();
    fireEvent.click(row);
    expect(screen.queryByTestId('create-watch')).not.toBeInTheDocument();
  });

  // A CANCELLED CREATE LEAVES THE RAIL ENTIRELY. The server omits it from the
  // collection; the row must not come back even if a stale list still had one.
  it('never draws a cancelled create in the rail', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ state: 'cancelled' })]);
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('manage-overview');
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(screen.queryByTestId('pending-create-rail-newkb')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pending-creates')).not.toBeInTheDocument();
  });

  // No creates, no block. An empty "Being created" heading would be the
  // overview answering a question nobody asked.
  it('renders no creates block when nothing is being created', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([]);
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('manage-overview');
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(screen.queryByTestId('pending-creates')).not.toBeInTheDocument();
  });
});
