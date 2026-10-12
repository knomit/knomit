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
      getFleet: vi.fn().mockRejectedValue(new Error('not in a fleet')), listFleetMembers: vi.fn().mockResolvedValue([]),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(),
      listRepoCreates: vi.fn(), listExperiments: vi.fn(), listPushedBranches: vi.fn(),
      // The drafts and the pinned targets.
      createRepo: vi.fn(), createLens: vi.fn(), updateLens: vi.fn(), archiveRepo: vi.fn(), restoreRepo: vi.fn(), purgeRepo: vi.fn(),
      getRepoCreate: vi.fn(), cancelRepoCreate: vi.fn(), dismissRepoCreate: vi.fn(),
      ontologyPresets: vi.fn(), ontologyPresetYAML: vi.fn(), validateOntology: vi.fn(), ontologySchema: vi.fn(), listTemplates: vi.fn(),
    },
  };
});

const DEV_LENS = { name: 'dev', uid: 'l-dev', write: { uid: 'u-ops', name: 'ops-kb' }, reads: [{ uid: 'u-ops', name: 'ops-kb' }, { uid: 'u-core', name: 'core-kb' }] };

async function primeApi({ readOnly = false, lenses = [] as unknown[] } = {}) {
  const mod = await import('./api');
  const m = mod.api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  vi.mocked(mod.fetchVersion).mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly } as Awaited<ReturnType<typeof mod.fetchVersion>>);
  m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }]);
  m.listLenses.mockResolvedValue(lenses);
  m.getLens.mockImplementation(async (name: string) => lenses.find(l => (l as { name: string }).name === name));
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
  m.getRepoCreate.mockRejectedValue(new Error('not found'));
  m.ontologyPresets.mockResolvedValue([{ name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] }]);
  m.ontologyPresetYAML.mockResolvedValue('id: general\nname: General\ntopics:\n  people:\n');
  m.validateOntology.mockResolvedValue({ ok: true, id: 'general', name: 'General', topics: ['people'], rule_count: 0 });
  m.ontologySchema.mockResolvedValue([]);
  m.listTemplates.mockResolvedValue([]);
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

