import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within, act } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';
import { FakeEventSource, installFakeEventSource } from './testEventSource';

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

  // The contents-rail (now index column) pin/spy tests moved to
  // SettingsPage.test.tsx, which mounts the page directly.

  // With zero repositories the create form is the FALLBACK selection, not
  // something you clicked, so the rail's disabled `+` never gets a chance to
  // stop you. Read-only used to land straight on a live form whose submit would
  // be refused, with nothing on screen saying why.
  it('explains itself instead of offering a create form it cannot submit', async () => {
    render(<RepoManager {...baseProps} repos={[]} currentRepo="" serverReadOnly />);

    expect(await screen.findByTestId('create-blocked-repository')).toBeInTheDocument();
    // `step-source` is the wizard's first screen — a proxy for "the create
    // surface rendered" (was `create-name`, the old flat form's first field;
    // see App.norepos.test.tsx for the fuller rationale).
    expect(screen.queryByTestId('step-source')).not.toBeInTheDocument();
  });

  // The same hole from the other direction: a selection made while live
  // survives into a history excursion, which is also read-only.
  it('blocks a create form the selection carried into a read-only state', async () => {
    const { rerender } = render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-new'));
    expect(await screen.findByTestId('step-source')).toBeInTheDocument();

    rerender(<RepoManager {...baseProps} serverReadOnly />);
    expect(screen.queryByTestId('step-source')).not.toBeInTheDocument();
    expect(screen.getByTestId('create-blocked-repository')).toBeInTheDocument();
  });
});

