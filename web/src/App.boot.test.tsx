import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { BOOT_POLL_MS } from './bootStatus';
import type { BootStatus } from './bootStatus';

// Boot used to take four dependent round trips before anything rendered, and
// the third (a second GET /repos/{repo} purely for agent_branch) was waste the
// step-2 response already answered. These cover the collapsed path: the
// remembered context is fetched speculatively, in parallel with the list, and
// its embedded branch root ends the boot in one hop.

const EMBEDDED = {
  head: 'emb123', branch: 'machine/test', index_commit: 'emb123',
  embeddings_enabled: false, ontology_root: '',
  index_state: 'ready', index_done: 0, index_total: 0, index_percent: 100,
};
const FETCHED = { ...EMBEDDED, head: 'fetched9', index_commit: 'fetched9' };

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

const repoRow = (name: string) => ({ name, index_state: 'ready', index_done: 0, index_total: 0 });

async function primeApi(repos: unknown[]) {
  const api = await apiMock();
  api.repos.mockResolvedValue(repos);
  api.listLenses.mockResolvedValue([]);
  api.listArchived.mockResolvedValue([]);
  api.status.mockResolvedValue(FETCHED);
  api.getOrigin.mockResolvedValue(null);
  api.browse.mockResolvedValue({ path: 'kb', children: [] });
  api.recent.mockResolvedValue({ facts: [], total: 0 });
  api.search.mockResolvedValue({ results: [] });
  api.stats.mockResolvedValue({ total: 0, domains: {}, entities: {} });
  api.activity.mockResolvedValue({ last_commit: '', total: 0, changes_7d: 0, changes_30d: 0, changes_90d: 0 });
  api.completions.mockResolvedValue({});
  api.listBranchNames.mockResolvedValue(['machine/test']);
  return api;
}

function rememberRepo(name: string) {
  localStorage.setItem('knomit.context', JSON.stringify({ kind: 'repo', repo: name }));
}

