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
