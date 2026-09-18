import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource, latestStream } from './testEventSource';

// The index chip and the indexing banner used to go stale for hours: a server
// restart heals every repo in the background, the flip to ready published
// nothing, and the app's repo list is a one-time snapshot. These cover the
// event that closes that gap.

const STATUS = {
  head: 'abc1234', branch: 'machine/test', embeddings_enabled: false, ontology_root: 'kb',
  index_state: 'indexing', index_done: 3, index_total: 10, index_percent: 30,
};

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    fetchVersion: vi.fn().mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly: false }),
    api: {
      listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(),
    },
  };
});

async function apiMock() {
  const { api } = await import('./api');
  return api as unknown as Record<string, ReturnType<typeof vi.fn>>;
}

const repoRow = (name: string, over: Record<string, unknown> = {}) => ({
  name, index_state: 'ready', index_done: 0, index_total: 0, ...over,
});

async function primeApi(repos: unknown[]) {
  const api = await apiMock();
  api.repos.mockResolvedValue(repos);
  api.listLenses.mockResolvedValue([]);
  api.listArchived.mockResolvedValue([]);
  api.getAgentBranch.mockResolvedValue('machine/test');
  api.status.mockResolvedValue(STATUS);
  api.getOrigin.mockResolvedValue(null);
  api.browse.mockResolvedValue({ path: 'kb', children: [] });
  api.recent.mockResolvedValue({ facts: [], total: 0 });
  api.search.mockResolvedValue({ results: [] });
  api.stats.mockResolvedValue(null);
  api.activity.mockResolvedValue(null);
  api.completions.mockResolvedValue([]);
  api.getRepo.mockResolvedValue({ name: 'alpha', description: '' });
  api.listBranchNames.mockResolvedValue(['main', 'machine/test']);
  return api;
}

/** The server-wide index stream, selected by URL — never by index. */
const repoEventStream = () => latestStream('/api/v1/repo-events');

/**
 * Open the repo picker, which is where the per-repo index chip lives.
 *
 * The chip is not on the bar itself — it annotates each row of the repo menu —
 * so a test that never opens the menu can only assert its ABSENCE, which is the
 * vacuous assertion these tests exist to avoid.
 */
async function openRepoMenu() {
  fireEvent.click(await screen.findByTestId('toknomitr-repo-select'));
  await screen.findByTestId('toknomitr-repo-menu');
}

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { uninstallFakeEventSource(); vi.clearAllMocks(); });

describe('index events clear a stale chip', () => {
  it('renders the chip BEFORE the event and removes it after', async () => {
    // TWO repos: the repo picker, which is where the chip lives, does not
    // render when there is nothing to pick. beta is ready, so exactly one chip
    // is expected and "the chip" is unambiguous.
    await primeApi([
      repoRow('alpha', { index_state: 'indexing', index_done: 3, index_total: 10 }),
      repoRow('beta'),
    ]);
    render(<App />);
    await openRepoMenu();

    // The chip must be THERE first. A disappearance-only assertion passes
    // against a chip that never rendered, which is the vacuous version of this
    // test — and the bug is about a chip that is present and should not be.
    const chip = await screen.findByTestId('repo-index-indexing');
    expect(chip).toBeInTheDocument();

    await waitFor(() => expect(repoEventStream()).toBeDefined());
    act(() => {
      repoEventStream()!.emit('index', { repo: 'alpha', state: 'ready', done: 10, total: 10 });
    });

    await waitFor(() => expect(screen.queryByTestId('repo-index-indexing')).toBeNull());
  });

  it('patches only the repo the event names', async () => {
    await primeApi([
      repoRow('alpha', { index_state: 'indexing', index_done: 1, index_total: 10 }),
      repoRow('beta', { index_state: 'indexing', index_done: 2, index_total: 10 }),
    ]);
    render(<App />);
    await openRepoMenu();
    await waitFor(() => expect(screen.getAllByTestId('repo-index-indexing').length).toBe(2));

    await waitFor(() => expect(repoEventStream()).toBeDefined());
    act(() => {
      repoEventStream()!.emit('index', { repo: 'beta', state: 'ready', done: 10, total: 10 });
    });

    // One left, not zero and not two: the event names one repo.
    await waitFor(() => expect(screen.getAllByTestId('repo-index-indexing').length).toBe(1));
  });

  it('does not refetch the list to apply an event', async () => {
    const api = await primeApi([
      repoRow('alpha', { index_state: 'indexing', index_done: 1, index_total: 10 }),
      repoRow('beta'),
    ]);
    render(<App />);
    await openRepoMenu();
    await screen.findByTestId('repo-index-indexing');
    await waitFor(() => expect(repoEventStream()).toBeDefined());
    const before = api.repos.mock.calls.length;

    act(() => {
      repoEventStream()!.emit('index', { repo: 'alpha', state: 'ready', done: 10, total: 10 });
    });
    await waitFor(() => expect(screen.queryByTestId('repo-index-indexing')).toBeNull());

    // The payload IS the update. A refetch per event would be a request per
    // progress tick.
    expect(api.repos.mock.calls.length).toBe(before);
  });
});

