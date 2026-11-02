import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { factsetView } from './factsetOptions';
import { factsetBase, firstParams } from './factsetApi';

// A reload restores the Library list and the context it was taken in from the
// URL (listUrl.ts), the URL beats the localStorage another tab wrote, and a
// context or branch that is gone falls back to today's boot and drops the keys
// that depended on it. With factset=1 the FactSet is re-created: the same
// first call as before the reload.
//
// Sabotage, one run each (against a commit): read localStorage before the URL
// in the boot; skip the branch switch in useListUrl; restore the list without
// checking the branch exists (abandon is a no-op); keep the old L5 clear of
// factset=1 (store constructor); drop the arm() step.

const EMBEDDED = {
  head: 'emb123', branch: 'machine/test', index_commit: 'emb123',
  embeddings_enabled: false, ontology_root: '',
  index_state: 'ready', index_done: 0, index_total: 0, index_percent: 100,
};
const EXP_STATUS = {
  ...EMBEDDED, branch: 'exp/try', writable: true,
  experiment: { name: 'try', parent: 'machine/test', fork_commit: 'f0rk' },
};

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    fetchVersion: vi.fn().mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly: false }),
    api: {
      getFleet: vi.fn().mockRejectedValue(new Error('not in a fleet')),
      listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(),
      lensSearch: vi.fn(), listLensFacts: vi.fn(), lensBrowse: vi.fn(),
    },
  };
});

type Mocks = Record<string, ReturnType<typeof vi.fn>>;
async function apiMock(): Promise<Mocks> {
  const { api } = await import('./api');
  return api as unknown as Mocks;
}

const repoRow = (name: string, uid: string) => ({ name, uid, index_state: 'ready', index_done: 0, index_total: 0 });
const LENS = {
  uid: 'L1', name: 'eng', write: { uid: 'u-a', name: 'alpha' },
  reads: [{ uid: 'u-a', name: 'alpha', branch: 'machine/test' }, { uid: 'u-b', name: 'beta', branch: 'machine/test' }],
  write_branch: EMBEDDED,
};

async function primeApi() {
  const api = await apiMock();
  api.repos.mockResolvedValue([repoRow('alpha', 'u-a'), repoRow('beta', 'u-b')]);
  api.listLenses.mockResolvedValue([LENS]);
  api.getLens.mockResolvedValue(LENS);
  api.listArchived.mockResolvedValue([]);
  api.getRepo.mockImplementation(async (name: string) => ({ name, read_branch: 'machine/test', branch: EMBEDDED }));
  api.status.mockImplementation(async (_repo: string, branch: string) => {
    if (branch === 'exp/try') return EXP_STATUS;
    if (branch === 'machine/test') return EMBEDDED;
    throw new Error('HTTP 404');
  });
  api.getOrigin.mockResolvedValue(null);
  api.browse.mockResolvedValue({ path: 'kb', children: [] });
  api.recent.mockResolvedValue({ facts: [], total: 0 });
  api.search.mockResolvedValue({ results: [] });
  api.lensSearch.mockResolvedValue([]);
  api.listLensFacts.mockResolvedValue({ facts: [], total: 0 });
  api.lensBrowse.mockResolvedValue({ children: [] });
  api.stats.mockResolvedValue({ total: 0, domains: {}, entities: {} });
  api.activity.mockResolvedValue({ last_commit: '', total: 0, changes_7d: 0, changes_30d: 0, changes_90d: 0 });
  api.completions.mockResolvedValue({});
  api.listBranchNames.mockResolvedValue(['machine/test']);
  return api;
}

const remember = (ctx: object) => localStorage.setItem('knomit.context', JSON.stringify(ctx));
const query = () => new URLSearchParams(window.location.search);
const firstCall = () => {
  const v = factsetView.get();
  return v.search ? `${factsetBase(v.search.target)}?${firstParams(v.search, v.options)}` : null;
};

async function typeSearch(text: string) {
  const input = await screen.findByPlaceholderText(/Filter\.\.\./);
  fireEvent.focus(input);
  fireEvent.change(input, { target: { value: text } });
  fireEvent.keyDown(input, { key: 'Enter' });
}

