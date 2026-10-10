import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource, streamsMatching, latestStream } from './testEventSource';
import { __resetRepoCreatesForTest } from './useRepoCreates';

// The facets of a target: Browse, Activity and Settings, as tabs in the top
// bar. facetsFor decides which exist; the facet is remembered under
// knomit.facet; Settings and Activity are the target's own pages, not Manage's;
// the browse keys answer only on Browse; and a connect commit holds every way
// off its page.

const STATUS = {
  head: 'abc1234', branch: 'machine/test', embeddings_enabled: false, ontology_root: 'kb',
  index_state: 'ready', index_done: 0, index_total: 0, index_percent: 100,
};
const POLICY = { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 };
const DESCRIPTIONS: Record<string, string> = { 'ops-kb': 'ops runbooks', 'core-kb': 'core notes' };

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    fetchVersion: vi.fn(),
    createSession: vi.fn(),
    streamTest: vi.fn(() => () => {}),
    streamPreview: vi.fn(() => () => {}),
    streamApply: vi.fn(),
    streamCommit: vi.fn(),
    deleteSession: vi.fn().mockResolvedValue(undefined),
    api: {
      listClientSessions: vi.fn(),
      listOAuthPending: vi.fn().mockResolvedValue(null),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(),
      listRepoCreates: vi.fn(), listExperiments: vi.fn(), listPushedBranches: vi.fn(),
    },
  };
});

async function primeApi({ readOnly = false } = {}) {
  const mod = await import('./api');
  const m = mod.api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  vi.mocked(mod.fetchVersion).mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly } as Awaited<ReturnType<typeof mod.fetchVersion>>);
  m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }]);
  m.listLenses.mockResolvedValue([]);
  m.listArchived.mockResolvedValue([]);
  m.getAgentBranch.mockResolvedValue('machine/test');
  m.status.mockResolvedValue(STATUS);
  m.getOrigin.mockResolvedValue({ name: 'origin', url: 'https://example.com/kb.git', branch: 'main', auth_method: 'none', last_status: 'ok', last_error: null });
  m.browse.mockResolvedValue({ path: 'kb', children: [] });
  m.recent.mockResolvedValue({ facts: [], total: 0 });
  m.search.mockResolvedValue({ results: [] });
  m.stats.mockResolvedValue(null);
  m.activity.mockResolvedValue(null);
  m.completions.mockResolvedValue([]);
  m.getRepo.mockImplementation(async (name: string) => ({ name, description: DESCRIPTIONS[name] ?? '' }));
  m.listBranchNames.mockResolvedValue(['main', 'machine/test']);
  m.listRepoCreates.mockResolvedValue([]);
  m.listExperiments.mockResolvedValue({ experiments: [] });
  m.listPushedBranches?.mockResolvedValue?.([]);
  m.listClientSessions.mockResolvedValue({ truncated: false, policy: POLICY, sessions: [] });
  return { m, mod };
}

async function mountApp() {
  render(<App />);
  await screen.findByTestId('status-footer');
  await screen.findByTestId('target-label');
}

const tabNames = () => screen.getAllByRole('tab').map(t => t.textContent);
const openStreams = (s: string) => streamsMatching(s).filter(es => !es.closedByClient).length;

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem('knomit.context', JSON.stringify({ kind: 'repo', repo: 'ops-kb' }));
  __resetRepoCreatesForTest();
  installFakeEventSource();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  vi.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => {
  // cleanup FIRST: afterEach runs in stack order, so test-setup's own cleanup
  // would otherwise unmount after the EventSource baseline is restored.
  cleanup();
  vi.restoreAllMocks();
  uninstallFakeEventSource();
  vi.clearAllMocks();
});