describe('index stream reconnect', () => {
  it('refetches the repo list on a RECONNECT, not on the first ready', async () => {
    const api = await primeApi([repoRow('alpha')]);
    render(<App />);
    await waitFor(() => expect(repoEventStream()).toBeDefined());
    await waitFor(() => expect(api.repos).toHaveBeenCalled());
    const afterBootstrap = api.repos.mock.calls.length;

    // The connect-time `ready` must cost nothing — the caller has just read the
    // list. (The fake emits open+ready on construction, as the server does.)
    await act(async () => { await Promise.resolve(); });
    expect(api.repos.mock.calls.length).toBe(afterBootstrap);

    // A SECOND ready is a reconnect: the gap may have swallowed a terminal
    // event, which is broadcast once and never replayed.
    act(() => { repoEventStream()!.emit('ready', {}); });
    await waitFor(() => expect(api.repos.mock.calls.length).toBe(afterBootstrap + 1));
  });

  // THE RACE the generation guard exists for. A refetch is async; an event can
  // land while it is in flight. Without the guard the older refetch resolves
  // last and reinstates the state the event just corrected — the stale chip,
  // restored by the mechanism meant to clear it.
  it('an in-flight refetch must not overwrite a newer event', async () => {
    const api = await primeApi([
      repoRow('alpha', { index_state: 'indexing', index_done: 1, index_total: 10 }),
      repoRow('beta'),
    ]);
    render(<App />);
    await openRepoMenu();
    await screen.findByTestId('repo-index-indexing');
    await waitFor(() => expect(repoEventStream()).toBeDefined());

    // Reconnect dispatches a refetch that we hold open. Its answer is STALE —
    // it still says indexing, which is exactly the case that matters.
    let settle: (v: unknown) => void = () => {};
    api.repos.mockReturnValueOnce(new Promise(resolve => { settle = resolve; }));
    act(() => { repoEventStream()!.emit('ready', {}); });

    // The terminal event arrives while that refetch is still in flight.
    act(() => {
      repoEventStream()!.emit('index', { repo: 'alpha', state: 'ready', done: 10, total: 10 });
    });
    await waitFor(() => expect(screen.queryByTestId('repo-index-indexing')).toBeNull());

    // ...and only now does the older read come back, with the older truth.
    await act(async () => {
      settle([repoRow('alpha', { index_state: 'indexing', index_done: 1, index_total: 10 }), repoRow('beta')]);
    });

    // The chip must STAY gone. This is the assertion the guard buys.
    expect(screen.queryByTestId('repo-index-indexing')).toBeNull();
  });
});

describe('the indexing banner follows the event', () => {
  // The 2 s status poll that used to clear this banner is gone; the event is
  // what clears it now, so this is that path's only cover.
  it('clears the active repo banner on a ready event', async () => {
    await primeApi([repoRow('alpha', { index_state: 'indexing', index_done: 3, index_total: 10 })]);
    render(<App />);
    const banner = await screen.findByTestId('indexing-banner');
    expect(banner).toBeInTheDocument();

    await waitFor(() => expect(repoEventStream()).toBeDefined());
    act(() => {
      repoEventStream()!.emit('index', { repo: 'alpha', state: 'ready', done: 10, total: 10 });
    });

    await waitFor(() => expect(screen.queryByTestId('indexing-banner')).toBeNull());
  });

  it('ignores an event for a DIFFERENT repo', async () => {
    await primeApi([
      repoRow('alpha', { index_state: 'indexing', index_done: 3, index_total: 10 }),
      repoRow('beta', { index_state: 'indexing', index_done: 1, index_total: 10 }),
    ]);
    render(<App />);
    await screen.findByTestId('indexing-banner');

    await waitFor(() => expect(repoEventStream()).toBeDefined());
    act(() => {
      repoEventStream()!.emit('index', { repo: 'beta', state: 'ready', done: 10, total: 10 });
    });

    // The banner speaks for the ACTIVE repo. Another repo reaching ready says
    // nothing about this one, and a banner that cleared on it would be lying.
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByTestId('indexing-banner')).toBeInTheDocument();
  });
});