// The browse keys act on Browse state that is NOT on screen while Settings is:
// the filter, and the nav stack Backspace pops. Each test therefore puts real
// Browse state in place, presses the key on Settings, comes back to Browse and
// finds it untouched — with the same key on Browse as the control that it
// WOULD have changed it.
describe('the browse keys belong to the Browse facet', () => {
  // A free-text filter, committed with Enter (the input blurs itself).
  async function filterBy(text: string) {
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: text } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(screen.getByTestId('browse-toolbar').textContent).toContain(text));
  }
  const filterShown = () => screen.getByTestId('browse-toolbar').textContent ?? '';
  const toSettings = async () => {
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByTestId('repo-settings');
    expect(document.activeElement).toBe(document.body);
  };
  const toBrowse = async () => {
    fireEvent.click(screen.getByTestId('facet-tab-browse'));
    await screen.findByTestId('browse-toolbar');
  };

  it('/ takes the key (and focuses the filter) on Browse; on Settings it leaves the key alone', async () => {
    await primeApi();
    await mountApp();
    let notCancelled = true;
    act(() => { notCancelled = fireEvent.keyDown(window, { key: '/' }); });
    expect(notCancelled).toBe(false);
    await waitFor(() => expect(document.activeElement?.id).toBe('filter-input'));
    (document.activeElement as HTMLElement).blur();

    await toSettings();
    act(() => { notCancelled = fireEvent.keyDown(window, { key: '/' }); });
    // Not swallowed: a "/" on a page with no filter is the browser's (and
    // any field's), not a shortcut for a box that is not there.
    expect(notCancelled).toBe(true);
    expect(document.activeElement).toBe(document.body);
  });

  it('Escape on Settings leaves the Browse filter alone; on Browse it clears it', async () => {
    await primeApi();
    await mountApp();
    await filterBy('abc');

    await toSettings();
    act(() => { fireEvent.keyDown(window, { key: 'Escape' }); });
    expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
    await toBrowse();
    expect(filterShown()).toContain('abc');

    // Control: the same key on Browse clears it.
    act(() => { fireEvent.keyDown(window, { key: 'Escape' }); });
    await waitFor(() => expect(filterShown()).not.toContain('abc'));
  });

  it('Backspace on Settings does not go back; on Browse it does', async () => {
    await primeApi();
    await mountApp();
    await filterBy('abc');
    // Escape on Browse clears the filter AND pushes the filtered view onto the
    // nav stack, so Back has somewhere to go: the filter returns.
    act(() => { fireEvent.keyDown(window, { key: 'Escape' }); });
    await waitFor(() => expect(filterShown()).not.toContain('abc'));

    await toSettings();
    act(() => { fireEvent.keyDown(window, { key: 'Backspace' }); });
    await toBrowse();
    await act(async () => { await Promise.resolve(); });
    expect(filterShown()).not.toContain('abc');

    // Control: Backspace on Browse pops that entry and the filter comes back.
    act(() => { fireEvent.keyDown(window, { key: 'Backspace' }); });
    await waitFor(() => expect(filterShown()).toContain('abc'));
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

// Leaving a lens for its WRITE repo keeps state.repo, so the status bootstrap
// (keyed on the repo) does not re-run: the branch must survive the switch, or
// Browse sits on "Opening…" for good with the branch stream closed.
describe('leaving a lens for its own write repo', () => {
  async function inLens() {
    localStorage.setItem('knomit.context', JSON.stringify({ kind: 'lens', name: 'dev' }));
    localStorage.setItem('knomit.rail', '1');
    const api = await primeApi({ lenses: [DEV_LENS] });
    render(<App />);
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('dev'));
    await screen.findByTestId('library-splitter');
    await waitFor(() => expect(openStreams('/repos/ops-kb/branches/')).toBe(1));
    return api;
  }

  it('Reconnect from the lens lands on the write repo, and its Browse opens', async () => {
    const { m } = await inLens();
    act(() => { latestStream('/branches/')!.emit('sync_error', { error: 'auth failed' }); });
    fireEvent.click(await screen.findByTestId('remote-error-reconnect'));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
    expect(await screen.findByTestId('repo-settings')).toBeInTheDocument();
    const statusCalls = m.status.mock.calls.length;

    fireEvent.click(screen.getByTestId('facet-tab-browse'));
    expect(await screen.findByTestId('library-splitter')).toBeInTheDocument();
    expect(screen.queryByTestId('browse-opening')).toBeNull();
    // The branch is still known, so nothing had to be re-read to get here, and
    // the branch's event stream (the live head) is still open.
    expect(m.status.mock.calls.length).toBe(statusCalls);
    expect(openStreams('/repos/ops-kb/branches/')).toBe(1);
  });

  it('a rail pick of the write repo from Browse keeps Browse open', async () => {
    await inLens();
    fireEvent.click(screen.getByTestId('rail-repo-ops-kb'));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
    expect(screen.getByTestId('library-splitter')).toBeInTheDocument();
    expect(screen.queryByTestId('browse-opening')).toBeNull();
    expect(openStreams('/repos/ops-kb/branches/')).toBe(1);
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

  // Activity is the only sessions reader left (Manage's Sessions tab and its
  // badge went with the global view), so opening Manage over it closes the
  // one stream and opens none.
  it('closes its sessions stream when Manage opens over it, and nothing else opens one', async () => {
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await waitFor(() => expect(openStreams('/sessions/events')).toBe(1));
    await act(async () => { fireEvent.click(screen.getByTestId('toknomitr-manage-btn')); });
    await screen.findByTestId('manage-surface');
    await waitFor(() => expect(openStreams('/sessions/events')).toBe(0));
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

// ── Edge targets and drafts (PR E) ───────────────────────────────────────────
// A target with no store to read is PINNED over the context and has Settings
// alone; leaving it for a normal target restores the facet the reader had. A
// `+` is a draft: the create form as a target, kept while the reader detours.
const Q3 = { id: 'id-q3', name: 'q3-planning', origin: '', archivedAt: '2026-08-12T10:00:00Z' };
const VENDOR = { name: 'vendor-docs', uid: 'u-v', state: 'missing', detail: 'Nothing at /Volumes/ext/vendor-docs.' };

describe('edge targets', () => {
  beforeEach(() => { localStorage.setItem('knomit.rail', '1'); });

  it('E1: from Activity, an archived repo shows only Settings and the archived chip; the next repo restores Activity', async () => {
    const { m } = await primeApi();
    m.listArchived.mockResolvedValue([Q3]);
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await screen.findByTestId('activity-toolbar');

    fireEvent.click(await screen.findByTestId('rail-archived'));
    fireEvent.click(screen.getByTestId('rail-archived-id-q3'));
    await waitFor(() => expect(tabNames()).toEqual(['Settings']));
    expect(screen.getByTestId('target-label')).toHaveTextContent('q3-planning');
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^archived$/);
    expect(screen.getByTestId('archived-settings')).toBeInTheDocument();
    // The context did not move: an archived repo is not something to browse.
    expect(m.status).not.toHaveBeenCalledWith('q3-planning', expect.anything());

    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    expect(await screen.findByTestId('activity-toolbar')).toBeInTheDocument();
    expect(screen.getByTestId('facet-tab-activity')).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByTestId('target-label-chip')).toBeNull();
  });

  it('E2: a repo with no store shows only Settings, the store missing chip and the callout; the next repo restores Activity', async () => {
    const { m } = await primeApi();
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, VENDOR]);
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await screen.findByTestId('activity-toolbar');

    fireEvent.click(screen.getByTestId('rail-repo-vendor-docs'));
    await waitFor(() => expect(tabNames()).toEqual(['Settings']));
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^store missing$/);
    expect(screen.getByTestId('repo-unavailable-detail')).toHaveTextContent('Nothing at /Volumes/ext/vendor-docs.');
    // Nothing reads through the store it does not have.
    expect(m.getRepo).not.toHaveBeenCalledWith('vendor-docs');

    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    expect(await screen.findByTestId('activity-toolbar')).toBeInTheDocument();
  });

  it('archiving a repo with no store pins its archived row, by the id the server returned', async () => {
    const { m } = await primeApi();
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, VENDOR]);
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-repo-vendor-docs'));
    await screen.findByTestId('repo-unavailable-archive');

    const row = { id: 'u-v', name: 'vendor-docs', origin: '', archivedAt: '2026-10-10T00:00:00Z' };
    m.archiveRepo.mockResolvedValue(row);
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }]);
    // A second archived repo of the same name: the pin must be by id.
    m.listArchived.mockResolvedValue([{ ...row, id: 'older', archivedAt: '2026-01-01T00:00:00Z' }, row]);
    fireEvent.click(screen.getByTestId('repo-unavailable-archive'));
    fireEvent.click(screen.getByTestId('repo-unavailable-archive-confirm'));

    expect(await screen.findByTestId('archived-settings')).toBeInTheDocument();
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^archived$/);
    expect(screen.getByTestId('rail-archived-u-v')).toHaveAttribute('aria-current', 'true');
    expect(screen.getByTestId('rail-archived-older').getAttribute('aria-current')).toBeNull();
  });

  // A lens with a mount that has no store: every read under it answers 503, so
  // it is pinned, not entered; and its Settings is the only way out, because
  // the server refuses to archive a repo a lens mounts.
  it('a lens with a broken mount pins to Settings alone, names the missing mount, and comes back once repaired', async () => {
    const { m } = await primeApi();
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, VENDOR]);
    const haunted = {
      name: 'haunted', uid: 'l-h', write: { uid: 'u-core', name: 'core-kb' },
      reads: [{ uid: 'u-core', name: 'core-kb' }, { uid: 'u-v', name: 'vendor-docs' }], write_branch: STATUS,
    };
    const healthy = { name: 'dev', uid: 'l-dev', write: { uid: 'u-ops', name: 'ops-kb' }, reads: [{ uid: 'u-ops', name: 'ops-kb' }], write_branch: STATUS };
    m.listLenses.mockResolvedValue([haunted, healthy]);
    m.getLens.mockImplementation(async (name: string) => (name === 'dev' ? healthy : haunted));
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await screen.findByTestId('activity-toolbar');

    const row = await screen.findByTestId('rail-lens-haunted');
    expect(row).toBeEnabled();
    fireEvent.click(row);
    await waitFor(() => expect(tabNames()).toEqual(['Settings']));
    expect(screen.getByTestId('target-label')).toHaveTextContent('haunted');
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^mount missing$/);
    expect(screen.getByTestId('lens-broken-callout')).toHaveTextContent('vendor-docs');
    expect(screen.getByTestId('lens-broken-mount-vendor-docs')).toHaveTextContent('Nothing at /Volumes/ext/vendor-docs.');
    // Only the missing mount is named, not the healthy one beside it.
    expect(screen.queryByTestId('lens-broken-mount-core-kb')).toBeNull();
    expect(row).toHaveAttribute('aria-current', 'true');
    // The context did not move: the broken lens is never entered (its page
    // re-reads the lens record, which is not a read through it).
    const ctx = () => JSON.parse(localStorage.getItem('knomit.context') ?? 'null');
    expect(ctx()).toEqual({ kind: 'repo', repo: 'ops-kb' });

    // The repair: unmount the missing repo. The lens is readable again, so it
    // becomes the context, still on Settings, with its three tabs.
    const repaired = { ...haunted, reads: [{ uid: 'u-core', name: 'core-kb' }] };
    m.updateLens.mockResolvedValue(repaired);
    m.listLenses.mockResolvedValue([repaired, healthy]);
    m.getLens.mockImplementation(async (name: string) => (name === 'dev' ? healthy : repaired));
    fireEvent.click(screen.getByTestId('lens-edit'));
    fireEvent.click(await screen.findByTestId('lens-read-vendor-docs'));
    fireEvent.click(screen.getByTestId('lens-edit-save'));
    await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
    expect(m.updateLens).toHaveBeenCalledWith('haunted', { reads: [] });
    expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByTestId('target-label-chip')).toBeNull();
    expect(screen.queryByTestId('lens-broken-callout')).toBeNull();
    await waitFor(() => expect(ctx()).toEqual({ kind: 'lens', name: 'haunted' }));
  });

  it('a healthy lens is still picked as the context, with its three tabs (control)', async () => {
    const { m } = await primeApi();
    const healthy = { name: 'dev', uid: 'l-dev', write: { uid: 'u-ops', name: 'ops-kb' }, reads: [{ uid: 'u-ops', name: 'ops-kb' }], write_branch: STATUS };
    m.listLenses.mockResolvedValue([healthy]);
    m.getLens.mockResolvedValue(healthy);
    await mountApp();
    fireEvent.click(await screen.findByTestId('rail-lens-dev'));
    await waitFor(() => expect(m.getLens).toHaveBeenCalledWith('dev'));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('dev'));
    expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
    expect(screen.queryByTestId('target-label-chip')).toBeNull();
  });

  it('restoring an archived repo lands on it as the context', async () => {
    const { m } = await primeApi();
    m.listArchived.mockResolvedValue([Q3]);
    await mountApp();
    fireEvent.click(await screen.findByTestId('rail-archived'));
    fireEvent.click(screen.getByTestId('rail-archived-id-q3'));
    await screen.findByTestId('archived-settings');

    m.restoreRepo.mockResolvedValue({ name: 'q3-planning' });
    m.listArchived.mockResolvedValue([]);
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, { name: 'q3-planning', uid: 'u-q3' }]);
    fireEvent.click(screen.getByTestId('archived-restore-id-q3'));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('q3-planning'));
    expect(screen.queryByTestId('target-label-chip')).toBeNull();
    expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
    expect(screen.getByTestId('rail-repo-q3-planning')).toHaveAttribute('aria-current', 'true');
  });
});

