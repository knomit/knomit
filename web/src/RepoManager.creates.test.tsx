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
    listClientSessions: vi.fn().mockResolvedValue({ sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360 } }),
    listArchived: vi.fn().mockResolvedValue([]),
    listLenses: vi.fn().mockResolvedValue([]),
    getRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    getOrigin: vi.fn().mockResolvedValue(null),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    listBranchNames: vi.fn().mockResolvedValue([]),
    listRepoCreates: vi.fn().mockResolvedValue([]),
    dismissRepoCreate: vi.fn().mockResolvedValue(undefined),
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
  it('shows a running create as a rail row and in the overview, with live progress', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({
      step: 'subscribe', phase: 'transfer', indeterminate: true, message: 'knomit: sent 5 MiB', pct: 40,
    })]);
    render(<RepoManager {...baseProps} />);

    // The rail: the create sits alongside the repositories, not hidden until
    // it succeeds.
    const railRow = await screen.findByTestId('pending-create-rail-newkb');
    expect(railRow).toHaveAttribute('data-create-state', 'creating');
    expect(screen.getByTestId('repomgr-item-core')).toBeInTheDocument();

    // The overview block, with the remote's own progress line and a bar that
    // claims no percentage.
    const block = await screen.findByTestId('pending-creates');
    expect(block).toBeInTheDocument();
    expect(screen.getByTestId('pending-create-message-overview-newkb')).toHaveTextContent('knomit: sent 5 MiB');
    expect(screen.getByTestId('create-bar-indeterminate-overview-newkb')).toBeInTheDocument();
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

  it('opens a running create in its own watch page', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ message: 'cloning' })]);
    render(<RepoManager {...baseProps} />);

    const open = await screen.findByTestId('pending-create-open-rail-newkb');
    fireEvent.click(open);

    expect(await screen.findByTestId('create-watch')).toBeInTheDocument();
    // The WATCH page, not the wizard: re-entering the form would ask the user
    // to re-answer questions the running create already has answers for.
    expect(screen.queryByTestId('create-repo-wizard')).not.toBeInTheDocument();
    expect(screen.getByTestId('create-progress')).toBeInTheDocument();
  });

  it('dismisses a finished create through the API and refreshes the list', async () => {
    vi.mocked(api.listRepoCreates)
      .mockResolvedValueOnce([job({ state: 'failed', error: 'remote is not a knowledge base' })])
      .mockResolvedValue([]);
    render(<RepoManager {...baseProps} />);

    const row = await screen.findByTestId('pending-create-rail-newkb');
    expect(row).toHaveAttribute('data-create-state', 'create-failed');
    expect(screen.getByTestId('pending-create-error-rail-newkb'))
      .toHaveTextContent('remote is not a knowledge base');

    await act(async () => {
      fireEvent.click(screen.getByTestId('pending-create-dismiss-rail-newkb'));
    });

    expect(api.dismissRepoCreate).toHaveBeenCalledWith('c1');
    await waitFor(() => expect(screen.queryByTestId('pending-create-rail-newkb')).not.toBeInTheDocument());
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
