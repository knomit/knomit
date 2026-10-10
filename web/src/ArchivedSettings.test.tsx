import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
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

  // Each archived repo says why it is there: by the user (with the note), by
  // knomit (with the why, and that restoring will be refused again), or — for
  // one archived before reasons were recorded — a neutral "No reason
  // recorded". An older server sends no reason at all: nothing is rendered.
  // Sabotage: drop ArchiveReasonLine from ArchivedDetail → red.
  it('shows why each archived repo was archived', async () => {
    vi.mocked(api.listArchived).mockResolvedValue([
      { id: 'u.1', name: 'mine', origin: '', archivedAt: '2026-10-01T00:00:00Z',
        reason: { source: 'user', reason: '', note: 'moved', condition: '', summary: 'Archived by the user: moved' } },
      { id: 's.2', name: 'linked', origin: '', archivedAt: '2026-10-02T00:00:00Z',
        reason: { source: 'system', reason: '.knomit/ontology.yaml is a symlink', note: '', condition: 'unopenable',
          summary: 'Archived automatically: .knomit/ontology.yaml is a symlink' } },
      { id: 'l.3', name: 'legacy', origin: '', archivedAt: '2026-01-01T00:00:00Z', reason: null },
      { id: 'o.4', name: 'olderserver', origin: '', archivedAt: '2026-01-01T00:00:00Z' },
    ]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-archived'));
    expect(await screen.findByTestId('archived-reason-u.1')).toHaveTextContent('Archived by the user: moved');
    expect(screen.queryByTestId('archived-condition-u.1')).toBeNull();
    const sys = screen.getByTestId('archived-reason-s.2');
    expect(sys).toHaveTextContent('Archived automatically: .knomit/ontology.yaml is a symlink');
    expect(sys).toHaveTextContent('Restoring re-runs the check');
    expect(screen.getByTestId('archived-condition-s.2')).toHaveTextContent('unopenable');
    expect(screen.getByTestId('archived-reason-l.3')).toHaveTextContent('No reason recorded');
    expect(screen.queryByTestId('archived-reason-o.4')).toBeNull();
    expect(screen.getByTestId('toc-archived-s.2').textContent).toContain('linked');
  });

  it('counts the automatically archived on the Overview', async () => {
    vi.mocked(api.listArchived).mockResolvedValue([
      { id: 's.2', name: 'linked', origin: '', archivedAt: '2026-10-02T00:00:00Z',
        reason: { source: 'system', reason: 'x', note: '', condition: 'unopenable', summary: 'Archived automatically: x' } },
      { id: 'l.3', name: 'legacy', origin: '', archivedAt: '2026-01-01T00:00:00Z', reason: null },
    ]);
    render(<RepoManager {...baseProps} />);
    expect(await screen.findByTestId('overview-auto-archived')).toHaveTextContent('(1 automatically)');
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
});