describe('facet tabs', () => {
  it('are exactly Browse, Activity, Settings for an available repo', async () => {
    await primeApi();
    await mountApp();
    expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
    expect(screen.getByTestId('facet-tab-browse')).toHaveAttribute('aria-selected', 'true');
  });

  it('Settings shows the CURRENT repo\'s own settings page', async () => {
    const { m } = await primeApi();
    await mountApp();
    m.getRepo.mockClear();
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    expect(await screen.findByTestId('repo-settings')).toBeInTheDocument();
    expect(await screen.findByText('ops runbooks')).toBeInTheDocument();
    expect(m.getRepo).toHaveBeenCalledWith('ops-kb');
    // Not Manage: the body is the facet, and the Library is gone with Browse.
    expect(screen.queryByTestId('manage-surface')).toBeNull();
    expect(screen.queryByTestId('library-splitter')).toBeNull();
  });

  it('keeps Settings across a rail switch, and never unmounts the shell to do it', async () => {
    localStorage.setItem('knomit.rail', '1');
    const { m } = await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByText('ops runbooks');

    // Hold core-kb's status so the switch has a window with no branch: the
    // window in which the boot screen used to blank the shell.
    let release!: () => void;
    m.status.mockImplementation(() => new Promise(res => { release = () => res(STATUS); }));
    let railLost = false;
    const observer = new MutationObserver(() => {
      if (!document.querySelector('[data-testid=target-rail]')) railLost = true;
    });
    observer.observe(document.body, { childList: true, subtree: true });
    try {
      fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
      expect(await screen.findByText('core notes')).toBeInTheDocument();
      expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
      await act(async () => { release(); });
      await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('core-kb'));
    } finally {
      observer.disconnect();
    }
    expect(railLost).toBe(false);
    expect(screen.queryByTestId('boot-phase')).toBeNull();
  });

  it('shows the Browse placeholder, and no panels, while a switched-to repo has no branch yet', async () => {
    localStorage.setItem('knomit.rail', '1');
    const { m } = await primeApi();
    await mountApp();
    expect(screen.getByTestId('library-splitter')).toBeInTheDocument();
    m.status.mockImplementation(() => new Promise(() => {}));
    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    expect(await screen.findByTestId('browse-opening')).toHaveTextContent('Opening core-kb…');
    expect(screen.queryByTestId('library-splitter')).toBeNull();
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
  });

  it('comes back on the facet it was left on after a reload', async () => {
    await primeApi();
    const first = render(<App />);
    await screen.findByTestId('target-label');
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await screen.findByTestId('activity-facet');
    await waitFor(() => expect(localStorage.getItem('knomit.facet')).toBe('activity'));
    first.unmount();

    render(<App />);
    expect(await screen.findByTestId('activity-facet')).toBeInTheDocument();
    expect(screen.getByTestId('facet-tab-activity')).toHaveAttribute('aria-selected', 'true');
  });
});

describe('the browse keys belong to the Browse facet', () => {
  it('/ focuses the filter on Browse (control), and does nothing on Settings', async () => {
    await primeApi();
    await mountApp();
    act(() => { fireEvent.keyDown(window, { key: '/' }); });
    await waitFor(() => expect(document.activeElement?.id).toBe('filter-input'));
    (document.activeElement as HTMLElement).blur();

    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByTestId('repo-settings');
    act(() => { fireEvent.keyDown(window, { key: '/' }); });
    expect(document.activeElement).toBe(document.body);
  });

  it('Backspace on Settings does not go back', async () => {
    const { m } = await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByTestId('repo-settings');
    const browseCalls = m.browse.mock.calls.length;
    act(() => { fireEvent.keyDown(window, { key: 'Backspace' }); });
    await act(async () => { await Promise.resolve(); });
    expect(m.browse.mock.calls.length).toBe(browseCalls);
    expect(document.activeElement).toBe(document.body);
    expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
  });

  it('Escape on Settings changes nothing', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByTestId('repo-settings');
    act(() => { fireEvent.keyDown(window, { key: 'Escape' }); });
    expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
    expect(screen.getByTestId('repo-settings')).toBeInTheDocument();
  });
});