// The tab strip is the second navigation axis: server-wide PAGES across the
// top, the things you OWN down the rail. The two must never look like
// siblings, which is the whole reason the split exists.
describe('Manage tabs', () => {
  const baseProps = {
    open: true as const,
    repos: [{ name: 'core', uid: 'uid-core' }, { name: 'work', uid: 'uid-work' }],
    currentRepo: 'core',
    serverReadOnly: false,
    hideRemoteConfig: false,
    onChanged: () => {},
    onBrowse: () => {},
  };
  const ACTIVE = 'rgb(34, 48, 58)'; // #22303a, the same lit background the rail uses
  const sess = (state: 'live' | 'idle' | 'dead') => ({
    id: `s-${state}-${Math.random()}`, instance_id: 'i', state, transport: 'stdio' as const,
    binding: { kind: 'repo', uid: 'uid-core', name: 'core' }, bindings: [], branch: 'agent/test',
    client: { name: 'claude-code', version: '2', initialized: true },
    bridge: { host: 'h', user: 'u', cwd: '/w', pid: 1, parent: 'claude', parent_pid: 2, version: '1' },
    remote_addr: '127.0.0.1', user_agent: 'knomit-bridge/1',
    first_seen_at: '2026-09-14T11:00:00Z', last_seen_at: '2026-09-14T11:59:00Z',
    ended_at: null, request_count: 3,
  });
  const POLICY = { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 };
  // The Authorizations tests narrow this; put the no-issuer default back so
  // test order cannot matter.
  beforeEach(() => { vi.mocked(api.listOAuthPending).mockResolvedValue(null); });

  // F09: Fleet is a top-level server page, BETWEEN Knowledge and Sessions
  // (user ruling), and it is a server page (no rail beside it).
  it('places the Fleet tab between Knowledge and Sessions', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [] });
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-overview');
    const ids = screen.getAllByRole('tab').map(t => t.getAttribute('data-testid'));
    const k = ids.indexOf('repomgr-overview');
    expect(ids[k + 1]).toBe('repomgr-fleet');
    expect(ids[k + 2]).toBe('repomgr-sessions');
  });

  it('badges the live count, and lights NEITHER tab once an entity is selected', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false,
      policy: POLICY,
      sessions: [sess('live'), sess('live'), sess('dead'), sess('idle')],
    });
    render(<RepoManager {...baseProps} />);

    const overview = await screen.findByTestId('repomgr-overview');
    const sessions = screen.getByTestId('repomgr-sessions');

    // Manage lands on Overview, so its tab is the lit one.
    expect(overview).toHaveStyle({ background: ACTIVE });
    expect(sessions).not.toHaveStyle({ background: ACTIVE });
    // Only the live ones count — idle and dead are not "connected right now".
    expect(await screen.findByTestId('repomgr-sessions-badge')).toHaveTextContent('2');

    // Switching to Sessions moves the highlight across the strip.
    fireEvent.click(sessions);
    expect(await screen.findByTestId('manage-sessions')).toBeInTheDocument();
    expect(sessions).toHaveStyle({ background: ACTIVE });
    expect(overview).not.toHaveStyle({ background: ACTIVE });

    // Selecting a repo is the OTHER axis: the rail row lights, and both tabs
    // go dim. A lit tab here would claim the pane is still showing a
    // server-wide page.
    //
    // Back via Overview first: Sessions is a server page and carries no rail,
    // so a repo is one tab click away from it rather than one rail click.
    fireEvent.click(overview);
    await screen.findByTestId('manage-overview');
    fireEvent.click(screen.getByTestId('repomgr-item-core'));
    await screen.findByTestId('block-agent-branch');
    expect(overview).not.toHaveStyle({ background: ACTIVE });
    expect(sessions).not.toHaveStyle({ background: ACTIVE });
    expect(screen.getByTestId('repomgr-item-core')).toHaveStyle({ background: ACTIVE });
  });

  it('updates the badge live while Manage is open on Overview', async () => {
    installFakeEventSource();
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess('live')] });
    render(<RepoManager {...baseProps} />);
    expect(await screen.findByTestId('repomgr-sessions-badge')).toHaveTextContent('1');

    // A second session initializes. Without the stream this badge would not
    // move until the next window focus — the bug this fixes.
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess('live'), sess('live')] });
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    await act(async () => { FakeEventSource.instances[0].emit('session', { id: 's2', kind: 'init' }); });

    await waitFor(() => expect(screen.getByTestId('repomgr-sessions-badge')).toHaveTextContent('2'));
  });

  it('opens exactly one sessions stream per tab: the page owns it while it is up', async () => {
    installFakeEventSource();
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess('live')] });
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-sessions-badge');
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    const headerStream = FakeEventSource.instances[0];

    // Opening Sessions hands the stream over: the page reports its count back
    // through onLiveCount, so a second stream would be two subscribers
    // feeding one number.
    fireEvent.click(screen.getByTestId('repomgr-sessions'));
    await screen.findByTestId('manage-sessions');
    await waitFor(() => expect(headerStream.closeCount).toBe(1));
    expect(FakeEventSource.instances.filter(es => es.closeCount === 0)).toHaveLength(1);
  });

  // The regression this pins: the header's own read is async, and the Sessions
  // page reports a FRESHER count through onLiveCount the moment it opens. A
  // read dispatched before that must not land after it and overwrite it.
  it('does not let a stale header read overwrite the count the Sessions page reported', async () => {
    installFakeEventSource();
    let settleStale: (v: { truncated: boolean; policy: typeof POLICY; sessions: ReturnType<typeof sess>[] }) => void = () => {};
    vi.mocked(api.listClientSessions).mockReturnValueOnce(
      new Promise(resolve => { settleStale = resolve; }),
    );
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-sessions');

    // The Sessions page opens and reports 2 while the header's read is still
    // in flight.
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess('live'), sess('live')] });
    fireEvent.click(screen.getByTestId('repomgr-sessions'));
    await screen.findByTestId('manage-sessions');
    await waitFor(() => expect(screen.getByTestId('repomgr-sessions-badge')).toHaveTextContent('2'));

    // ...and only now does the older read come back, with the older number.
    await act(async () => { settleStale({ truncated: false, policy: POLICY, sessions: [sess('live')] }); });
    expect(screen.getByTestId('repomgr-sessions-badge')).toHaveTextContent('2');
  });

  // Same shape, failure variant: a rejection from a superseded read must not
  // blank a badge that a newer, successful reader has already filled in.
  it('does not blank the badge when a superseded header read fails', async () => {
    installFakeEventSource();
    let failStale: (e: Error) => void = () => {};
    vi.mocked(api.listClientSessions).mockReturnValueOnce(
      new Promise((_, reject) => { failStale = reject; }),
    );
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-sessions');

    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess('live'), sess('live')] });
    fireEvent.click(screen.getByTestId('repomgr-sessions'));
    await screen.findByTestId('manage-sessions');
    await waitFor(() => expect(screen.getByTestId('repomgr-sessions-badge')).toHaveTextContent('2'));

    await act(async () => { failStale(new Error('503')); });
    expect(screen.getByTestId('repomgr-sessions-badge')).toHaveTextContent('2');
  });

  // Opening Sessions must cost exactly ONE list read: the page's own first
  // load. The header stops reading (the page reports its count back through
  // onLiveCount), so a second read means something re-ran that should not have.
  //
  // Asserted across the TRANSITION rather than on a standalone ManageSessions
  // mount, because that is the only place the fault can appear: ManageSessions
  // keeps onLiveCount in load's useCallback deps and runs its poll effect on
  // [load], so an onLiveCount whose identity changes per render re-runs the
  // effect and issues an extra read. A standalone render with a stable prop
  // passes either way.
  it('opening Sessions costs exactly one list read', async () => {
    installFakeEventSource();
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess('live')] });
    const { rerender } = render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-sessions-badge');
    const before = vi.mocked(api.listClientSessions).mock.calls.length;

    fireEvent.click(screen.getByTestId('repomgr-sessions'));
    await screen.findByTestId('manage-sessions');
    await act(async () => { await Promise.resolve(); });
    expect(vi.mocked(api.listClientSessions).mock.calls.length - before).toBe(1);

    // And a RepoManager re-render — which the page itself causes every time it
    // reports a changed count — must cost nothing. This is the half an inline
    // arrow fails: a new callback identity per render reaches the page's
    // effect deps and re-runs its poll.
    const afterOpen = vi.mocked(api.listClientSessions).mock.calls.length;
    rerender(<RepoManager {...baseProps} />);
    await act(async () => { await Promise.resolve(); });
    expect(vi.mocked(api.listClientSessions).mock.calls.length).toBe(afterOpen);
  });

  // The landing tab shows only the knowledge bases — repositories and lenses —
  // and is the one tab beside which the rail renders. "Overview" promised a
  // summary of the whole server and delivered one kind of thing, which only
  // became misleading once Sessions and Logs sat beside it.
  //
  // The label is the assertion; the test id stays repomgr-overview, because
  // renaming identifiers would churn tests and facts for no user-visible gain.
  it('names the landing tab Knowledge, not Overview', async () => {
    render(<RepoManager {...baseProps} />);
    const knowledge = await screen.findByRole('tab', { name: 'Knowledge' });
    expect(knowledge).toHaveAttribute('data-testid', 'repomgr-overview');
    // Manage lands here, so it is the lit one.
    expect(knowledge).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByRole('tab', { name: 'Overview' })).not.toBeInTheDocument();
  });

  it('drops the entity rail on Sessions — a server page is not about your repos', async () => {
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-sessions'));
    expect(await screen.findByTestId('manage-sessions')).toBeInTheDocument();

    // The tab strip is the OTHER axis and stays: hiding the rail is not
    // "server pages have no navigation".
    expect(screen.getByTestId('repomgr-sessions')).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByTestId('repomgr-overview')).toBeInTheDocument();

    // Every rail control is gone, not dimmed. Nothing in it is selected and
    // nothing in it explains or filters a table about the server, so it would
    // be a column of dead exits — and a dead control is worse than an absent
    // one.
    for (const id of ['repomgr-new', 'repomgr-item-core', 'repomgr-item-work', 'repomgr-new-lens', 'repomgr-archived']) {
      expect(screen.queryByTestId(id)).not.toBeInTheDocument();
    }
    expect(within(screen.getByTestId('manage-detail')).getByTestId('manage-sessions')).toBeInTheDocument();
  });

  it('brings the rail back on Overview, whose content IS the entities', async () => {
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-sessions'));
    await screen.findByTestId('manage-sessions');
    expect(screen.queryByTestId('repomgr-item-core')).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId('repomgr-overview'));
    await screen.findByTestId('manage-overview');
    // Overview is a TAB and keeps the rail: the rule is about what the pane
    // shows, not about which axis you clicked last.
    expect(screen.getByTestId('repomgr-item-core')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-item-work')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-archived')).toBeInTheDocument();
  });

  it('opens the Logs page full width, with no rail', async () => {
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-logs'));
    expect(await screen.findByTestId('manage-logs')).toBeInTheDocument();

    // A server page, so the same rule Sessions established applies.
    expect(screen.getByTestId('repomgr-logs')).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByTestId('repomgr-item-core')).not.toBeInTheDocument();
    expect(screen.queryByTestId('repomgr-archived')).not.toBeInTheDocument();
    // The strip stays, and Sessions is still reachable from it.
    expect(screen.getByTestId('repomgr-sessions')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-overview')).toBeInTheDocument();
  });

  // The read-only demo refuses the log with a 403, so a Logs tab there would be
  // a control that cannot work — worse than an absent one, the same rule the
  // rail follows.
  it('renders no Logs tab at all on a read-only instance', async () => {
    render(<RepoManager {...baseProps} serverReadOnly />);
    await screen.findByTestId('repomgr-sessions');
    expect(screen.queryByTestId('repomgr-logs')).not.toBeInTheDocument();
  });

  // F19 phase 3b: the Authorizations tab exists only where the server has an
  // OAuth issuer. The list call is the probe: 404 (null) means no tab, which
  // is the default everywhere else in this file and the desktop's case.
  it('renders no Authorizations tab when the server has no OAuth issuer', async () => {
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-sessions');
    await waitFor(() => expect(api.listOAuthPending).toHaveBeenCalled());
    expect(screen.queryByTestId('repomgr-oauth')).not.toBeInTheDocument();
  });

  it('opens the Authorizations page full width, badged with the waiting count', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([
      { id: 'p1', client_id: 'kb', client_name: 'kb', redirect_uri: 'http://127.0.0.1:1/callback', scopes: ['read'], resource: 'http://localhost:1', remote_addr: '127.0.0.1:2', user_agent: 'ua', created_at: '2026-09-24T12:00:00Z', expires_at: '2026-09-24T12:10:00Z' },
    ]);
    render(<RepoManager {...baseProps} />);
    expect(await screen.findByTestId('repomgr-oauth-badge')).toHaveTextContent('1');
    fireEvent.click(screen.getByTestId('repomgr-oauth'));
    expect(await screen.findByTestId('oauth-row')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-oauth')).toHaveAttribute('aria-selected', 'true');
    // A server page: no entity rail.
    expect(screen.queryByTestId('repomgr-item-core')).not.toBeInTheDocument();
  });

  it('renders no Authorizations tab on a read-only instance', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([]);
    render(<RepoManager {...baseProps} serverReadOnly />);
    await screen.findByTestId('repomgr-sessions');
    expect(screen.queryByTestId('repomgr-oauth')).not.toBeInTheDocument();
  });

  it('renders no badge at zero, and none when the count cannot be read', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess('dead')] });
    const { unmount } = render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-sessions');
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalled());
    expect(screen.queryByTestId('repomgr-sessions-badge')).not.toBeInTheDocument();
    unmount();

    vi.mocked(api.listClientSessions).mockRejectedValue(new Error('503'));
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('repomgr-sessions');
    // A badge we cannot vouch for would read as "nobody is connected".
    expect(screen.queryByTestId('repomgr-sessions-badge')).not.toBeInTheDocument();
  });

  it('has no tab strip at all with zero repositories', async () => {
    render(<RepoManager {...baseProps} repos={[]} currentRepo="" />);
    await waitFor(() => expect(screen.getByTestId('step-source')).toBeInTheDocument());
    expect(screen.queryByTestId('repomgr-overview')).not.toBeInTheDocument();
    expect(screen.queryByTestId('repomgr-sessions')).not.toBeInTheDocument();
  });

  // The badge counts from a page the server BOUNDS. When that page was cut the
  // number is a lower bound, and a bare count beside a table that admits it is
  // showing "N of more" would be the one dishonest number on the screen — the
  // exact silent truncation the limit work exists to prevent.
  describe('RepoManager live badge truncation', () => {
    const POLICY = { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 };

    it('marks the count when the page was truncated', async () => {
      vi.mocked(api.listClientSessions).mockResolvedValue({
        truncated: true, policy: POLICY, sessions: [sess('live'), sess('live'), sess('dead')],
      });
      render(<RepoManager {...baseProps} />);
      const badge = await screen.findByTestId('repomgr-sessions-badge');
      // Two live of three shown, and more exist: "at least two".
      expect(badge).toHaveTextContent('2+');
    });

    it('renders the exact count when the list is exhausted', async () => {
      vi.mocked(api.listClientSessions).mockResolvedValue({
        truncated: false, policy: POLICY, sessions: [sess('live'), sess('live')],
      });
      render(<RepoManager {...baseProps} />);
      const badge = await screen.findByTestId('repomgr-sessions-badge');
      expect(badge).toHaveTextContent('2');
      expect(badge).not.toHaveTextContent('+');
    });

    // The marker must follow the DATA. A "+" left over from an earlier truncated
    // read would claim there is more after the condition has gone.
    it('clears the marker when a later read is not truncated', async () => {
      installFakeEventSource();
      vi.mocked(api.listClientSessions).mockResolvedValue({
        truncated: true, policy: POLICY, sessions: [sess('live')],
      });
      render(<RepoManager {...baseProps} />);
      await waitFor(() => expect(screen.getByTestId('repomgr-sessions-badge')).toHaveTextContent('1+'));

      vi.mocked(api.listClientSessions).mockResolvedValue({
        truncated: false, policy: POLICY, sessions: [sess('live')],
      });
      await act(async () => { window.dispatchEvent(new Event('focus')); });
      await waitFor(() => expect(screen.getByTestId('repomgr-sessions-badge')).not.toHaveTextContent('+'));
    });

    // The Sessions page owns the number while it is open and reports through
    // onLiveCount — so the marker has to travel that path too, not just the
    // header's own read.
    it('marks the count reported by the Sessions page', async () => {
      installFakeEventSource();
      vi.mocked(api.listClientSessions).mockResolvedValue({
        truncated: true, policy: POLICY, sessions: [sess('live'), sess('live'), sess('live')],
      });
      render(<RepoManager {...baseProps} />);
      fireEvent.click(await screen.findByTestId('repomgr-sessions'));
      await screen.findByTestId('manage-sessions');
      await waitFor(() => expect(screen.getByTestId('repomgr-sessions-badge')).toHaveTextContent('3+'));
    });
  });
});

