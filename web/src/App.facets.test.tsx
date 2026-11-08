import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource, streamsMatching, latestStream, chatStream } from './testEventSource';
import { __resetRepoCreatesForTest } from './useRepoCreates';
import { SUBSCRIPTION_TITLE } from './state';
import { stubFleetFetch } from './fleetTestFetch';
import { resetFactCache } from './chatFactLinks';

// The facets of a target: Browse, Activity and Settings, as tabs in the top
// bar. facetsFor decides which exist; the facet is remembered under
// knomit.facet; Settings and Activity are the target's own pages;
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
      // The gear opens on Access, which reads the stored secrets.
      listSecrets: vi.fn().mockResolvedValue([]),
      getFleet: vi.fn().mockRejectedValue(new Error('not in a fleet')), listFleetMembers: vi.fn().mockResolvedValue([]),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(), motifCluster: vi.fn(),
      listRepoCreates: vi.fn(), listExperiments: vi.fn(), listPushedBranches: vi.fn(),
      // The drafts and the pinned targets.
      createRepo: vi.fn(), createLens: vi.fn(), updateLens: vi.fn(), renameLens: vi.fn(), renameRepo: vi.fn(), archiveRepo: vi.fn(), restoreRepo: vi.fn(), purgeRepo: vi.fn(),
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
  // A motif is a cluster of two carriers, so its menu offers the pivot.
  m.motifCluster.mockImplementation(async (_r: string, _b: string, key: string) => ({
    cluster_key: key, canonical: key, members: [key], df: 2, carrier_count: 2,
    carriers: [{ path: 'kb/decisions/one.md', title: 'One', type: 'observation' }, { path: 'kb/b.md', title: 'B', type: 'observation' }],
    aliases: [{ motif: key, method: 'canonical' }],
  }));
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
    // The body is the facet, and the Library is gone with Browse.
    expect(screen.queryByTestId('server-pages')).toBeNull();
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
    expect(screen.getByTestId('server-toggle').querySelector('span')).toBeNull();
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

  // Activity is the only sessions reader left (the old Sessions tab and its
  // badge went with the global view), so opening the server pages over it
  // closes the one stream and opens none.
  it('closes its sessions stream when the server pages open over it, and nothing else opens one', async () => {
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await waitFor(() => expect(openStreams('/sessions/events')).toBe(1));
    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    await screen.findByTestId('server-pages');
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
    expect(screen.getByTestId('server-toggle')).toBeEnabled();

    await act(async () => { fireEvent.click(await screen.findByTestId('wizard-connect')); });
    await waitFor(() => expect((screen.getByTestId('wizard-crumb-back') as HTMLButtonElement).disabled).toBe(true));

    expect(screen.getByTestId('rail-repo-core-kb')).toBeDisabled();
    expect(screen.getByTestId('facet-tab-browse')).toBeDisabled();
    expect(screen.getByTestId('facet-tab-activity')).toBeDisabled();
    expect(screen.getByTestId('server-toggle')).toBeDisabled();

    m.status.mockClear();
    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    fireEvent.click(screen.getByTestId('facet-tab-browse'));
    fireEvent.click(screen.getByTestId('server-toggle'));
    await act(async () => { await Promise.resolve(); });
    expect(m.status).not.toHaveBeenCalledWith('core-kb', expect.anything());
    expect(screen.getByTestId('remote-connect-wizard')).toBeInTheDocument();
    expect(screen.queryByTestId('server-pages')).toBeNull();
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

  // #476: refreshLists swallows a failed listArchived, and archivedLoaded is
  // already true from boot, so the archived list it left behind does not hold
  // the new row. The pin must still have a row behind it, or the auto-unpin
  // effect drops it the moment it is set.
  it('archiving a repo with no store keeps its archived row pinned when the archived re-read fails', async () => {
    const { m } = await primeApi();
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, VENDOR]);
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-repo-vendor-docs'));
    await screen.findByTestId('repo-unavailable-archive');

    const row = { id: 'u-v', name: 'vendor-docs', origin: '', archivedAt: '2026-10-10T00:00:00Z' };
    m.archiveRepo.mockResolvedValue(row);
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }]);
    m.listArchived.mockRejectedValue(new Error('archived list unavailable'));
    fireEvent.click(screen.getByTestId('repo-unavailable-archive'));
    fireEvent.click(screen.getByTestId('repo-unavailable-archive-confirm'));

    expect(await screen.findByTestId('archived-settings')).toBeInTheDocument();
    // Still there once everything has settled: not pinned and then dropped.
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByTestId('archived-settings')).toBeInTheDocument();
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^archived$/);
    expect(screen.getByTestId('rail-archived-u-v')).toHaveAttribute('aria-current', 'true');
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

  // #476: a rename re-reads the lens list, which no longer holds the old
  // name. The pin follows the lens to its new name in the same render, so the
  // auto-unpin effect never sees it gone; the reader stays on its Settings.
  it('renaming a pinned broken lens keeps it pinned, on its Settings, under the new name', async () => {
    const { m } = await primeApi();
    m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, VENDOR]);
    const haunted = {
      name: 'haunted', uid: 'l-h', write: { uid: 'u-core', name: 'core-kb' },
      reads: [{ uid: 'u-core', name: 'core-kb' }, { uid: 'u-v', name: 'vendor-docs' }], write_branch: STATUS,
    };
    m.listLenses.mockResolvedValue([haunted]);
    m.getLens.mockResolvedValue(haunted);
    await mountApp();
    fireEvent.click(await screen.findByTestId('rail-lens-haunted'));
    await screen.findByTestId('lens-broken-callout');

    const spooky = { ...haunted, name: 'spooky' };
    m.renameLens.mockResolvedValue(spooky);
    m.listLenses.mockResolvedValue([spooky]);
    m.getLens.mockResolvedValue(spooky);
    fireEvent.change(screen.getByTestId('lens-rename-input'), { target: { value: 'spooky' } });
    fireEvent.change(screen.getByTestId('lens-rename-confirm'), { target: { value: 'haunted' } });
    fireEvent.click(screen.getByTestId('lens-rename-submit'));

    await waitFor(() => expect(m.renameLens).toHaveBeenCalledWith('haunted', 'spooky'));
    await waitFor(() => expect(screen.getByTestId('target-label')).toHaveTextContent('spooky'));
    await act(async () => { await new Promise(r => setTimeout(r, 50)); });
    expect(screen.getByTestId('target-label')).toHaveTextContent('spooky');
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^mount missing$/);
    expect(tabNames()).toEqual(['Settings']);
    expect(screen.getByTestId('lens-broken-callout')).toHaveTextContent('vendor-docs');
    expect(screen.getByTestId('rail-lens-spooky')).toHaveAttribute('aria-current', 'true');
    // Never entered: the context is where it was.
    expect(JSON.parse(localStorage.getItem('knomit.context') ?? 'null')).toEqual({ kind: 'repo', repo: 'ops-kb' });
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

// #472: a cancelled create leaves the creates list (the server omits it), so
// the pinned target must carry the name the top bar shows.
describe('a pinned pending create that is cancelled', () => {
  it('keeps its name in the top bar after the job leaves the list', async () => {
    const { m } = await primeApi();
    m.listRepoCreates.mockResolvedValue([{ create_id: 'c9', name: 'b-kb', mode: 'subscribe', state: 'running' }]);
    await mountApp();
    fireEvent.click(await screen.findByTestId('footer-creates'));
    fireEvent.click(await screen.findByTestId('pending-create-rail-b-kb'));
    await waitFor(() => expect(screen.getByTestId('target-label')).toHaveTextContent('b-kb'));
    // The cancel lands: the server now omits the job from the list and serves it by id.
    m.listRepoCreates.mockResolvedValue([]);
    m.getRepoCreate.mockResolvedValue({ create_id: 'c9', name: 'b-kb', mode: 'subscribe', state: 'cancelled' });
    m.cancelRepoCreate.mockResolvedValue(undefined);
    fireEvent.click(await screen.findByTestId('create-cancel-button'));
    await screen.findByTestId('create-cancelled');
    expect(screen.getByTestId('target-label')).toHaveTextContent('b-kb');
  });
});