describe('the remote error on the facets', () => {
  async function failSync() {
    await waitFor(() => expect(streamsMatching('/branches/')).toHaveLength(1));
    act(() => { latestStream('/branches/')!.emit('sync_error', { error: 'auth failed' }); });
    await screen.findByTestId('remote-error-banner');
  }

  it('Reconnect… lands on Settings, scrolled to the Remote block', async () => {
    await primeApi();
    const seen: string[] = [];
    vi.spyOn(HTMLElement.prototype, 'scrollIntoView').mockImplementation(function (this: HTMLElement) { seen.push(this.id); });
    await mountApp();
    await failSync();
    fireEvent.click(screen.getByTestId('remote-error-reconnect'));
    expect(await screen.findByTestId('repo-settings')).toBeInTheDocument();
    expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
    await waitFor(() => expect(seen).toContain('remote'));
    expect(screen.getByTestId('toc-remote')).toHaveAttribute('aria-current', 'true');
  });

  it('puts the dot on the Settings tab, not the gear', async () => {
    await primeApi();
    await mountApp();
    expect(screen.queryByTestId('facet-tab-settings-dot')).toBeNull();
    await failSync();
    expect(screen.getByTestId('facet-tab-settings-dot')).toBeInTheDocument();
    expect(screen.getByTestId('toknomitr-manage-btn').querySelector('span')).toBeNull();
  });

  it('puts no dot on the Settings tab on a read-only server, even with the banner up', async () => {
    await primeApi({ readOnly: true });
    await mountApp();
    await failSync();
    expect(screen.getByTestId('facet-tab-settings')).toBeInTheDocument();
    expect(screen.queryByTestId('facet-tab-settings-dot')).toBeNull();
  });
});

describe('Activity', () => {
  it('asks for the target\'s sessions, holds one stream, and drops it on leaving', async () => {
    const { m } = await primeApi();
    await mountApp();
    expect(openStreams('/sessions/events')).toBe(0);
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await screen.findByTestId('activity-toolbar');
    await waitFor(() => expect(m.listClientSessions).toHaveBeenCalledWith({ binding: 'repo:u-ops', includeHidden: false }));
    await waitFor(() => expect(openStreams('/sessions/events')).toBe(1));

    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByTestId('repo-settings');
    expect(openStreams('/sessions/events')).toBe(0);
  });

  it('never has two sessions streams open when Manage opens over it', async () => {
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await waitFor(() => expect(openStreams('/sessions/events')).toBe(1));
    await act(async () => { fireEvent.click(screen.getByTestId('toknomitr-manage-btn')); });
    await screen.findByTestId('manage-surface');
    // Manage's badge reader owns the one stream now; Activity's is closed.
    await waitFor(() => expect(openStreams('/sessions/events')).toBe(1));
    expect(screen.queryByTestId('activity-facet')).toBeNull();
  });
});