describe('the list search in the URL', () => {
  beforeEach(() => {
    installFakeEventSource(); localStorage.clear();
    // The FactSet view's first call goes through fetch; hold it open.
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
  });
  afterEach(() => {
    cleanup();
    factsetView.close();
    uninstallFakeEventSource(); vi.clearAllMocks(); vi.unstubAllGlobals(); localStorage.clear();
  });

  it('repo: the URL context beats a localStorage context another tab wrote, and the list comes back', async () => {
    const api = await primeApi();
    remember({ kind: 'repo', repo: 'beta' }); // another tab's last write
    window.history.replaceState(null, '', '/?ctx_repo=u-a&ctx_branch=machine%2Ftest&l_q=goroutine&l_f=type%3Apolicy');

    render(<App />);

    await waitFor(() => expect(api.search).toHaveBeenCalled());
    const [repo, branch, text, , , opts] = api.search.mock.calls.at(-1)!;
    expect([repo, branch, text]).toEqual(['alpha', 'machine/test', 'goroutine']);
    expect((opts as { types?: string[] }).types).toEqual(['policy']);
    // Written back unchanged, under the stable uid.
    expect(query().get('ctx_repo')).toBe('u-a');
    expect(query().getAll('l_f')).toEqual(['type:policy']);
  });

  it('a reload on an exp/ branch keeps that branch and since_fork', async () => {
    const api = await primeApi();
    window.history.replaceState(null, '', '/?ctx_repo=u-a&ctx_branch=exp%2Ftry&l_since_fork=1');

    render(<App />);

    await waitFor(() => expect(api.recent.mock.calls.some(c => c[1] === 'exp/try')).toBe(true));
    const call = api.recent.mock.calls.filter(c => c[1] === 'exp/try').at(-1)!;
    expect(call[0]).toBe('alpha');
    expect((call[6] as { sinceFork?: boolean }).sinceFork).toBe(true);
    expect(api.status).toHaveBeenCalledWith('alpha', 'exp/try');
    await waitFor(() => expect(query().get('l_since_fork')).toBe('1'));
    expect(query().get('ctx_branch')).toBe('exp/try');
  });

  it('a repo that is gone falls back to localStorage and drops the list and FactSet keys', async () => {
    const api = await primeApi();
    remember({ kind: 'repo', repo: 'beta' });
    window.history.replaceState(null, '', '/?ctx_repo=u-gone&l_q=stale&factset=1&x=1');

    render(<App />);

    await waitFor(() => expect(query().get('ctx_repo')).toBe('u-b'));
    expect(query().get('l_q')).toBeNull();
    expect(query().get('factset')).toBeNull();
    expect(query().get('x')).toBe('1'); // not ours: kept
    expect(factsetView.get().pending).toBe(false);
    expect(api.search.mock.calls.some(c => c[2] === 'stale')).toBe(false);
  });

  it('a branch that is gone falls back to the read branch and drops the dependent keys', async () => {
    const api = await primeApi();
    window.history.replaceState(null, '', '/?ctx_repo=u-a&ctx_branch=exp%2Fgone&l_q=stale&factset=1');

    render(<App />);

    await waitFor(() => expect(api.status).toHaveBeenCalledWith('alpha', 'exp/gone'));
    await waitFor(() => expect(query().get('ctx_branch')).toBe('machine/test'));
    expect(query().get('ctx_repo')).toBe('u-a');
    expect(query().get('l_q')).toBeNull();
    expect(query().get('factset')).toBeNull();
    expect(api.search.mock.calls.some(c => c[2] === 'stale')).toBe(false);
  });

  it('a lens that is gone falls back to localStorage and drops the dependent keys', async () => {
    const api = await primeApi();
    api.listLenses.mockResolvedValue([]);
    remember({ kind: 'repo', repo: 'alpha' });
    window.history.replaceState(null, '', '/?ctx_lens=L-gone&l_q=stale&factset=1');

    render(<App />);

    await waitFor(() => expect(query().get('ctx_repo')).toBe('u-a'));
    expect(query().get('ctx_lens')).toBeNull();
    expect(query().get('l_q')).toBeNull();
    expect(query().get('factset')).toBeNull();
    expect(api.getLens).not.toHaveBeenCalled();
  });

  it('repo: a reload with factset=1 re-creates the same first call', async () => {
    await primeApi();
    remember({ kind: 'repo', repo: 'alpha' });
    render(<App />);
    await waitFor(() => expect(query().get('ctx_repo')).toBe('u-a'));
    await typeSearch('goroutine');
    await waitFor(() => expect(query().get('l_q')).toBe('goroutine'));
    fireEvent.click(await screen.findByTestId('library-factset'));
    const before = firstCall();
    expect(before).toContain('q=goroutine');
    expect(query().get('factset')).toBe('1');

    // The reload: a new mount reading the URL the first one wrote; the
    // in-memory FactSet is gone with the page.
    cleanup();
    remember({ kind: 'repo', repo: 'beta' }); // and another tab moved on
    render(<App />);

    await waitFor(() => expect(firstCall()).not.toBeNull());
    expect(firstCall()).toBe(before);
    expect(factsetView.get().search!.target).toEqual({ kind: 'repo', repo: 'alpha', branch: 'machine/test' });
  });

  it('lens: a reload with factset=1 re-creates the same first call', async () => {
    const api = await primeApi();
    remember({ kind: 'lens', name: 'eng' });
    render(<App />);
    await waitFor(() => expect(query().get('ctx_lens')).toBe('L1'));
    await typeSearch('goroutine');
    await waitFor(() => expect(query().get('l_q')).toBe('goroutine'));
    await waitFor(() => expect(api.lensSearch).toHaveBeenCalled());
    fireEvent.click(await screen.findByTestId('library-factset'));
    const before = firstCall();
    expect(before).toContain('/lenses/eng/');
    expect(before).toContain('q=goroutine');

    cleanup();
    remember({ kind: 'repo', repo: 'beta' });
    render(<App />);

    await waitFor(() => expect(firstCall()).not.toBeNull());
    expect(firstCall()).toBe(before);
    expect(api.listLenses).toHaveBeenCalled();
  });
});