// #469: a create started by ANOTHER client is seen by the creates poll, but
// nothing in this tab re-read the repo list for it, so it stayed a "created"
// pending row until clicked and was missing from the lens draft's mounts.
describe('a create that finished elsewhere', () => {
  beforeEach(() => { localStorage.setItem('knomit.rail', '1'); });

  const job = (over: Record<string, unknown> = {}) => ({
    create_id: 'c-far', name: 'far-kb', mode: 'initialize', state: 'running', step: 'ingest', phase: 'ingest',
    ...over,
  });
  const BASE = [{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }];
  const WITH_FAR = [...BASE, { name: 'far-kb', uid: 'u-far' }];

  it('becomes a real repo row with no click, and a mount the lens draft offers', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    m.listRepoCreates.mockResolvedValue([job()]);
    await mountApp();
    expect(await screen.findByTestId('pending-create-rail-far-kb')).toBeInTheDocument();

    // The other client's create finishes; the server lists its repo.
    m.repos.mockResolvedValue(WITH_FAR);
    m.listRepoCreates.mockResolvedValue([job({ state: 'done', step: 'done', phase: 'done', index_state: 'ready', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });

    expect(await screen.findByTestId('rail-repo-far-kb')).toBeInTheDocument();
    expect(screen.queryByTestId('pending-create-rail-far-kb')).toBeNull();
    fireEvent.click(screen.getByTestId('rail-new-lens'));
    expect(await screen.findByTestId('lens-read-far-kb')).toBeInTheDocument();
  });

  it('is listed, flagged creating, as soon as it registers — before the job is done', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    m.listRepoCreates.mockResolvedValue([job()]);
    await mountApp();
    await screen.findByTestId('pending-create-rail-far-kb');

    m.repos.mockResolvedValue(WITH_FAR);
    m.listRepoCreates.mockResolvedValue([job({ step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });

    await screen.findByTestId('rail-repo-far-kb');
    expect(screen.getByTestId('rail-create-chip-far-kb')).toHaveTextContent(/^creating$/);
    expect(screen.queryByTestId('pending-create-rail-far-kb')).toBeNull();
  });

  // NON-REGRESSION, not coverage of the fix: this passes without the
  // re-read too, because PendingCreateSettings asks /repos itself and hands
  // over on its own. It pins that the re-read does not break that path: the
  // pending row goes, the auto-unpin effect has no `pending` case so the pin
  // stays, and the pinned page still hands over to the repository.
  it('a pinned pending create that registers elsewhere still hands over to its repository', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    m.listRepoCreates.mockResolvedValue([job()]);
    await mountApp();
    fireEvent.click(await screen.findByTestId('pending-create-rail-far-kb'));
    await screen.findByTestId('pending-create-settings');

    m.repos.mockResolvedValue(WITH_FAR);
    m.listRepoCreates.mockResolvedValue([job({ step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });

    await waitFor(() => expect(screen.getByTestId('rail-repo-far-kb')).toHaveAttribute('aria-current', 'true'));
    expect(screen.queryByTestId('pending-create-settings')).toBeNull();
    expect(tabNames()).toEqual(['Settings']);
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent(/^creating$/);
  });

  // The cap: "registered and not listed" can hold on every poll (the list
  // lags the job, or an older server still lists a done job whose repo is
  // gone), and each poll must not become a /repos read.
  it('a done job whose repo stays unlisted costs one re-read, not one per poll', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    await mountApp();
    await waitFor(() => expect(m.repos).toHaveBeenCalled());
    // Let boot's reads settle before counting.
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    const before = m.repos.mock.calls.length;

    // Every answer is a NEW array, as a real poll's is: a mock that returned
    // one instance would publish nothing new, and no effect would re-run.
    m.repos.mockImplementation(async () => [...BASE]);
    m.listRepoCreates.mockImplementation(async () => [job({ state: 'done', step: 'done', phase: 'done', index_state: 'ready', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });
    await waitFor(() => expect(m.repos.mock.calls.length).toBe(before + 1));
    for (let i = 0; i < 3; i++) {
      await act(async () => { await refreshRepoCreates(); });
    }
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    expect(m.repos.mock.calls.length).toBe(before + 1);
  });

  // The done point is the retry: a registered-point read that failed must not
  // leave the repo out of the list for good.
  it('a failed read at registration is retried when the job is done', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    m.listRepoCreates.mockResolvedValue([job()]);
    await mountApp();
    await screen.findByTestId('pending-create-rail-far-kb');
    const before = m.repos.mock.calls.length;

    m.repos.mockRejectedValueOnce(new Error('blip'));
    m.repos.mockResolvedValue(WITH_FAR);
    m.listRepoCreates.mockImplementation(async () => [job({ step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });
    await waitFor(() => expect(m.repos.mock.calls.length).toBe(before + 1));
    // Still indexing: the registered point is spent, so no second read.
    await act(async () => { await refreshRepoCreates(); });
    expect(m.repos.mock.calls.length).toBe(before + 1);
    expect(screen.queryByTestId('rail-repo-far-kb')).toBeNull();

    m.listRepoCreates.mockImplementation(async () => [job({ state: 'done', step: 'done', phase: 'done', index_state: 'ready', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });
    expect(await screen.findByTestId('rail-repo-far-kb')).toBeInTheDocument();
    expect(m.repos.mock.calls.length).toBe(before + 2);
  });

  it('a cancelled or failed job never triggers a re-read', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    await mountApp();
    await waitFor(() => expect(m.repos).toHaveBeenCalled());
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    const before = m.repos.mock.calls.length;

    // Both look registered (index_state, a repo uid) and neither is listed.
    m.listRepoCreates.mockImplementation(async () => [
      job({ create_id: 'c-x', name: 'x-kb', state: 'cancelled', step: 'index', index_state: 'indexing', repo: { name: 'x-kb', uid: 'u-x' } }),
      job({ create_id: 'c-y', name: 'y-kb', state: 'failed', step: 'index', index_state: 'indexing', repo: { name: 'y-kb', uid: 'u-y' } }),
    ]);
    for (let i = 0; i < 3; i++) {
      await act(async () => { await refreshRepoCreates(); });
    }
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    expect(m.repos.mock.calls.length).toBe(before);
  });

  // Review of #755, reproduced: the effect can fire while boot is still
  // awaiting the remembered repo's details, after reposLoaded but before boot's
  // SET_CONTEXT. A re-read that re-synced the context (refreshLists) then
  // dispatched SET_REPO for the first readable repo, which landed after boot's
  // SET_CONTEXT and replaced the remembered context, in localStorage too.
  it('a registered job seen during boot does not move the remembered context', async () => {
    const { m } = await primeApi();
    const later = <T,>(v: T, ms: number) => new Promise<T>(r => setTimeout(() => r(v), ms));
    m.getRepo.mockImplementation((name: string) => later({ name, description: DESCRIPTIONS[name] ?? '' }, 40));
    m.repos.mockResolvedValueOnce(BASE);
    m.repos.mockImplementation(() => later(WITH_FAR, 120));
    m.listRepoCreates.mockResolvedValue([job({ step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await mountApp();

    await screen.findByTestId('rail-repo-far-kb', {}, { timeout: 2000 });
    await act(async () => { await new Promise(r => setTimeout(r, 200)); });
    expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
    expect(JSON.parse(localStorage.getItem('knomit.context') ?? 'null')).toEqual({ kind: 'repo', repo: 'ops-kb' });
  });

  // An event that lands while the re-read is in flight is newer than its
  // snapshot: the answer may add the new repo, never undo the event.
  it('an event during the re-read survives its answer, and the new repo is still added', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    await mountApp();
    await waitFor(() => expect(latestStream('/api/v1/repo-events')).toBeDefined());
    let answer: (v: unknown) => void = () => {};
    m.repos.mockImplementation(() => new Promise(r => { answer = r; }));
    const before = m.repos.mock.calls.length;
    m.listRepoCreates.mockResolvedValue([job({ step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });
    await waitFor(() => expect(m.repos.mock.calls.length).toBe(before + 1));

    act(() => { latestStream('/api/v1/repo-events')!.emit('remote', { repo: 'core-kb', sync: 'ok', push: 'error' }); });
    expect(await screen.findByTestId('rail-health-core-kb')).toBeInTheDocument();
    // The snapshot was taken before the event: core-kb has no failing remote in it.
    await act(async () => { answer(WITH_FAR); });

    expect(await screen.findByTestId('rail-repo-far-kb')).toBeInTheDocument();
    expect(screen.getByTestId('rail-health-core-kb')).toBeInTheDocument();
  });

  // Re-review of #755, reproduced: a held answer still carries a repo that a
  // NEWER whole list (here the stream's reconnect read) has since dropped. It
  // must not bring that repo back, not even as "a row the list lacks".
  it('a held answer cannot bring back a repo a newer whole list removed', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    const GONE = [...BASE, { name: 'gone-kb', uid: 'u-gone' }];
    m.repos.mockResolvedValue(GONE);
    await mountApp();
    await screen.findByTestId('rail-repo-gone-kb');
    await waitFor(() => expect(latestStream('/api/v1/repo-events')).toBeDefined());

    let answer: (v: unknown) => void = () => {};
    m.repos.mockImplementationOnce(() => new Promise(r => { answer = r; }));
    const before = m.repos.mock.calls.length;
    m.listRepoCreates.mockResolvedValue([job({ step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });
    await waitFor(() => expect(m.repos.mock.calls.length).toBe(before + 1));

    // The stream reconnects and re-reads: gone-kb was deleted meanwhile.
    m.repos.mockResolvedValue(WITH_FAR);
    act(() => { latestStream('/api/v1/repo-events')!.emit('ready', {}); });
    await waitFor(() => expect(screen.queryByTestId('rail-repo-gone-kb')).toBeNull());
    expect(screen.getByTestId('rail-repo-far-kb')).toBeInTheDocument();

    // The held answer lands, still listing gone-kb.
    await act(async () => { answer([...GONE, { name: 'far-kb', uid: 'u-far' }]); });
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    expect(screen.queryByTestId('rail-repo-gone-kb')).toBeNull();
    expect(screen.getByTestId('rail-repo-far-kb')).toBeInTheDocument();
  });

  // The same hole through this tab's own whole-list write: a rename's
  // refreshLists lands while the read is held, and the held answer still has
  // the old name. It must neither revert the rename nor add the old name as a
  // second row for the same repository.
  it('a rename landing during a held read is neither reverted nor doubled', async () => {
    const { m } = await primeApi();
    const { refreshRepoCreates } = await import('./useRepoCreates');
    await mountApp();
    await waitFor(() => expect(m.repos).toHaveBeenCalled());

    let answer: (v: unknown) => void = () => {};
    m.repos.mockImplementationOnce(() => new Promise(r => { answer = r; }));
    const before = m.repos.mock.calls.length;
    m.listRepoCreates.mockResolvedValue([job({ step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'far-kb', uid: 'u-far' } })]);
    await act(async () => { await refreshRepoCreates(); });
    await waitFor(() => expect(m.repos.mock.calls.length).toBe(before + 1));

    // ops-kb is renamed to ops-new from its Settings.
    const RENAMED = [{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-new', uid: 'u-ops' }, { name: 'far-kb', uid: 'u-far' }];
    m.repos.mockResolvedValue(RENAMED);
    m.renameRepo.mockResolvedValue({ name: 'ops-new' });
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    fireEvent.change(await screen.findByTestId('repo-rename-input'), { target: { value: 'ops-new' } });
    fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'ops-kb' } });
    fireEvent.click(screen.getByTestId('repo-rename-submit'));
    await screen.findByTestId('rail-repo-ops-new');
    await waitFor(() => expect(screen.queryByTestId('rail-repo-ops-kb')).toBeNull());

    // The held answer lands with the name from before the rename.
    await act(async () => { answer([...WITH_FAR]); });
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    expect(screen.queryByTestId('rail-repo-ops-kb')).toBeNull();
    expect(screen.getByTestId('rail-repo-ops-new')).toBeInTheDocument();
    expect(screen.getByTestId('rail-repo-far-kb')).toBeInTheDocument();
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

// #324: the instance actions (+ repo, + lens and the Logs tab) were once gated
// on the BROWSED repo's read-only state. A fresh origin has no remembered
// context, so pickRepo lands on the first sorted repo — here a subscription —
// and the instance looked read-only although the server accepts every write.
describe('browsing a subscription on a writable instance', () => {
  it('leaves the rail\'s + enabled and the Logs tab present, while fact authoring stays blocked', async () => {
    localStorage.removeItem('knomit.context');
    localStorage.setItem('knomit.rail', '1');
    const { m } = await primeApi();
    m.getRepo.mockImplementation(async (name: string) => ({ name, description: '', mode: 'subscribe', read_branch: 'main' }));
    m.browse.mockResolvedValue({ path: 'kb', children: [{ name: 'foo.md', is_dir: false, title: 'Foo', type: 'observation' }] });
    m.fact.mockResolvedValue({
      path: 'kb/foo.md', title: 'Foo', body: 'body', type: 'observation', confidence: 0.9, sources: 1,
      domain: [], entities: [], refs: [], commit_hash: 'aaa1111', commit_date: '2026-05-01T00:00:00Z',
    });
    m.factCommits.mockResolvedValue({ entries: [] });
    m.explain.mockResolvedValue({ incoming: [], outgoing: [] });
    m.commitDetail.mockResolvedValue(null);

    await mountApp();
    // SET_REPO_READONLY lands asynchronously, after the boot details. Wait until
    // the subscription is OBSERVABLE — the fact pane's retract names it — so the
    // instance assertions below cannot pass merely by running before it arrives.
    fireEvent.click(await screen.findByTestId('dir-entry'));
    await waitFor(() => {
      const retract = screen.getByTestId('retract-btn');
      expect(retract).toBeDisabled();
      expect(retract).toHaveAttribute('title', SUBSCRIPTION_TITLE);
    });

    expect(screen.getByTestId('rail-new-repo')).toBeEnabled();
    expect(screen.getByTestId('rail-new-lens')).toBeEnabled();
    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    expect(await screen.findByTestId('server-tab-logs')).toBeInTheDocument();
  });
});

// A chat is a pinned target whose backing list is GET /chats (useChats). The
// pin-backing-list invariant: it is in that list no later than the render that
// pins it, or the auto-unpin effect drops it at once.
describe('a chat target', () => {
  beforeEach(() => { localStorage.setItem('knomit.rail', '1'); });
  afterEach(() => { vi.unstubAllGlobals(); });

  const PROVIDERS = { _embedded: { providers: [{ name: 'anthropic', kind: 'anthropic', models: [{ id: 'a1', available: true, tools: true }] }] } };
  const CHAT_A = { id: 'chat-a', provider: 'anthropic', model: 'a1' };

  function chatRoutes(initial: unknown[] = []) {
    const server = { chats: initial as unknown[] };
    const fx = stubFleetFetch({
      'GET /api/v1/chat-providers': { body: PROVIDERS },
      'GET /api/v1/chats': () => ({ body: { _embedded: { chats: server.chats } } }),
      'POST /api/v1/chats': () => { server.chats = [CHAT_A]; return { status: 201, body: CHAT_A }; },
      'GET /api/v1/chats/chat-a': { body: { ...CHAT_A, running: false, items: [], pending_approvals: [], grants: [] } },
    });
    return { fx, server };
  }
  const createChatThroughTheForm = async () => {
    fireEvent.click(await screen.findByTestId('rail-new-chat'));
    expect(screen.getByTestId('target-label')).toHaveTextContent('New chat');
    fireEvent.click(await screen.findByTestId('chat-model-anthropic-a1'));
    fireEvent.change(screen.getByTestId('chat-start-input'), { target: { value: 'first words' } });
    fireEvent.keyDown(screen.getByTestId('chat-start-input'), { key: 'Enter' });
    await waitFor(() => expect(screen.getByTestId('rail-chat-chat-a')).toHaveAttribute('aria-current', 'true'));
  };

  // SABOTAGE: ChatStartForm.start calls createChat without the message -> the POST
  // body has no message and the transcript stays empty.
  it('Enter on the start screen creates the chat with its first message, which shows as the first turn', async () => {
    await primeApi();
    const server = { chats: [] as unknown[], created: false };
    const fx = stubFleetFetch({
      'GET /api/v1/chat-providers': { body: PROVIDERS },
      'GET /api/v1/chats': () => ({ body: { _embedded: { chats: server.chats } } }),
      'POST /api/v1/chats': () => { server.chats = [CHAT_A]; server.created = true; return { status: 201, body: { ...CHAT_A, turn: 0 } }; },
      'GET /api/v1/chats/chat-a': () => ({ body: {
        ...CHAT_A, running: true, pending_approvals: [], grants: [],
        items: server.created ? [{ kind: 'user', text: 'first words' }] : [],
      } }),
    });
    await mountApp();
    fireEvent.click(await screen.findByTestId('rail-new-chat'));
    fireEvent.click(await screen.findByTestId('chat-model-anthropic-a1'));
    expect(fx.calls.filter(c => c.route === 'POST /api/v1/chats')).toHaveLength(0);
    fireEvent.change(screen.getByTestId('chat-start-input'), { target: { value: 'first words' } });
    fireEvent.keyDown(screen.getByTestId('chat-start-input'), { key: 'Enter' });
    const turn = await screen.findByTestId('chat-turn-0');
    expect(within(turn).getByTestId('chat-user')).toHaveTextContent('first words');
    const post = fx.calls.filter(c => c.route === 'POST /api/v1/chats');
    expect(post).toHaveLength(1);
    expect(post[0].body).toEqual({ provider: 'anthropic', model: 'a1', message: 'first words' });
  });

  it('a chat pinned in the same render it is added is not auto-unpinned', async () => {
    await primeApi();
    const { fx } = chatRoutes();
    await mountApp();
    await createChatThroughTheForm();
    expect(tabNames()).toEqual(['Chat', 'Settings']);
    expect(screen.getByTestId('target-label')).toHaveTextContent('a1');
    // Let every effect run: a pin without its row would be dropped by now.
    await act(async () => { await new Promise(r => setTimeout(r, 30)); });
    expect(screen.getByTestId('rail-chat-chat-a')).toHaveAttribute('aria-current', 'true');
    expect(tabNames()).toEqual(['Chat', 'Settings']);
    // The row came from add(), not from a re-read of the list.
    expect(fx.calls.filter(c => c.route === 'POST /api/v1/chats')).toHaveLength(1);
    expect(fx.calls.filter(c => c.route === 'GET /api/v1/chats')).toHaveLength(1);
  });

  it('a pinned chat that leaves the list is unpinned: the view returns to the browse context', async () => {
    await primeApi();
    const { server } = chatRoutes();
    await mountApp();
    await createChatThroughTheForm();

    server.chats = [];
    act(() => { window.dispatchEvent(new Event('focus')); });
    await waitFor(() => expect(screen.queryByTestId('rail-chat-chat-a')).toBeNull());
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
    expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
  });

  it('picking a chat from the rail pins it with Chat and Settings; a repo row leaves it, on Browse', async () => {
    await primeApi();
    chatRoutes([CHAT_A]);
    await mountApp();
    fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
    await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
    expect(screen.getByTestId('facet-tab-chat')).toHaveAttribute('aria-selected', 'true');

    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
    expect(screen.getByTestId('facet-tab-browse')).toHaveAttribute('aria-selected', 'true');
    // The chat is still in the rail: leaving it does not end it.
    expect(screen.getByTestId('rail-chat-chat-a')).toBeInTheDocument();
  });

  // The whole wiring from a real chip in a real App. SABOTAGE: ChatFacet's
  // onCommit left out -> the chip does nothing and no panel opens.
  it('a commit chip opens the commit in the chat\'s side panel and the reader stays in the chat', async () => {
    window.history.replaceState(null, '', '/');
    resetFactCache();
    const { api } = await import('./api');
    const m = api as unknown as Record<string, ReturnType<typeof vi.fn>>;
    await primeApi();
    const FULL = 'abc1234' + 'f'.repeat(33);
    const PATH = 'kb/decisions/one.md';
    const { fx } = chatRoutes([CHAT_A]);
    fx.set('GET /api/v1/chats/chat-a', { body: { ...CHAT_A, running: false, pending_approvals: [], grants: [], items: [
      { kind: 'user', text: 'q' },
      { kind: 'assistant', text: 'Fixed in `abc1234`.' },
    ] } });
    fx.set('POST /api/v1/facts/resolve', { body: [{ handle: 'abc1234', id: '', title: '', type: '', repo: 'core-kb', branch: 'read/branch', path: '', excerpt: '', commit: FULL }] });
    m.commitDetail.mockResolvedValue({ commit: FULL, date: '2026-11-01T10:00:00Z', message: 'learn: one', author: { name: 'Ada', email: 'a@example.com' }, files: [{ path: PATH, action: 'modified', title: 'One' }] });
    m.fact.mockResolvedValue({ path: PATH, title: 'One', type: 'observation', body: 'THE OLD TEXT', domain: [], confidence: 0.9, sources: 1, entities: [], refs: [] });
    m.explain.mockResolvedValue({ incoming: [], outgoing: [] });
    m.factCommits.mockResolvedValue({ entries: [] });
    await mountApp();
    fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
    fireEvent.click(await screen.findByTestId('commit-link'));

    const commit = await screen.findByTestId('chat-commit');
    expect(m.commitDetail).toHaveBeenCalledWith('core-kb', 'read/branch', FULL);
    expect(commit).toHaveTextContent('learn: one');
    expect(screen.getByTestId('rail-chat-chat-a')).toHaveAttribute('aria-current', 'true');
    expect(tabNames()).toEqual(['Chat', 'Settings']);

    m.fact.mockClear();
    fireEvent.click(within(commit).getByTestId('timeline-file-row'));
    expect(await screen.findByTestId('fact-view')).toHaveTextContent('THE OLD TEXT');
    expect(m.fact).toHaveBeenCalledWith('core-kb', 'read/branch', PATH, FULL, { fallback: 'before' });
    expect(tabNames()).toEqual(['Chat', 'Settings']);
  });

  // Every way from a chat's fact to Browse, with the real Library and only the api
  // mocked. Recent and search select row 0 when the open fact is not on page 1 and
  // keep the anchor, so landing in either would silently show ANOTHER fact at that
  // commit, under a band that looks right. The list here never holds the fact, and
  // answers late or in the same microtask (the case that needs the landing to be a
  // layout effect: an effect's update would render after it).
  // SABOTAGE: reveal without sort/freeText; the landing in a passive effect.
  describe('from the chat\'s band to Browse', () => {
    const FULL = 'abc1234' + 'f'.repeat(33);
    const PATH = 'kb/decisions/one.md';
    const ROW0 = 'kb/other/zero.md';
    const later = <T,>(v: T, ms: number) => (ms ? new Promise<T>(r => setTimeout(() => r(v), ms)) : Promise.resolve(v));
    type Control = 'version' | 'in' | 'out';
    const TEST_ID: Record<Control, string> = { version: 'version-walker', in: 'connections-in', out: 'connections-out' };
    const statusOf = (b: string) => ({ head: 'abc1234', branch: b, embeddings_enabled: false, ontology_root: 'kb', index_state: 'ready', index_done: 0, index_total: 0, index_percent: 100 });

    async function toTheChatsFact({ start, mode, ms, action = 'modified', branch = 'machine/test', motifs }: {
      start: string; mode: 'recent' | 'search' | 'path'; ms: number; action?: string; branch?: string; motifs?: string[];
    }) {
      window.history.replaceState(null, '', '/'); // the URL holds the last test's list and context, and beats localStorage
      resetFactCache(); // the resolver's answers are kept per handle for the page's life
      localStorage.setItem('knomit.context', JSON.stringify({ kind: 'repo', repo: start }));
      const { api } = await import('./api');
      const m = api as unknown as Record<string, ReturnType<typeof vi.fn>>;
      await primeApi();
      m.recent.mockImplementation(() => later({ facts: [{ path: ROW0, title: 'Row zero' }], total: 1 }, ms));
      m.search.mockImplementation(() => later({ results: [{ path: ROW0, title: 'Row zero', score: 1 }], total: 1 }, ms));
      m.explain.mockResolvedValue({ incoming: [{ path: 'kb/in.md', title: 'In', versions: [{ commit: 'c1' }] }], outgoing: [{ path: 'kb/out.md', title: 'Out', versions: [{ commit: 'c2' }] }] });
      m.factCommits.mockResolvedValue({ entries: [{ commit: 'newest1', date: '', message: 'newest' }, { commit: FULL, date: '', message: 'at it' }] });
      m.commitDetail.mockResolvedValue({ commit: FULL, date: '2026-11-01T10:00:00Z', message: 'learn: one', author: { name: 'Ada', email: 'a@example.com' }, files: [{ path: PATH, action, title: 'One' }] });
      m.fact.mockImplementation(async (_r: string, _b: string, path: string, c?: string) => ({ path, title: path, type: 'observation', body: 'BODY OF ' + path, domain: [], confidence: 0.9, sources: 1, entities: [], refs: [], commit_hash: c ?? 'newest1', ...(path === PATH && motifs ? { motifs } : {}) }));
      const { fx } = chatRoutes([CHAT_A]);
      fx.set('GET /api/v1/chats/chat-a', { body: { ...CHAT_A, running: false, pending_approvals: [], grants: [], items: [
        { kind: 'user', text: 'q' },
        { kind: 'assistant', text: 'Fixed in `abc1234`.' },
      ] } });
      fx.set('POST /api/v1/facts/resolve', { body: [{ handle: 'abc1234', id: '', title: '', type: '', repo: 'core-kb', branch, path: '', excerpt: '', commit: FULL }] });
      await mountApp();
      if (mode === 'recent') {
        fireEvent.mouseEnter(screen.getByTestId('mode-flyout'));
        const seg = await screen.findByTestId('sort-recent');
        fireEvent.pointerDown(seg, { pointerType: 'mouse' });
        fireEvent.click(seg, { detail: 1 });
      } else if (mode === 'search') {
        const input = within(screen.getByTestId('browse-search')).getByRole('textbox');
        fireEvent.change(input, { target: { value: 'something' } });
        fireEvent.keyDown(input, { key: 'Enter' });
      }
      await act(async () => { await new Promise(r => setTimeout(r, 60)); });
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      fireEvent.click(await screen.findByTestId('commit-link'));
      const commit = await screen.findByTestId('chat-commit');
      fireEvent.click(within(commit).getByTestId('timeline-file-row'));
      await screen.findByTestId('fact-view');
      await screen.findByTestId('version-walker');
      await waitFor(() => expect(screen.getByTestId('connections-in')).toHaveTextContent('1'));
      return m;
    }
    // The version lands at once. Cited by and cites open their menu IN PLACE in the
    // chat's panel (chat review, 2026-11-08): the app stays on the chat until the
    // reader picks from it, and the pick is what lands in Browse.
    const PICK: Record<'in' | 'out', string> = { in: 'In', out: 'Out' };
    const click = async (m: Record<string, ReturnType<typeof vi.fn>>, control: Control, ms: number) => {
      m.fact.mockClear();
      fireEvent.click(screen.getByTestId(TEST_ID[control]));
      if (control !== 'version') {
        const chatView = within(screen.getByTestId('fact-view'));
        expect(chatView.getByTestId(TEST_ID[control]).getAttribute('aria-expanded')).toBe('true');
        await act(async () => { await new Promise(r => setTimeout(r, 30)); });
        expect(tabNames()).toEqual(['Chat', 'Settings']);
        fireEvent.click(within(chatView.getByTestId('connections-panel')).getByText(PICK[control]));
      }
      await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
      await act(async () => { await new Promise(r => setTimeout(r, ms + 150)); });
      return within(screen.getByTestId('browse-facet'));
    };
    // Everything Browse read for a fact, and what is on screen.
    const reads = (m: Record<string, ReturnType<typeof vi.fn>>) => m.fact.mock.calls.map(c => `${c[2]}@${String(c[3]).slice(0, 7)}`);
    // The version lands on this fact at the commit; a pick from cited by or cites
    // lands on the fact picked, at the commit its edge pins.
    const LANDS: Record<Control, string> = { version: `${PATH}@abc1234`, in: 'kb/in.md@c1', out: 'kb/out.md@c2' };
    const expectLandedOnTheFact = (m: Record<string, ReturnType<typeof vi.fn>>, control: Control, browse: ReturnType<typeof within>) => {
      expect(reads(m).at(-1)).toBe(LANDS[control]);
      // Row 0 at the commit is the trap. Row 0 live is only the back-crumb's title, when it was the fact open before.
      expect(reads(m)).not.toContain(`${ROW0}@abc1234`);
      const body = screen.getByTestId('browse-facet').textContent;
      expect(body).toContain('BODY OF ' + LANDS[control].split('@')[0]);
      expect(body).not.toContain('BODY OF ' + ROW0);
      expect(browse.getByTestId('timeline-nav')).toBeInTheDocument();
      // No menu is handed over: Browse lands with its cells closed.
      for (const dir of ['in', 'out'] as const) {
        expect(browse.getByTestId(TEST_ID[dir]).getAttribute('aria-expanded')).toBe('false');
      }
    };

    // Back returns to the chat, whichever way the reader asks.
    const expectBackInTheChat = async () => {
      await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
      expect(screen.getByTestId('rail-chat-chat-a')).toHaveAttribute('aria-current', 'true');
      // As it was: the same chat, its panel still on the fact.
      expect(screen.getByTestId('fact-view')).toBeVisible();
      expect(screen.getByTestId('fact-view')).toHaveTextContent('BODY OF ' + PATH);
    };

    for (const control of ['version', 'in', 'out'] as const) {
      for (const start of ['core-kb', 'ops-kb']) for (const mode of ['recent', 'search'] as const) for (const ms of [30, 0]) {
        it(`${control}: from ${start === 'core-kb' ? 'the same repo' : 'another repo'}, Browse in ${mode}, the list answering ${ms ? 'late' : 'in the same microtask'}`, async () => {
          const m = await toTheChatsFact({ start, mode, ms });
          const browse = await click(m, control, ms);
          expectLandedOnTheFact(m, control, browse);
        });
      }
      for (const start of ['core-kb', 'ops-kb']) {
        it(`${control}: a fact retracted at HEAD (on no list at all), from ${start}, in Recent`, async () => {
          const m = await toTheChatsFact({ start, mode: 'recent', ms: 30, action: 'deleted' });
          const browse = await click(m, control, 30);
          expectLandedOnTheFact(m, control, browse);
        });
      }
    }

    for (const start of ['core-kb', 'ops-kb']) for (const mode of ['recent', 'search'] as const) {
      it(`a motif opens its menu in the chat, and its pivot lands on that fact, at that commit, with the motif as the query: from ${start}, in ${mode}`, async () => {
        const m = await toTheChatsFact({ start, mode, ms: 30, motifs: ['a-shape'] });
        m.fact.mockClear();
        // The pivot's list holds the fact (it carries the motif), as Browse's own would.
        m.recent.mockImplementation(() => Promise.resolve({ facts: [{ path: PATH, title: 'One' }], total: 1 }));
        m.search.mockImplementation(() => Promise.resolve({ results: [{ path: PATH, title: 'One', score: 1 }], total: 1 }));
        const chatView = within(screen.getByTestId('fact-view'));
        fireEvent.click(await chatView.findByTestId('motif-cell'));
        const pivot = await within(chatView.getByTestId('motif-panel')).findByTestId('motif-pivot');
        expect(tabNames()).toEqual(['Chat', 'Settings']);
        fireEvent.click(pivot);
        await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
        await act(async () => { await new Promise(r => setTimeout(r, 180)); });
        expect(reads(m).at(-1)).toBe(`${PATH}@abc1234`);
        // The list Browse reads is the motif's (the chip row is not drawn while anchored,
        // so the query is checked where it goes): the motif, and no path scope.
        const lastList = JSON.stringify(m.recent.mock.calls.at(-1));
        expect(lastList).toContain('a-shape');
        expect(lastList).not.toContain('kb/decisions');
      });
    }

    // Browse on another branch of the same repo: the commit was read from the
    // resolver's branch, and that is the history the timeline has to show.
    it('Browse on another branch of the repo moves to the branch the fact was read from', async () => {
      const m = await toTheChatsFact({ start: 'core-kb', mode: 'recent', ms: 30, branch: 'read/branch' });
      m.status.mockImplementation(async (_r: string, b?: string) => statusOf(b || 'machine/test'));
      const browse = await click(m, 'version', 30);
      expect(m.status).toHaveBeenCalledWith('core-kb', 'read/branch');
      expectLandedOnTheFact(m, 'version', browse);
      const ours = m.fact.mock.calls.filter(c => c[2] === PATH);
      expect(ours.length).toBeGreaterThan(0);
      expect(ours.every(c => c[1] === 'read/branch')).toBe(true);
    });

    // The branch switch is a second wait, with the list read of the first still in flight.
    for (const ms of [30, 0]) it(`from another repo in Recent, where the fact was read from another branch, the list answering ${ms ? 'late' : 'in the same microtask'}`, async () => {
      const m = await toTheChatsFact({ start: 'ops-kb', mode: 'recent', ms, branch: 'read/branch' });
      m.status.mockImplementation(async (_r: string, b?: string) => statusOf(b || 'machine/test'));
      const browse = await click(m, 'version', ms);
      expectLandedOnTheFact(m, 'version', browse);
      expect(m.fact.mock.calls.filter(c => c[2] === PATH).every(c => c[1] === 'read/branch')).toBe(true);
    });

    it('a branch that cannot be read still lands on the fact, on the branch on screen', async () => {
      const m = await toTheChatsFact({ start: 'core-kb', mode: 'recent', ms: 30, branch: 'read/branch' });
      m.status.mockImplementation(async (_r: string, b?: string) => {
        if (b === 'read/branch') throw new Error('no such branch');
        return statusOf(b || 'machine/test');
      });
      expectLandedOnTheFact(m, 'version', await click(m, 'version', 30));
    });

    it('the timeline\'s back returns to the chat, with the side panel as it was', async () => {
      const m = await toTheChatsFact({ start: 'core-kb', mode: 'path', ms: 0 });
      const browse = await click(m, 'version', 0);
      fireEvent.click(browse.getByTestId('timeline-back'));
      await expectBackInTheChat();
    });
    it('⌘[ returns to the chat', async () => {
      const m = await toTheChatsFact({ start: 'ops-kb', mode: 'path', ms: 0 });
      await click(m, 'version', 0);
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await expectBackInTheChat();
    });
    it('back from a hop inside Browse stays in Browse; the next one returns to the chat', async () => {
      const m = await toTheChatsFact({ start: 'core-kb', mode: 'path', ms: 0 });
      const browse = await click(m, 'version', 0);
      fireEvent.click(browse.getByTestId('connections-in'));
      fireEvent.click(within(browse.getByTestId('connections-panel')).getByText('In'));
      await waitFor(() => expect(reads(m).at(-1)).toBe('kb/in.md@c1'));
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(reads(m).at(-1)).toBe(`${PATH}@abc1234`));
      expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await expectBackInTheChat();
    });
    it('what Browse held before the chat is not on the trail, because back from here is the chat', async () => {
      const m = await toTheChatsFact({ start: 'core-kb', mode: 'recent', ms: 0 }); // row 0 is the open fact, live
      const browse = await click(m, 'version', 0);
      await waitFor(() => expect(document.body.textContent).toContain('BODY OF ' + PATH));
      expect(browse.queryByText('Row zero')).toBeNull();
      expect(browse.queryByText(ROW0)).toBeNull();
    });
    it('a chat that has since ended: back stays in Browse, on what it restored, and does not bring the chat back', async () => {
      const m = await toTheChatsFact({ start: 'core-kb', mode: 'path', ms: 0 });
      await click(m, 'version', 0);
      const es = await waitFor(() => { const e = chatStream('chat-a'); expect(e).toBeTruthy(); return e!; });
      act(() => { es.emit('chat_end', { reason: 'deleted' }); });
      await waitFor(() => expect(screen.queryByTestId('rail-chat-chat-a')).toBeNull());
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(screen.queryByTestId('timeline-nav')).toBeNull());
      expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
      expect(screen.queryByTestId('chat-body-chat-a')).toBeNull();
    });
    // A fact chip, not a commit: the fact live, in the one fact view, on the chip's branch.
    async function toTheChipsFact({ start, mode, ms, branch = 'machine/test', status }: { start: string; mode: 'recent' | 'path'; ms: number; branch?: string; status?: (r: string, b?: string) => unknown }) {
      window.history.replaceState(null, '', '/');
      resetFactCache();
      localStorage.setItem('knomit.context', JSON.stringify({ kind: 'repo', repo: start }));
      const { api } = await import('./api');
      const m = api as unknown as Record<string, ReturnType<typeof vi.fn>>;
      await primeApi();
      if (status) m.status.mockImplementation(async (r: string, b?: string) => status(r, b));
      m.recent.mockImplementation(() => later({ facts: [{ path: ROW0, title: 'Row zero' }], total: 1 }, ms));
      m.explain.mockResolvedValue({ incoming: [{ path: 'kb/in.md', title: 'In', versions: [{ commit: 'c1' }] }], outgoing: [{ path: 'kb/out.md', title: 'Out', versions: [{ commit: 'c2' }] }] });
      m.factCommits.mockResolvedValue({ entries: [{ commit: 'newest1', date: '', message: 'newest' }, { commit: FULL, date: '', message: 'at it' }] });
      m.fact.mockImplementation(async (_r: string, _b: string, path: string, c?: string) => ({ path, title: path, type: 'observation', body: 'BODY OF ' + path, domain: ['web'], confidence: 0.9, sources: 1, entities: [], refs: [{ raw: 'kb/ref.md', path: 'kb/ref.md', kind: 'fact' }], commit_hash: c ?? 'newest1' }));
      const { fx } = chatRoutes([CHAT_A]);
      fx.set('GET /api/v1/chats/chat-a', { body: { ...CHAT_A, running: false, pending_approvals: [], grants: [], items: [
        { kind: 'user', text: 'q' },
        { kind: 'assistant', text: `See \`${PATH}\`.` },
      ] } });
      fx.set('POST /api/v1/facts/resolve', { body: [{ handle: PATH, id: 'x', title: 'One', type: 'observation', repo: 'core-kb', branch, path: PATH, excerpt: '' }] });
      await mountApp();
      if (mode === 'recent') {
        fireEvent.mouseEnter(screen.getByTestId('mode-flyout'));
        const seg = await screen.findByTestId('sort-recent');
        fireEvent.pointerDown(seg, { pointerType: 'mouse' });
        fireEvent.click(seg, { detail: 1 });
      }
      await act(async () => { await new Promise(r => setTimeout(r, 60)); });
      return m;
    }
    const openChip = async () => {
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      const chip = await screen.findByTestId('fact-link');
      fireEvent.click(chip);
      const view = await screen.findByTestId('fact-view');
      await within(view).findByTestId('version-walker');
      await waitFor(() => expect(within(view).getByTestId('connections-in')).toHaveTextContent('1'));
      return within(view);
    };
    const landed = async (ms: number) => {
      await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
      await act(async () => { await new Promise(r => setTimeout(r, ms + 150)); });
      return within(screen.getByTestId('browse-facet'));
    };
    const backToTheChat = async () => {
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
      expect(screen.getByTestId('fact-view')).toBeVisible();
      expect(screen.getByTestId('fact-view')).toHaveTextContent('BODY OF ' + PATH);
    };

    for (const start of ['core-kb', 'ops-kb']) for (const mode of ['recent', 'path'] as const) for (const control of ['version', 'in', 'out'] as const) {
      it(`fact chip, ${control} in its band, Back: from ${start}, Browse in ${mode}`, async () => {
        const m = await toTheChipsFact({ start, mode, ms: 30 });
        const view = await openChip();
        m.fact.mockClear();
        fireEvent.click(view.getByTestId(TEST_ID[control]));
        // Cited by and cites open their menu in the chat; the pick is what lands.
        const to = control === 'version' ? `${PATH}@newest1` : control === 'in' ? 'kb/in.md@c1' : 'kb/out.md@c2';
        if (control !== 'version') {
          expect(view.getByTestId(TEST_ID[control]).getAttribute('aria-expanded')).toBe('true');
          expect(tabNames()).toEqual(['Chat', 'Settings']);
          fireEvent.click(within(view.getByTestId('connections-panel')).getByText(control === 'in' ? 'In' : 'Out'));
        }
        const browse = await landed(30);
        expect(reads(m).at(-1)).toBe(to);
        expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + to.split('@')[0]);
        expect(reads(m)).not.toContain(`${ROW0}@newest1`);
        expect(browse.getByTestId('connections-in').getAttribute('aria-expanded')).toBe('false');
        await backToTheChat();
      });
    }

    it('a tag on the fact lands in Browse with that filter on, the fact open, and one Back returns to the chat', async () => {
      const m = await toTheChipsFact({ start: 'core-kb', mode: 'path', ms: 0 });
      const view = await openChip();
      m.fact.mockClear();
      m.recent.mockImplementation(() => Promise.resolve({ facts: [{ path: PATH, title: 'One' }], total: 1 }));
      fireEvent.click(view.getByText('web'));
      await landed(0);
      expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + PATH);
      expect(screen.getAllByTestId('filter-chip').map(c => c.textContent).join(' ')).toContain('web');
      await backToTheChat();
    });

    it('a reference in the body lands in Browse on that fact, at the commit the edge pins, and Back returns', async () => {
      const m = await toTheChipsFact({ start: 'core-kb', mode: 'path', ms: 0 });
      m.explain.mockResolvedValue({ incoming: [{ path: 'kb/in.md', title: 'In', versions: [{ commit: 'c1' }] }], outgoing: [{ path: 'kb/ref.md', title: 'Ref', versions: [{ commit: 'pin1234' }] }] });
      const view = await openChip();
      m.fact.mockClear();
      fireEvent.click(view.getByText(/kb\/ref\.md/));
      await landed(0);
      expect(reads(m).at(-1)).toBe('kb/ref.md@pin1234');
      await backToTheChat();
    });

    it('Browse\'s fact has its retract button and the chat\'s does not', async () => {
      await toTheChipsFact({ start: 'core-kb', mode: 'path', ms: 0 });
      const view = await openChip();
      expect(view.queryByTestId('retract-btn')).toBeNull();
      fireEvent.click(view.getByTestId('version-walker'));
      const browse = await landed(0);
      expect(browse.getByTestId('retract-btn')).toBeInTheDocument();
    });

    // Cmd-click on a chip. A reveal asks for the tree; since-fork on would demote
    // it to Recent, whose row 0 would then replace the fact.
    const EXP_STATUS = { ...statusOf('exp/try'), writable: true, experiment: { name: 'try', parent: 'machine/test', fork_commit: 'f0rk' } };
    for (const ms of [30, 0]) it(`cmd-click in an experiment with since-fork on lands on the fact, not row 0, the list answering ${ms ? 'late' : 'in the same microtask'}`, async () => {
      const m = await toTheChipsFact({ start: 'core-kb', mode: 'path', ms, branch: 'exp/try', status: () => EXP_STATUS });
      fireEvent.click(await screen.findByTestId('since-fork-toggle'));
      await waitFor(() => expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'true'));
      await act(async () => { await new Promise(r => setTimeout(r, ms + 60)); });
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      m.fact.mockClear();
      fireEvent.click(await screen.findByTestId('fact-link'), { metaKey: true });
      await landed(ms);
      expect(reads(m).at(-1)).toBe(`${PATH}@undefin`);
      expect(m.fact.mock.calls.at(-1)?.[1]).toBe('exp/try');
      expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + PATH);
      expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'false');
    });
    it('cmd-click takes the chip\'s branch, not the one Browse is on', async () => {
      const m = await toTheChipsFact({ start: 'core-kb', mode: 'path', ms: 0, branch: 'read/branch', status: (_r, b) => statusOf(b || 'machine/test') });
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      m.fact.mockClear();
      fireEvent.click(await screen.findByTestId('fact-link'), { metaKey: true });
      await landed(0);
      expect(m.status).toHaveBeenCalledWith('core-kb', 'read/branch');
      expect(m.fact.mock.calls.filter(c => c[2] === PATH).every(c => c[1] === 'read/branch')).toBe(true);
    });

    // A landing that moves the branch: its entry holds what the reader left, and Back
    // restores all of it. The old fact does not exist on the branch landed on.
    // SABOTAGE: status before the landing; NAV_BACK without the branch; no sinceFork in NavEntry.
    const onlyOnExp = (m: Record<string, ReturnType<typeof vi.fn>>) => {
      const answer = m.fact.getMockImplementation() as unknown as (...a: unknown[]) => unknown;
      m.fact.mockImplementation(async (r: string, b: string, path: string, c?: string) => {
        if (path === ROW0 && b !== 'exp/try') throw new Error(`no fact at path ${path} on branch ${b}`);
        return answer(r, b, path, c);
      });
    };
    const inExperimentOnAFact = async (chip: string, mode: 'recent' | 'path' = 'recent') => {
      const m = await toTheChipsFact({ start: 'core-kb', mode, ms: 0, branch: chip, status: (_r, b) => (b === 'read/branch' ? statusOf('read/branch') : EXP_STATUS) });
      onlyOnExp(m);
      fireEvent.click(await screen.findByTestId('since-fork-toggle'));
      await waitFor(() => expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + ROW0));
      expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'true');
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      m.fact.mockClear();
      fireEvent.click(await screen.findByTestId('fact-link'), { metaKey: true });
      await landed(0);
      return m;
    };
    const backThenBrowse = async () => {
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
      fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
      await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
      await act(async () => { await new Promise(r => setTimeout(r, 60)); });
    };
    it('leaving an experiment by a landing: Back restores the branch, the fact and since-fork, and Browse is not a 404', async () => {
      const m = await inExperimentOnAFact('read/branch');
      expect(m.fact.mock.calls.at(-1)?.[1]).toBe('read/branch');
      expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + PATH);
      expect(screen.queryByTestId('since-fork-toggle')).toBeNull(); // not in the experiment now
      await backThenBrowse();
      const body = screen.getByTestId('browse-facet').textContent;
      expect(body).toContain('BODY OF ' + ROW0);
      expect(body).not.toContain('no fact at path');
      expect(m.fact.mock.calls.at(-1)?.[1]).toBe('exp/try');
      expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'true');
    });
    // The experiment may have ended while the reader was away, an agent's doing or not.
    // SABOTAGE: no re-read; any failure treated as gone; a gone branch left in place.
    const gone = Object.assign(new Error('no such branch'), { status: 404 });
    const afterAway = async (restored: (b?: string) => unknown, mode: 'recent' | 'path' = 'recent') => {
      const m = await inExperimentOnAFact('read/branch', mode);
      m.status.mockClear();
      m.status.mockImplementation(async (_r: string, b?: string) => (b === 'exp/try' ? restored(b) : statusOf(b || 'machine/test')));
      // What each branch lists: the experiment's fact is only on the experiment.
      m.recent.mockImplementation(async (_r: string, b: string) => ({ facts: [b === 'exp/try' ? { path: ROW0, title: 'Row zero' } : { path: PATH, title: 'One' }], total: 1 }));
      await backThenBrowse();
      return m;
    };
    it('the restored branch is gone: Browse\'s normal view on the agent branch, not a 404, and no experiment', async () => {
      const m = await afterAway(() => { throw gone; });
      expect(m.status).toHaveBeenCalledWith('core-kb', 'exp/try');
      await waitFor(() => expect(m.status).toHaveBeenCalledWith('core-kb', 'machine/test'));
      await waitFor(() => expect(screen.queryByTestId('since-fork-toggle')).toBeNull());
      // The reader's sort is kept, and Recent selects its first row there, as it does.
      await waitFor(() => expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + PATH));
      const body = screen.getByTestId('browse-facet').textContent;
      expect(body).not.toContain('no fact at path');
      expect(body).not.toContain('BODY OF ' + ROW0);
      expect(screen.queryByTestId('filter-chip')).toBeNull();
      expect(window.location.search).not.toContain('exp');
    });
    // In the tree nothing selects a row by itself, so only closing the fact stops a 404.
    it('the restored branch is gone, in the tree: the fact is closed and Browse shows its summary', async () => {
      await afterAway(() => { throw gone; }, 'path');
      await waitFor(() => expect(screen.queryByTestId('since-fork-toggle')).toBeNull());
      expect(await screen.findByTestId('stats-view')).toBeInTheDocument();
      const body = screen.getByTestId('browse-facet').textContent;
      expect(body).not.toContain('no fact at path');
      expect(body).not.toContain('BODY OF ' + ROW0);
    });
    it('a failure that does not say the branch is gone leaves the reader on it', async () => {
      const m = await afterAway(() => { throw new Error('network down'); });
      expect(m.status).toHaveBeenCalledWith('core-kb', 'exp/try');
      expect(m.status).not.toHaveBeenCalledWith('core-kb', 'machine/test');
      expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + ROW0);
      expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'true');
    });
    // An answer for the restored branch that lands after the reader moved on to another
    // repo is for a context that is gone, and must change nothing.
    // SABOTAGE: drop either still() in the re-read in App.tsx (the catch's, or the one after
    // the agent branch's status).
    const lateAnswer = async () => {
      const m = await inExperimentOnAFact('read/branch');
      let settle!: (v: { ok: boolean; status?: unknown }) => void;
      const late = new Promise<unknown>((res, rej) => { settle = v => (v.ok ? res(v.status) : rej(gone)); });
      m.status.mockClear();
      m.getAgentBranch.mockClear();
      m.status.mockImplementation((r: string, b?: string) => (r === 'core-kb' && b === 'exp/try' ? late : Promise.resolve(statusOf(b || 'machine/test'))));
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
      await waitFor(() => expect(m.status).toHaveBeenCalledWith('core-kb', 'exp/try'));
      // The context moves on while the answer is in flight.
      fireEvent.click(screen.getByTestId('rail-repo-ops-kb'));
      await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
      await act(async () => { await new Promise(r => setTimeout(r, 60)); });
      return { m, settle };
    };
    const STALE = { ...EXP_STATUS, head: 'stale99', branch: 'exp/try' };
    it('a late 404 for the restored branch, after the context moved on, does not go looking for its agent branch', async () => {
      const { m, settle } = await lateAnswer();
      await act(async () => { settle({ ok: false }); await new Promise(r => setTimeout(r, 60)); });
      expect(m.getAgentBranch).not.toHaveBeenCalledWith('core-kb');
      expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
    });
    it('a late success for the restored branch, after the context moved on, is not applied', async () => {
      const { settle } = await lateAnswer();
      await act(async () => { settle({ ok: true, status: STALE }); await new Promise(r => setTimeout(r, 60)); });
      expect(screen.queryByTestId('since-fork-toggle')).toBeNull();
      expect(screen.queryByText(/stale99/)).toBeNull();
    });
    it('a 404 whose agent branch answers after the context moved on is not applied either', async () => {
      const m = await inExperimentOnAFact('read/branch');
      let release!: () => void;
      const agentAnswer = new Promise<void>(res => { release = res; });
      m.status.mockClear();
      m.status.mockImplementation(async (r: string, b?: string) => {
        if (r === 'core-kb' && b === 'exp/try') throw gone;
        if (r === 'core-kb' && b === 'machine/test') { await agentAnswer; return { ...STALE, branch: 'machine/test' }; }
        return statusOf(b || 'machine/test');
      });
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
      await waitFor(() => expect(m.status).toHaveBeenCalledWith('core-kb', 'machine/test'));
      fireEvent.click(screen.getByTestId('rail-repo-ops-kb'));
      await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
      await act(async () => { await new Promise(r => setTimeout(r, 60)); });
      await act(async () => { release(); await new Promise(r => setTimeout(r, 60)); });
      expect(screen.queryByTestId('since-fork-toggle')).toBeNull();
      expect(screen.queryByText(/stale99/)).toBeNull();
      expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
    });
    it('the re-read refreshes what the entry held: a write gate that changed while away', async () => {
      const m = await afterAway(() => ({ ...EXP_STATUS, head: 'newhead', writable: false }));
      expect(m.status).toHaveBeenCalledWith('core-kb', 'exp/try');
      await waitFor(() => expect(screen.getByTestId('retract-btn')).toBeDisabled());
      expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'true');
    });

    it('a landing on the same experiment branch: since-fork is off there, and Back turns it on again', async () => {
      await inExperimentOnAFact('exp/try');
      expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'false');
      await backThenBrowse();
      expect(screen.getByTestId('since-fork-toggle')).toHaveAttribute('aria-checked', 'true');
      expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + ROW0);
    });
    it('a landing that moves between ordinary branches restores the branch too', async () => {
      const m = await toTheChipsFact({ start: 'core-kb', mode: 'recent', ms: 0, branch: 'read/branch', status: (_r, b) => statusOf(b || 'machine/test') });
      const answer = m.fact.getMockImplementation() as unknown as (...a: unknown[]) => unknown;
      m.fact.mockImplementation(async (r: string, b: string, path: string, c?: string) => {
        if (path === ROW0 && b === 'read/branch') throw new Error('no fact at path');
        return answer(r, b, path, c);
      });
      await waitFor(() => expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + ROW0));
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      fireEvent.click(await screen.findByTestId('fact-link'), { metaKey: true });
      await landed(0);
      await backThenBrowse();
      expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + ROW0);
      expect(m.fact.mock.calls.at(-1)?.[1]).toBe('machine/test');
    });

    // The chat stays mounted behind Browse, and its Focus must not reach it.
    // SABOTAGE: useMatFocus ignores `hidden`; App does not pass onScreen.
    const matFocus = () => document.documentElement.dataset.matFocus;
    const inPanelFocus = async () => {
      await toTheChipsFact({ start: 'core-kb', mode: 'path', ms: 0 });
      const view = await openChip();
      fireEvent.click(view.getByTestId('materialize-btn'));
      await screen.findByTestId('mat-actions');
      expect(matFocus()).toBe('1');
    };
    for (const [way, go] of [
      ['cmd-click on a chip', () => fireEvent.click(screen.getByTestId('fact-link'), { metaKey: true })],
      ['the rail', () => fireEvent.click(screen.getByTestId('rail-repo-core-kb'))],
    ] as const) it(`the panel's Focus does not follow the reader to Browse by ${way}, and Esc there is Browse's`, async () => {
      await inPanelFocus();
      go();
      await landed(0);
      expect(matFocus()).toBeUndefined();
      if (way === 'cmd-click on a chip') expect(screen.getAllByTestId('filter-chip').length).toBeGreaterThan(0);
      fireEvent.keyDown(window, { key: 'Escape' }); // Browse's: CLEAR_FILTERS
      await waitFor(() => expect(screen.queryAllByTestId('filter-chip')).toHaveLength(0));
      fireEvent.click(screen.getByTestId('rail-chat-chat-a'));
      await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
      expect(matFocus()).toBe('1'); // it was not spent, and the chat is as it was
    });
    it('the chat\'s Settings tab hides the panel, and its Focus with it', async () => {
      await inPanelFocus();
      fireEvent.click(screen.getByRole('tab', { name: 'Settings' }));
      await waitFor(() => expect(matFocus()).toBeUndefined());
      fireEvent.click(screen.getByRole('tab', { name: 'Chat' }));
      await waitFor(() => expect(matFocus()).toBe('1'));
    });

    // S4: a branch that cannot be read lands in the same task as the rejection, or the
    // list read in flight answers first and selects row 0.
    it('a branch that cannot be read lands on the fact even with the list answering in the same microtask', async () => {
      const m = await toTheChatsFact({ start: 'ops-kb', mode: 'recent', ms: 0, branch: 'read/branch' });
      m.status.mockImplementation(async (_r: string, b?: string) => {
        if (b === 'read/branch') throw new Error('no such branch');
        return statusOf(b || 'machine/test');
      });
      expectLandedOnTheFact(m, 'version', await click(m, 'version', 0));
    });

    // S5: the reader left for another repo while the branch was being read.
    it('a branch read that answers after the reader moved to another repo lands nothing there', async () => {
      const m = await toTheChatsFact({ start: 'core-kb', mode: 'path', ms: 0, branch: 'read/branch' });
      let release: (v: unknown) => void = () => {};
      m.status.mockImplementation((_r: string, b?: string) => b === 'read/branch' ? new Promise(r => { release = r; }) : Promise.resolve(statusOf(b || 'machine/test')));
      m.fact.mockClear();
      fireEvent.click(screen.getByTestId(TEST_ID.version));
      await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
      fireEvent.click(screen.getByTestId('rail-repo-ops-kb'));
      await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
      await act(async () => { release(statusOf('read/branch')); await new Promise(r => setTimeout(r, 40)); });
      expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
      expect(reads(m)).not.toContain(`${PATH}@abc1234`);
      expect(screen.getByTestId('browse-facet').textContent).not.toContain('BODY OF ' + PATH);
    });

    it('back within Browse with no chat involved is untouched: the chat is not shown', async () => {
      await primeApi();
      chatRoutes([CHAT_A]);
      window.history.replaceState(null, '', '/');
      await mountApp();
      const input = within(screen.getByTestId('browse-search')).getByRole('textbox');
      fireEvent.change(input, { target: { value: 'path:kb/x' } });
      fireEvent.keyDown(input, { key: 'Enter' });
      await act(async () => { await new Promise(r => setTimeout(r, 30)); });
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await act(async () => { await new Promise(r => setTimeout(r, 30)); });
      expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
    });

    // Back across a repo switch returns to the repo left, on its branch, with its
    // fact: never the old fact resolved in the new repo (a 404 in the fact pane).
    // SABOTAGE: SET_CONTEXT without push; NAV_BACK applying the entry in the current
    // context; no branch re-read in the navReturn landing; no existence check.
    describe('Back across a repo switch', () => {
      const ONE = 'kb/ops/one.md';
      const TWO = 'kb/ops/two.md';
      const THREE = 'kb/ops/three.md';
      const CORE = 'kb/core/c.md';
      const notHere = (path: string, r: string) => Object.assign(new Error(`no fact at path ${path} in ${r}`), { status: 404 });
      // `start: 'core-kb'` begins on core-kb's fact and picks ops-kb from there, so
      // the stack holds another context's entry below ops-kb's.
      async function inOpsOnTwo(status?: (r: string, b?: string) => unknown, start: 'ops-kb' | 'core-kb' = 'ops-kb') {
        window.history.replaceState(null, '', '/');
        resetFactCache();
        localStorage.setItem('knomit.context', JSON.stringify({ kind: 'repo', repo: start }));
        const { m } = await primeApi();
        chatRoutes([CHAT_A]);
        if (status) m.status.mockImplementation(async (r: string, b?: string) => status(r, b));
        // Everything the fact view reads, set here: no test in this group may lean on
        // what an earlier test left mocked (each must pass alone, with -t).
        m.explain.mockResolvedValue({ incoming: [], outgoing: [] });
        m.factCommits.mockResolvedValue({ entries: [] });
        m.listExperiments.mockResolvedValue({ experiments: [{ name: 'other', branch: 'exp/other', parent: 'machine/test', fork_commit: 'f1', writable: true }] });
        m.recent.mockImplementation(async (r: string) => (r === 'ops-kb'
          ? { facts: [{ path: ONE, title: 'One' }, { path: TWO, title: 'Two' }, { path: THREE, title: 'Three' }], total: 3 }
          : { facts: [{ path: CORE, title: 'Core' }], total: 1 }));
        m.fact.mockImplementation(async (r: string, _b: string, path: string, c?: string) => {
          if (path.startsWith('kb/ops/') !== (r === 'ops-kb')) throw notHere(path, r);
          return { path, title: path, type: 'observation', body: 'BODY OF ' + path, domain: [], confidence: 0.9, sources: 1, entities: [], refs: [], commit_hash: c ?? 'newest1' };
        });
        await mountApp();
        fireEvent.mouseEnter(screen.getByTestId('mode-flyout'));
        const seg = await screen.findByTestId('sort-recent');
        fireEvent.pointerDown(seg, { pointerType: 'mouse' });
        fireEvent.click(seg, { detail: 1 });
        if (start === 'core-kb') {
          await waitFor(() => expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + CORE));
          fireEvent.click(screen.getByTestId('rail-repo-ops-kb'));
          await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
        }
        await waitFor(() => expect(screen.getAllByTestId('chrono-item')).toHaveLength(3));
        fireEvent.click(screen.getAllByTestId('chrono-item')[0]);
        await waitFor(() => expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + ONE));
        fireEvent.click(screen.getAllByTestId('chrono-item')[1]);
        await waitFor(() => expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + TWO));
        return m;
      }
      const toCore = async () => {
        fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
        await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('core-kb'));
        await waitFor(() => expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + CORE));
      };
      const back = async () => {
        fireEvent.keyDown(window, { key: '[', metaKey: true });
        await act(async () => { await new Promise(r => setTimeout(r, 80)); });
      };
      const body = () => screen.getByTestId('browse-facet').textContent ?? '';

      it('Back returns to the repo left, on the fact that was open, and never reads it in the other repo', async () => {
        const m = await inOpsOnTwo();
        await toCore();
        expect(screen.getByRole('button', { name: 'Back' })).toBeEnabled();
        m.fact.mockClear();
        await back();
        await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
        await waitFor(() => expect(body()).toContain('BODY OF ' + TWO));
        expect(body()).not.toContain('no fact at path');
        expect(m.fact.mock.calls.filter(c => c[0] === 'core-kb' && String(c[2]).startsWith('kb/ops/'))).toEqual([]);
        expect(screen.getByTestId('rail-repo-ops-kb')).toHaveAttribute('aria-current', 'true');
        expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
        // And Back inside the repo it returned to is Browse's own, as before.
        await back();
        await waitFor(() => expect(body()).toContain('BODY OF ' + ONE));
        expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
      });

      const EXP = { ...statusOf('exp/try'), writable: true, experiment: { name: 'try', parent: 'machine/test', fork_commit: 'f0rk' } };
      it('the branch left is read again and restored, with the experiment', async () => {
        let onExp = true;
        const m = await inOpsOnTwo((r, b) => (r === 'ops-kb' && (b === 'exp/try' || (!b && onExp)) ? EXP : statusOf('machine/test')));
        expect(screen.getByTestId('since-fork-toggle')).toBeInTheDocument();
        await toCore();
        onExp = false; // the repo's own branch is no longer the experiment
        m.status.mockClear();
        m.fact.mockClear();
        await back();
        await waitFor(() => expect(m.status).toHaveBeenCalledWith('ops-kb', 'exp/try'));
        await waitFor(() => expect(body()).toContain('BODY OF ' + TWO));
        expect(m.fact.mock.calls.filter(c => c[2] === TWO).at(-1)?.[1]).toBe('exp/try');
        expect(screen.getByTestId('since-fork-toggle')).toBeInTheDocument();
      });

      it('the branch left is gone: Browse\'s normal view on the repo\'s branch, not a 404', async () => {
        let onExp = true;
        const gone = Object.assign(new Error('no such branch'), { status: 404 });
        const m = await inOpsOnTwo((r, b) => {
          if (r === 'ops-kb' && b === 'exp/try' && !onExp) throw gone;
          return r === 'ops-kb' && (b === 'exp/try' || (!b && onExp)) ? EXP : statusOf('machine/test');
        });
        await toCore();
        onExp = false;
        m.fact.mockClear();
        await back();
        await waitFor(() => expect(m.status).toHaveBeenCalledWith('ops-kb', 'exp/try'));
        await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
        expect(screen.queryByTestId('since-fork-toggle')).toBeNull();
        expect(body()).not.toContain('no fact at path');
        expect(m.fact.mock.calls.every(c => c[1] !== 'exp/try')).toBe(true);
        expect(screen.getByRole('button', { name: 'Back' })).toBeDisabled();
      });

      it('the repo left is gone: the reader stays where they are, with no error, and Back has nothing left there', async () => {
        const m = await inOpsOnTwo();
        await toCore();
        m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }]);
        act(() => { latestStream('/api/v1/repo-events')!.emit('ready', {}); });
        await waitFor(() => expect(screen.queryByTestId('rail-repo-ops-kb')).toBeNull());
        m.getRepo.mockClear();
        await back();
        expect(screen.getByTestId('target-label').textContent).toBe('core-kb');
        expect(body()).toContain('BODY OF ' + CORE);
        expect(body()).not.toContain('no fact at path');
        expect(m.getRepo).not.toHaveBeenCalledWith('ops-kb');
        expect(screen.queryByTestId('boot-phase')).toBeNull();
        expect(screen.getByRole('button', { name: 'Back' })).toBeDisabled();
      });

      // The repo's own branch is the experiment until the reader leaves; after that the
      // entry's branch (exp/try) answers through `away`, which each test controls.
      const gone404 = () => Object.assign(new Error('no such branch'), { status: 404 });
      async function awayFromExp(away: () => unknown) {
        let onExp = true;
        const m = await inOpsOnTwo((r, b) => {
          if (r === 'ops-kb' && b === 'exp/try' && !onExp) return away();
          if (r === 'ops-kb' && (b === 'exp/try' || (!b && onExp))) return EXP;
          return statusOf(b || 'machine/test');
        });
        await waitFor(() => expect(screen.getByTestId('retract-btn')).toBeEnabled()); // the entry's gate: open
        await toCore();
        onExp = false;
        m.status.mockClear();
        m.fact.mockClear();
        return m;
      }
      const hold = () => {
        let settle!: (ok: boolean) => void;
        const p = new Promise((res, rej) => { settle = ok => (ok ? res(EXP) : rej(gone404())); });
        return { p, settle };
      };
      const askedForTheBranchLeft = async (m: Record<string, ReturnType<typeof vi.fn>>) => {
        await back();
        await waitFor(() => expect(m.status).toHaveBeenCalledWith('ops-kb', 'exp/try'));
      };
      const settleNow = async (h: ReturnType<typeof hold>, ok: boolean) => {
        await act(async () => { h.settle(ok); await new Promise(r => setTimeout(r, 60)); });
      };

      // The return's 404 drops only the entries left on the dead branch of that repo:
      // an entry from another repo below them is still there to go back to.
      // SABOTAGE: the effect-2 catch dispatches BRANCH_GONE (it clears every context's entries).
      it('the branch left is gone: only its own entries go, and Back still reaches the repo before it', async () => {
        let onExp = true;
        const m = await inOpsOnTwo((r, b) => {
          if (r === 'ops-kb' && b === 'exp/try' && !onExp) throw gone404();
          return r === 'ops-kb' && (b === 'exp/try' || (!b && onExp)) ? EXP : statusOf(b || 'machine/test');
        }, 'core-kb');
        await toCore();
        onExp = false;
        m.status.mockClear();
        await back();
        await waitFor(() => expect(m.status).toHaveBeenCalledWith('ops-kb', 'exp/try'));
        await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
        await act(async () => { await new Promise(r => setTimeout(r, 60)); });
        expect(body()).not.toContain('no fact at path');
        expect(screen.getByRole('button', { name: 'Back' })).toBeEnabled();
        await back();
        await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('core-kb'));
        await waitFor(() => expect(body()).toContain('BODY OF ' + CORE));
      });

      // SABOTAGE: still() in the navReturn landing is () => true.
      for (const ok of [true, false]) it(`a late ${ok ? 'answer' : '404'} for the branch left, after the reader picked another repo, changes nothing there`, async () => {
        const h = hold();
        const m = await awayFromExp(() => h.p);
        await askedForTheBranchLeft(m);
        await toCore();
        await settleNow(h, ok);
        expect(screen.getByTestId('target-label').textContent).toBe('core-kb');
        expect(screen.queryByTestId('since-fork-toggle')).toBeNull();
        expect(body()).toContain('BODY OF ' + CORE);
        expect(m.fact.mock.calls.some(c => c[1] === 'exp/try')).toBe(false);
      });

      // SABOTAGE: every failure treated as a gone branch.
      it('a failure that does not say the branch is gone: the repo\'s normal view, and the trail before it is kept', async () => {
        const m = await awayFromExp(() => { throw new Error('network down'); });
        await askedForTheBranchLeft(m);
        await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
        await act(async () => { await new Promise(r => setTimeout(r, 60)); });
        expect(body()).not.toContain('BODY OF ' + TWO);
        expect(body()).not.toContain('no fact at path');
        expect(screen.getByRole('button', { name: 'Back' })).toBeEnabled();
      });

      // SABOTAGE: NAV_RETURN_LAND restores branchWritable from the entry.
      it('the write gate comes from the branch read again, not from what the entry held', async () => {
        const m = await awayFromExp(() => ({ ...EXP, writable: false }));
        await askedForTheBranchLeft(m);
        await waitFor(() => expect(body()).toContain('BODY OF ' + TWO));
        await waitFor(() => expect(screen.getByTestId('retract-btn')).toBeDisabled());
      });

      // SABOTAGE: a reader move does not drop navReturn (APPLY_NAV), or enterBranch does not.
      it('a fact the reader opens while the return is on its way is not overwritten by its landing', async () => {
        const h = hold();
        const m = await awayFromExp(() => h.p);
        await askedForTheBranchLeft(m);
        await waitFor(() => expect(screen.getAllByTestId('chrono-item')).toHaveLength(3));
        fireEvent.click(screen.getAllByTestId('chrono-item')[2]);
        await waitFor(() => expect(body()).toContain('BODY OF ' + THREE));
        await settleNow(h, true);
        expect(body()).toContain('BODY OF ' + THREE);
        expect(body()).not.toContain('BODY OF ' + TWO);
        expect(screen.queryByTestId('since-fork-toggle')).toBeNull();
      });
      it('a branch the reader picks while the return is on its way is not overwritten by its landing', async () => {
        const h = hold();
        const m = await awayFromExp(() => h.p);
        await askedForTheBranchLeft(m);
        fireEvent.click(await screen.findByTestId('toknomitr-branch'));
        fireEvent.click(await screen.findByTestId('toknomitr-branch-option-other'));
        await waitFor(() => expect(m.status).toHaveBeenCalledWith('ops-kb', 'exp/other'));
        await settleNow(h, true);
        expect(body()).not.toContain('BODY OF ' + TWO);
        expect(m.fact.mock.calls.some(c => c[1] === 'exp/try')).toBe(false);
        expect(screen.queryByTestId('since-fork-toggle')).toBeNull();
      });

      // SABOTAGE: canGoBack ignores navReturn.
      it('Back is not offered while a return is on its way, and is again once it has landed', async () => {
        const h = hold();
        const m = await awayFromExp(() => h.p);
        await askedForTheBranchLeft(m);
        expect(screen.getByRole('button', { name: 'Back' })).toBeDisabled();
        await settleNow(h, true);
        await waitFor(() => expect(body()).toContain('BODY OF ' + TWO));
        expect(screen.getByRole('button', { name: 'Back' })).toBeEnabled();
      });
    });

    // A chat's landing in another repo: Back returns to the chat AND puts Browse back
    // in the repo it left, as it was (here: its search). One Back, one entry.
    // SABOTAGE: the landing pushes again (no departed); the switch does not push.
    it('a landing in another repo: Back returns to the chat, and Browse is back in the repo it left, as it was', async () => {
      const m = await toTheChipsFact({ start: 'ops-kb', mode: 'path', ms: 0 });
      const input = within(screen.getByTestId('browse-search')).getByRole('textbox');
      fireEvent.change(input, { target: { value: 'needle' } });
      fireEvent.keyDown(input, { key: 'Enter' });
      await act(async () => { await new Promise(r => setTimeout(r, 60)); });
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      m.fact.mockClear();
      fireEvent.click(await screen.findByTestId('fact-link'), { metaKey: true });
      await landed(0);
      expect(screen.getByTestId('target-label').textContent).toBe('core-kb');
      expect(screen.getByTestId('browse-facet').textContent).toContain('BODY OF ' + PATH);
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(tabNames()).toEqual(['Chat', 'Settings']));
      expect(screen.getByTestId('rail-chat-chat-a')).toHaveAttribute('aria-current', 'true');
      // Browse behind the chat is ops-kb again, before the reader asks for it.
      await waitFor(() => expect(window.location.search).toContain('ctx_repo=u-ops'));
      fireEvent.click(screen.getByTestId('rail-repo-ops-kb'));
      await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
      expect(screen.getByTestId('target-label').textContent).toBe('ops-kb');
      // The search it was in (the list's state, as the URL writes it).
      expect(window.location.search).toContain('l_q=needle');
    });

    // The chat it came from has ended: Back is Browse's, so Browse goes to the repo
    // returned to, facets and all (a repo now being created shows Settings, its only
    // facet). The facets follow the context at render; the return never touches the
    // view (a rail dispatch there would unpin a chat still open: the test above).
    // SABOTAGE: the return is not entered when its entry came from a chat.
    it('a landing in another repo whose chat has since ended: Back takes Browse to the repo left, as a pick of it would', async () => {
      const m = await toTheChipsFact({ start: 'ops-kb', mode: 'path', ms: 0 });
      fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
      fireEvent.click(await screen.findByTestId('fact-link'), { metaKey: true });
      await landed(0);
      expect(screen.getByTestId('target-label').textContent).toBe('core-kb');
      const es = await waitFor(() => { const e = chatStream('chat-a'); expect(e).toBeTruthy(); return e!; });
      act(() => { es.emit('chat_end', { reason: 'deleted' }); });
      await waitFor(() => expect(screen.queryByTestId('rail-chat-chat-a')).toBeNull());
      const { refreshRepoCreates } = await import('./useRepoCreates');
      m.listRepoCreates.mockResolvedValue([{ create_id: 'c-ops', name: 'ops-kb', mode: 'initialize', state: 'running', step: 'index', phase: 'index', index_state: 'indexing', repo: { name: 'ops-kb', uid: 'u-ops' } }]);
      await act(async () => { await refreshRepoCreates(); });
      expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
      fireEvent.keyDown(window, { key: '[', metaKey: true });
      await waitFor(() => expect(screen.getByTestId('target-label').textContent).toMatch(/^ops-kb/));
      await waitFor(() => expect(tabNames()).toEqual(['Settings']));
      expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
    });
  });

  it('the chat ending (chat_end on its stream) removes it from the rail and unpins it', async () => {
    await primeApi();
    chatRoutes();
    await mountApp();
    await createChatThroughTheForm();
    const es = await waitFor(() => {
      const s = chatStream('chat-a');
      expect(s).toBeDefined();
      return s!;
    });
    act(() => { es.emit('chat_end', { reason: 'deleted' }); });
    await waitFor(() => expect(screen.queryByTestId('rail-chat-chat-a')).toBeNull());
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('ops-kb'));
    expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
    expect(screen.queryByTestId('chat-body-chat-a')).toBeNull();
  });

  it('a chat keeps its stream open while a repository is shown', async () => {
    await primeApi();
    chatRoutes([CHAT_A]);
    await mountApp();
    fireEvent.click(await screen.findByTestId('rail-chat-chat-a'));
    await waitFor(() => expect(openStreams('/chats/events')).toBe(1));
    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    await waitFor(() => expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']));
    expect(openStreams('/chats/events')).toBe(1);
  });
});