describe('a connect commit holds every way off its page', () => {
  async function startCommit(mod: typeof import('./api')) {
    const m = mod as unknown as Record<string, ReturnType<typeof vi.fn>>;
    m.createSession.mockResolvedValue({ session_id: 'sess-lock' });
    m.streamTest.mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { branches: ['main'], agent_branches: [], default_branch: 'main', matched_agent: '', history: 'shared', remote_fact_count: 1, local_fact_count: 1 } }));
      return () => {};
    });
    m.streamPreview.mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 1, remote_only: 0, shared_path: 0, dead_refs_found: 0 } }));
      return () => {};
    });
    m.streamApply.mockImplementation(async (_r: string, _s: string, _st: string, _b: string | undefined, onEvent: (e: unknown) => void) => {
      onEvent({ phase: 'done', result: { total_facts: 0, from_local: 0, from_remote: 0, overwrites: 0 } });
    });
    m.streamCommit.mockImplementation(() => new Promise(() => {}));
  }

  async function openWizard(m: Record<string, ReturnType<typeof vi.fn>>) {
    m.getOrigin.mockResolvedValue(null);
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByTestId('repo-settings');
    await act(async () => { fireEvent.click(await screen.findByTestId('remote-connect')); });
    await act(async () => { fireEvent.change(await screen.findByTestId('wizard-url'), { target: { value: 'https://example.com/repo.git' } }); });
    await act(async () => { fireEvent.click(screen.getByTestId('wizard-test')); });
  }

  it('the rail, the facet tabs and the gear refuse while a connect commit is writing', async () => {
    localStorage.setItem('knomit.rail', '1');
    const { m, mod } = await primeApi();
    await startCommit(mod);
    await mountApp();
    await openWizard(m);
    // Positive control: before the commit, everything answers.
    expect(screen.getByTestId('rail-repo-core-kb')).toBeEnabled();
    expect(screen.getByTestId('facet-tab-browse')).toBeEnabled();
    expect(screen.getByTestId('toknomitr-manage-btn')).toBeEnabled();

    await act(async () => { fireEvent.click(await screen.findByTestId('wizard-connect')); });
    await waitFor(() => expect((screen.getByTestId('wizard-crumb-back') as HTMLButtonElement).disabled).toBe(true));

    expect(screen.getByTestId('rail-repo-core-kb')).toBeDisabled();
    expect(screen.getByTestId('facet-tab-browse')).toBeDisabled();
    expect(screen.getByTestId('facet-tab-activity')).toBeDisabled();
    expect(screen.getByTestId('toknomitr-manage-btn')).toBeDisabled();

    m.status.mockClear();
    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    fireEvent.click(screen.getByTestId('facet-tab-browse'));
    fireEvent.click(screen.getByTestId('toknomitr-manage-btn'));
    await act(async () => { await Promise.resolve(); });
    expect(m.status).not.toHaveBeenCalledWith('core-kb', expect.anything());
    expect(screen.getByTestId('remote-connect-wizard')).toBeInTheDocument();
    expect(screen.queryByTestId('manage-surface')).toBeNull();
    // ⌘B still works: hiding the rail unmounts nothing in the body.
    act(() => { fireEvent.keyDown(window, { key: 'b', metaKey: true }); });
    expect(screen.queryByTestId('target-rail')).toBeNull();
    expect(screen.getByTestId('remote-connect-wizard')).toBeInTheDocument();
  });

  // The lock must hold from the moment it PAINTS. The busy flag is relayed by
  // layout effects (wizard → RepoSettings → App); a passive hop would leave a
  // window in which the reader could see they may not leave, click a rail row,
  // and leave mid-write. act() collapses that window, so this uses NATIVE
  // events and clicks the rail row from a MutationObserver the instant the
  // lock paints.
  it('refuses a rail click made in the same task the commit lock paints in', async () => {
    localStorage.setItem('knomit.rail', '1');
    const { m, mod } = await primeApi();
    await startCommit(mod);
    await mountApp();
    await openWizard(m);
    m.status.mockClear();

    const crumb = () => document.querySelector('[data-testid=wizard-crumb-back]') as HTMLButtonElement | null;
    let wizardsWhenLockPainted = -1;
    let observer: MutationObserver | undefined;
    const fired = new Promise<void>((done, fail) => {
      const timer = setTimeout(() => fail(new Error('the commit lock never painted: wizard-crumb-back was never disabled')), 4000);
      observer = new MutationObserver(() => {
        if (!crumb()?.disabled) return;
        clearTimeout(timer);
        observer?.disconnect();
        wizardsWhenLockPainted = document.querySelectorAll('[data-testid=remote-connect-wizard]').length;
        (document.querySelector('[data-testid=rail-repo-core-kb]') as HTMLElement)
          .dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
        done();
      });
      observer.observe(document.body, { childList: true, subtree: true, attributes: true });
    });
    try {
      (await screen.findByTestId('wizard-connect')).dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
      await fired;
    } finally {
      observer?.disconnect();
    }
    await act(async () => { await Promise.resolve(); });

    // Guards the guard: the click above landed while the wizard was up.
    expect(wizardsWhenLockPainted).toBe(1);
    expect(m.status).not.toHaveBeenCalledWith('core-kb', expect.anything());
    expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
    expect(screen.getByTestId('remote-connect-wizard')).toBeInTheDocument();
  });
});