describe('drafts', () => {
  beforeEach(() => { localStorage.setItem('knomit.rail', '1'); });

  const job = (over: Record<string, unknown> = {}) => ({
    create_id: 'c-new', name: 'ops-new', mode: 'initialize', state: 'running', step: 'index', phase: 'index',
    index_state: 'indexing', repo: { name: 'ops-new', uid: 'u-new' }, ...over,
  });

  it('E3: the wizard lands on the new repo\'s Settings in creating mode, and stays there when the job is done', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-new-repo'));
    expect(screen.getByTestId('target-label')).toHaveTextContent('New repository');
    expect(screen.queryAllByRole('tab')).toHaveLength(0);

    fireEvent.click(screen.getByTestId('choose-local'));
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'ops-new' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await screen.findByTestId('step-ontology');
    await waitFor(() => expect(screen.getByRole('button', { name: /^next$/i })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await screen.findByTestId('step-review');

    // The create reports the index phase with the repo registered, and runs on.
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, { name: 'ops-new', uid: 'u-new' }]);
    m.listRepoCreates.mockResolvedValue([job()]);
    m.createRepo.mockImplementation(async (_b: unknown, onStatus: (s: unknown) => void) => {
      onStatus(job());
      return new Promise(() => {});
    });
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toContain('ops-new'));
    await waitFor(() => expect(tabNames()).toEqual(['Settings']));
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^creating$/);
    expect(await screen.findByTestId('create-progress')).toBeInTheDocument();
    const ids = Array.from(document.querySelectorAll('section[id]')).map(s => s.id);
    expect(ids[0]).toBe('create');
    // The draft row gave way to the real row.
    expect(screen.queryByTestId('rail-draft-repo')).toBeNull();
    expect(screen.getByTestId('rail-repo-ops-new')).toHaveAttribute('aria-current', 'true');

    // The job is done: Browse and Activity appear, and the reader stays on Settings.
    m.listRepoCreates.mockResolvedValue([job({ state: 'done', step: 'done', index_state: 'ready' })]);
    await act(async () => { await refreshRepoCreates(); });
    await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
    expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
  });

  it('N2: creating a lens lands on its Browse, labelled in the lens accent', async () => {
    const { m } = await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-new-lens'));
    expect(screen.getByTestId('target-label')).toHaveTextContent('New lens');
    fireEvent.change(await screen.findByTestId('lens-name'), { target: { value: 'platform' } });

    const platform = { name: 'platform', uid: 'l-platform', write: { uid: 'u-core', name: 'core-kb' }, reads: [{ uid: 'u-core', name: 'core-kb' }], write_branch: STATUS };
    m.createLens.mockResolvedValue(platform);
    m.listLenses.mockResolvedValue([platform]);
    m.getLens.mockResolvedValue(platform);
    fireEvent.click(screen.getByTestId('lens-create'));

    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('platform'));
    expect(screen.getByTestId('target-label')).toHaveStyle({ color: 'rgb(185, 166, 240)' });
    await waitFor(() => expect(screen.getByTestId('facet-tab-browse')).toHaveAttribute('aria-selected', 'true'));
    expect(screen.queryByTestId('rail-draft-lens')).toBeNull();
    expect(screen.getByTestId('rail-lens-platform')).toHaveAttribute('aria-current', 'true');
  });

  // The wizard's create keeps polling after its draft is replaced (one slot),
  // so its registration can land while the LENS draft is on screen.
  it('a repo create that registers while the lens draft is shown leaves the lens draft alone', async () => {
    const { m } = await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-new-repo'));
    fireEvent.click(screen.getByTestId('choose-local'));
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'ops-new' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await screen.findByTestId('step-ontology');
    await waitFor(() => expect(screen.getByRole('button', { name: /^next$/i })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await screen.findByTestId('step-review');

    let report: ((s: unknown) => void) | null = null;
    m.createRepo.mockImplementation(async (_b: unknown, onStatus: (s: unknown) => void) => {
      report = onStatus;
      return new Promise(() => {});
    });
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));
    await waitFor(() => expect(report).not.toBeNull());

    // The reader moves on to a lens and starts filling it in.
    fireEvent.click(screen.getByTestId('rail-new-lens'));
    fireEvent.change(await screen.findByTestId('lens-name'), { target: { value: 'platform' } });

    // Now the repo registers.
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, { name: 'ops-new', uid: 'u-new' }]);
    m.listRepoCreates.mockResolvedValue([job()]);
    const reposCalls = m.repos.mock.calls.length;
    await act(async () => { report!(job()); });
    // The lists were refreshed, so the registration was acted on…
    await waitFor(() => expect(m.repos.mock.calls.length).toBeGreaterThan(reposCalls));
    await screen.findByTestId('rail-repo-ops-new');
    // …and the lens draft is still the one on screen, with what was typed.
    expect(screen.getByTestId('target-label')).toHaveTextContent('New lens');
    expect(screen.getByTestId('rail-draft-lens')).toHaveAttribute('aria-current', 'true');
    expect(screen.getByTestId('lens-name')).toHaveValue('platform');
    expect(screen.getByTestId('rail-repo-ops-new').getAttribute('aria-current')).toBeNull();
    expect(m.status).not.toHaveBeenCalledWith('ops-new', expect.anything());
  });

  it('survives a detour: what was typed is still there when the draft row is picked again', async () => {
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-new-repo'));
    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'my-repo' } });

    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('core-kb'));
    // Hidden, not unmounted, and its row kept.
    expect(screen.getByTestId('draft-repo')).not.toBeVisible();
    expect(screen.getByTestId('rail-draft-repo')).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('rail-draft-repo'));
    expect(screen.getByTestId('draft-repo')).toBeVisible();
    expect((screen.getByTestId('create-url') as HTMLInputElement).value).toBe('my-repo');
  });

  it('Cancel removes the draft and restores the target and facet before it', async () => {
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await screen.findByTestId('activity-toolbar');
    fireEvent.click(screen.getByTestId('rail-new-repo'));
    await screen.findByTestId('step-source');

    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(await screen.findByTestId('activity-toolbar')).toBeInTheDocument();
    expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
    expect(screen.queryByTestId('rail-draft-repo')).toBeNull();
    expect(screen.queryByTestId('draft-repo')).toBeNull();
  });
});

