import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act, cleanup } from '@testing-library/react';
import { PendingCreateSettings } from './PendingCreateSettings';
import { api, type RepoCreateStatus } from './api';
import { __resetRepoCreatesForTest } from './useRepoCreates';

// A create that is not registered yet is a target pinned from the rail. Its
// page is one section, the create's progress, with the one action its state
// allows: Cancel while it runs, Dismiss once it failed. The moment its repo
// exists it hands over to that repo (onOpenRepo), which lands on the repo's
// own Settings in creating mode.

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    listRepoCreates: vi.fn(),
    cancelRepoCreate: vi.fn().mockResolvedValue(undefined),
    dismissRepoCreate: vi.fn().mockResolvedValue(undefined),
    getRepoCreate: vi.fn().mockRejectedValue(new Error('not found')),
    repos: vi.fn().mockResolvedValue([]),
  },
}));

function job(over: Partial<RepoCreateStatus> = {}): RepoCreateStatus {
  return { create_id: 'c1', name: 'newkb', mode: 'subscribe', state: 'running', ...over };
}

function renderPage() {
  const onClose = vi.fn();
  const onOpenRepo = vi.fn();
  render(<PendingCreateSettings createId="c1" onClose={onClose} onOpenRepo={onOpenRepo} />);
  return { onClose, onOpenRepo };
}

beforeEach(() => { __resetRepoCreatesForTest(); vi.clearAllMocks(); });
afterEach(() => { cleanup(); __resetRepoCreatesForTest(); });

describe('PendingCreateSettings', () => {
  it('is one Creating section with the progress, indexed under "Repository"', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ message: 'cloning' })]);
    renderPage();
    expect(await screen.findByTestId('create-progress')).toBeInTheDocument();
    const ids = Array.from(document.querySelectorAll('section[id]')).map(s => s.id);
    expect(ids).toEqual(['create']);
    expect(screen.getByTestId('block-create')).toHaveTextContent('Creating newkb');
    expect(screen.getByTestId('settings-index-heading')).toHaveTextContent(/^Repository$/);
  });

  it('Cancel calls cancelRepoCreate with the create id and then says Cancelling…', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job()]);
    renderPage();
    const cancel = await screen.findByTestId('create-cancel-button');
    expect(cancel).toHaveTextContent('Cancel create');
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ state: 'cancelling' })]);
    await act(async () => { fireEvent.click(cancel); });
    expect(api.cancelRepoCreate).toHaveBeenCalledWith('c1');
    await waitFor(() => expect(screen.getByTestId('create-cancel-button')).toHaveTextContent('Cancelling…'));
    expect(screen.getByTestId('create-cancel-button')).toBeDisabled();
  });

  it('a failed create shows Dismiss and no Cancel, and Dismiss closes the page', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ state: 'failed', error: 'remote is not a knowledge base' })]);
    const { onClose } = renderPage();
    const dismiss = await screen.findByTestId('create-watch-dismiss');
    expect(screen.getByTestId('create-progress')).toHaveTextContent('remote is not a knowledge base');
    expect(screen.queryByTestId('create-cancel-button')).toBeNull();
    vi.mocked(api.listRepoCreates).mockResolvedValue([]);
    await act(async () => { fireEvent.click(dismiss); });
    expect(api.dismissRepoCreate).toHaveBeenCalledWith('c1');
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it('a running create offers Cancel and no Dismiss (control for the case above)', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job()]);
    renderPage();
    expect(await screen.findByTestId('create-cancel-button')).toBeEnabled();
    expect(screen.queryByTestId('create-watch-dismiss')).toBeNull();
  });

  it('hands over to the repo once the job says it is registered AND the list carries it', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ step: 'index', phase: 'index', repo: { name: 'newkb', uid: 'u-new' } })]);
    vi.mocked(api.repos).mockResolvedValue([{ name: 'newkb', uid: 'u-new' }]);
    const { onOpenRepo } = renderPage();
    await waitFor(() => expect(onOpenRepo).toHaveBeenCalledWith('newkb'));
    expect(onOpenRepo).toHaveBeenCalledTimes(1);
  });

  it('says so when the job is no longer listed anywhere', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([]);
    renderPage();
    expect(await screen.findByTestId('create-watch-gone')).toBeInTheDocument();
  });
});