describe('App boot', () => {
  beforeEach(() => { installFakeEventSource(); localStorage.clear(); });
  afterEach(() => { uninstallFakeEventSource(); vi.clearAllMocks(); localStorage.clear(); });

  it('fires getRepo for the remembered repo BEFORE the repo list resolves', async () => {
    const api = await primeApi([repoRow('alpha')]);
    rememberRepo('alpha');

    // Hold the list open so the ordering is observable rather than a race.
    let releaseList!: (v: unknown) => void;
    api.repos.mockReturnValue(new Promise((res) => { releaseList = res; }));
    api.getRepo.mockResolvedValue({ name: 'alpha', read_branch: 'machine/test', branch: EMBEDDED });

    render(<App />);

    // The speculative fetch left in the same tick as the list, not after it.
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('alpha'));
    expect(api.repos).toHaveBeenCalledTimes(1);

    await act(async () => { releaseList([repoRow('alpha')]); });
  });

  it('uses the embedded branch root once the list confirms, with no second getRepo', async () => {
    const api = await primeApi([repoRow('alpha')]);
    rememberRepo('alpha');
    api.getRepo.mockResolvedValue({ name: 'alpha', read_branch: 'machine/test', branch: EMBEDDED });

    render(<App />);

    // Boot ends: the branch is known, so the boot screen is gone.
    await waitFor(() => expect(screen.queryByTestId('boot-screen')).toBeNull());

    // One request for the repo, and NO branch fetch at all.
    expect(api.getRepo).toHaveBeenCalledTimes(1);
    expect(api.status).not.toHaveBeenCalled();
  });

  it('falls back to a branch fetch when the server did not embed one', async () => {
    const api = await primeApi([repoRow('alpha')]);
    rememberRepo('alpha');
    api.getRepo.mockResolvedValue({ name: 'alpha', read_branch: 'machine/test' }); // no embed

    render(<App />);

    await waitFor(() => expect(screen.queryByTestId('boot-screen')).toBeNull());
    expect(api.status).toHaveBeenCalledWith('alpha', 'machine/test');
  });

  it('discards the speculative result when the list picks a different repo', async () => {
    // The remembered repo is gone (archived out from under the tab).
    const api = await primeApi([repoRow('beta')]);
    rememberRepo('alpha');
    api.getRepo.mockImplementation(async (name: string) => {
      if (name === 'alpha') return { name: 'alpha', read_branch: 'stale/branch', branch: { ...EMBEDDED, branch: 'stale/branch', head: 'STALE' } };
      return { name: 'beta', read_branch: 'machine/test', branch: EMBEDDED };
    });

    render(<App />);

    await waitFor(() => expect(screen.queryByTestId('boot-screen')).toBeNull());

    // THIS is the load-bearing line. The speculative response was for alpha;
    // the list picked beta, so the bootstrap must have gone back to the server
    // for beta rather than reusing what it already had in hand. Without the
    // confirmation check it would reuse the alpha response and the app would
    // boot beta on alpha's branch.
    expect(api.getRepo).toHaveBeenCalledWith('beta');
    // Corollary, and much weaker on its own: alpha's stale branch never
    // reaches a status call. This holds trivially on several wrong
    // implementations, so it is a backstop, not the assertion.
    const statusCalls = api.status.mock.calls.filter((c: unknown[]) => c[1] === 'stale/branch');
    expect(statusCalls).toHaveLength(0);
  });

  it('feeds a remembered lens s embedded write_branch straight into status', async () => {
    const api = await primeApi([repoRow('alpha')]);
    localStorage.setItem('knomit.context', JSON.stringify({ kind: 'lens', name: 'eng' }));
    api.getLens.mockResolvedValue({
      name: 'eng', write: { uid: 'u1', name: 'alpha' },
      reads: [{ uid: 'u1', name: 'alpha', branch: 'machine/test' }],
      write_branch: EMBEDDED,
    });

    render(<App />);

    await waitFor(() => expect(screen.queryByTestId('boot-screen')).toBeNull());
    // The lens carried the branch, so no branch fetch was needed for it.
    expect(api.getLens).toHaveBeenCalledWith('eng');
    expect(api.status).not.toHaveBeenCalled();
  });

  it('shows the boot screen naming the phase while the list is in flight', async () => {
    const api = await primeApi([repoRow('alpha')]);
    api.repos.mockReturnValue(new Promise(() => {})); // never resolves

    render(<App />);

    expect(screen.getByTestId('boot-screen')).toBeInTheDocument();
    expect(screen.getByTestId('boot-phase')).toHaveTextContent('Connecting…');
  });

  // DESKTOP FIRST LAUNCH. /config.js sets __KNOMIT_BOOTING__ because the
  // knomit server does not exist yet — on a cold install it is minutes away,
  // fetching model artifacts. Two things must hold, and the second is the bug
  // this replaced: the screen has to NAME what it is waiting on, and the app
  // must not touch the API until there is one. A request issued now resolves
  // against the webview origin, whose SPA fallback answers 200 with index.html,
  // and HTML-where-JSON-was-expected is indistinguishable from a real failure —
  // so the app retried forever and never recovered, even once the server came up.
  describe('desktop, server still booting', () => {
    let statuses: BootStatus[] = [];
    beforeEach(() => {
      (window as Window & { __KNOMIT_BOOTING__?: boolean }).__KNOMIT_BOOTING__ = true;
      vi.spyOn(globalThis, 'fetch').mockImplementation(async () => {
        const next = statuses.length > 1 ? statuses.shift()! : statuses[0];
        return { ok: true, json: async () => next } as Response;
      });
    });
    afterEach(() => {
      delete (window as Window & { __KNOMIT_BOOTING__?: boolean }).__KNOMIT_BOOTING__;
      delete (window as Window & { __KNOMIT_API_BASE__?: string }).__KNOMIT_API_BASE__;
    });

    it('names the phase and asks the API for NOTHING until the server is up', async () => {
      const api = await primeApi([repoRow('alpha')]);
      statuses = [{ ready: false, phase: 'downloading-models' }];

      render(<App />);

      await waitFor(() =>
        expect(screen.getByTestId('boot-phase')).toHaveTextContent('Downloading models…'));
      // The whole point. Before the gate this was called immediately and got
      // index.html back with a 200.
      expect(api.repos).not.toHaveBeenCalled();
    });

    it('adopts the reported API base and continues the boot, with no reload', async () => {
      const api = await primeApi([repoRow('alpha')]);
      statuses = [
        { ready: false, phase: 'downloading-models' },
        { ready: true, phase: 'ready', api_base: 'http://127.0.0.1:54321' },
      ];

      render(<App />);

      // One BOOT_POLL_MS has to elapse before the second status arrives, and
      // that is longer than waitFor's 1 s default — so this waits for the real
      // interval rather than pretending the transition is instant.
      const afterOnePoll = { timeout: BOOT_POLL_MS + 2000 };
      await waitFor(() => expect(api.repos).toHaveBeenCalled(), afterOnePoll);
      expect((window as Window & { __KNOMIT_API_BASE__?: string }).__KNOMIT_API_BASE__)
        .toBe('http://127.0.0.1:54321');
      await waitFor(() => expect(screen.queryByTestId('boot-screen')).toBeNull());
    });

    it('shows a failed desktop boot as a failure, not an endless retry', async () => {
      const api = await primeApi([repoRow('alpha')]);
      statuses = [{ ready: false, phase: 'failed', error: 'embedder init failed' }];

      render(<App />);

      await waitFor(() =>
        expect(screen.getByTestId('boot-phase')).toHaveTextContent('Could not start knomit'));
      expect(screen.getByTestId('boot-error')).toHaveTextContent('embedder init failed');
      expect(api.repos).not.toHaveBeenCalled();
    });
  });

  it('still renders the no-repos empty state, never the boot screen', async () => {
    await primeApi([]);

    render(<App />);

    await waitFor(() => expect(screen.getByTestId('no-repos')).toBeInTheDocument());
    expect(screen.queryByTestId('boot-screen')).toBeNull();
  });
});