// The two flows where a context switch lands on a repo that refreshContextAfterChange
// has already made current (SET_REPO), so the SET_CONTEXT that follows keeps
// the repo. The branch must survive that (D's SET_CONTEXT rule), or Browse
// sits on "Opening…" for good.
describe('Browse opens after the repo it lands on was made current by the list refresh', () => {
  beforeEach(() => { localStorage.setItem('knomit.rail', '1'); });

  // listArchived answers AFTER the new repo's status: that is the order in
  // which the second SET_CONTEXT lands on a repo that already has its branch.
  const later = <T,>(v: T) => () => new Promise<T>(r => setTimeout(() => r(v), 60));

  const browseOpen = async () => {
    fireEvent.click(await screen.findByTestId('facet-tab-browse'));
    await waitFor(() => expect(screen.getByTestId('library-splitter')).toBeInTheDocument());
    expect(screen.queryByTestId('browse-opening')).toBeNull();
  };

  it('zero repos → create → done → Browse shows', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    localStorage.removeItem('knomit.context');
    m.repos.mockResolvedValue([]);
    await mountApp();
    expect(screen.getByTestId('target-label')).toHaveTextContent('New repository');

    fireEvent.click(screen.getByTestId('choose-local'));
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'first-kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await screen.findByTestId('step-ontology');
    await waitFor(() => expect(screen.getByRole('button', { name: /^next$/i })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));
    await screen.findByTestId('step-review');

    const job = (over: Record<string, unknown> = {}) => ({
      create_id: 'c-first', name: 'first-kb', mode: 'initialize', state: 'running', step: 'index', phase: 'index',
      index_state: 'indexing', repo: { name: 'first-kb', uid: 'u-first' }, ...over,
    });
    m.repos.mockResolvedValue([{ name: 'first-kb', uid: 'u-first' }]);
    m.listArchived.mockImplementation(later([]));
    m.listRepoCreates.mockResolvedValue([job()]);
    m.createRepo.mockImplementation(async (_b: unknown, onStatus: (s: unknown) => void) => {
      onStatus(job());
      return new Promise(() => {});
    });
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toContain('first-kb'));
    await waitFor(() => expect(tabNames()).toEqual(['Settings']));

    m.listRepoCreates.mockResolvedValue([job({ state: 'done', step: 'done', index_state: 'ready' })]);
    await act(async () => { await refreshRepoCreates(); });
    await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
    await browseOpen();
  });

  it('archive the last repo → restore it → Browse shows', async () => {
    const { m } = await primeApi();
    m.repos.mockResolvedValue([{ name: 'ops-kb', uid: 'u-ops' }]);
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    fireEvent.click(await screen.findByTestId('repo-archive'));

    const row = { id: 'u-ops', name: 'ops-kb', origin: '', archivedAt: '2026-10-10T00:00:00Z' };
    m.archiveRepo.mockResolvedValue(row);
    m.repos.mockResolvedValue([]);
    m.listArchived.mockResolvedValue([row]);
    fireEvent.click(screen.getByTestId('repo-archive-confirm'));
    // The last repo gone: the shell is the repository draft.
    await waitFor(() => expect(screen.getByTestId('target-label')).toHaveTextContent('New repository'));

    fireEvent.click(await screen.findByTestId('rail-archived'));
    fireEvent.click(screen.getByTestId('rail-archived-u-ops'));
    await screen.findByTestId('archived-settings');
    m.restoreRepo.mockResolvedValue({ name: 'ops-kb' });
    m.listArchived.mockImplementation(later([]));
    m.repos.mockResolvedValue([{ name: 'ops-kb', uid: 'u-ops' }]);
    fireEvent.click(screen.getByTestId('archived-restore-u-ops'));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
    await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
    await browseOpen();
  });
});

describe('the status footer\'s creates count', () => {
  it('opens the rail, where each create is a row with its chip', async () => {
    const { m } = await primeApi();
    m.listRepoCreates.mockResolvedValue([{ create_id: 'c9', name: 'b-kb', mode: 'subscribe', state: 'running' }]);
    await mountApp();
    expect(screen.queryByTestId('target-rail')).toBeNull();
    fireEvent.click(await screen.findByTestId('footer-creates'));
    expect(await screen.findByTestId('target-rail')).toBeInTheDocument();
    expect(await screen.findByTestId('pending-create-chip-rail-b-kb')).toHaveTextContent('creating');
  });
});
