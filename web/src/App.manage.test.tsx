import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import App from './App';
import { installFakeEventSource } from './testEventSource';

// Manage is a MODE, not a dialog. These pin the three claims that distinguish
// the two, because each was a deliberate design call and each is easy to undo
// by accident:
//
//   1. It REPLACES the browse panes rather than floating over them.
//   2. ONE control at ONE anchor both enters and leaves it (the gear), so the
//      way out is never somewhere the way in was not.
//   3. Escape leaves it, and claims the key ahead of the clear-filters branch.
//
// The zero-repo variant of the mode (locked, no way out) lives in
// App.norepos.test.tsx, which owns the empty-repo-list fixture.

const STATUS = {
  head: 'aaaaaaa1111',
  branch: 'machine/test',
  index_commit: 'aaaaaaa1111',
  embeddings_enabled: false,
  ontology_root: 'kb',
  index_state: 'ready',
  index_done: 0,
  index_total: 0,
  index_percent: 100,
};

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    fetchVersion: vi.fn().mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly: false }),
    // The connect sub-page's backend. Only the commit-lock test drives these;
    // everywhere else they exist so the wizard cannot reach the network.
    createSession: vi.fn(),
    streamTest: vi.fn(() => () => {}),
    streamPreview: vi.fn(() => () => {}),
    streamApply: vi.fn(),
    streamCommit: vi.fn(),
    deleteSession: vi.fn().mockResolvedValue(undefined),
    api: {
      listClientSessions: vi.fn().mockResolvedValue({ sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360 } }),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(),
      listBranchNames: vi.fn(),
    },
  };
});

async function primeApi() {
  const { api } = await import('./api');
  const m = api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  m.repos.mockResolvedValue([{ name: 'alpha' }, { name: 'beta' }]);
  m.listLenses.mockResolvedValue([]);
  m.listArchived.mockResolvedValue([]);
  m.getAgentBranch.mockResolvedValue('machine/test');
  m.status.mockResolvedValue(STATUS);
  m.getOrigin.mockResolvedValue(null);
  m.browse.mockResolvedValue({ path: 'kb', children: [] });
  m.recent.mockResolvedValue({ facts: [], total: 0 });
  m.search.mockResolvedValue({ results: [] });
  m.stats.mockResolvedValue(null);
  m.activity.mockResolvedValue(null);
  m.completions.mockResolvedValue([]);
  m.getRepo.mockResolvedValue({ name: 'alpha', description: '' });
  m.listBranchNames.mockResolvedValue(['main', 'machine/test']);
  return m;
}

async function mountApp() {
  render(<App />);
  await screen.findByTestId('status-footer');
}

const enterManage = async () => {
  await act(async () => { fireEvent.click(screen.getByTestId('toknomitr-manage-btn')); });
  await waitFor(() => expect(screen.getByTestId('manage-surface')).toBeInTheDocument());
};

beforeEach(async () => {
  installFakeEventSource();
  vi.clearAllMocks();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  vi.spyOn(console, 'info').mockImplementation(() => {});
  await primeApi();
});

afterEach(() => {
  vi.restoreAllMocks();
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
});

describe('Manage as a mode', () => {
  it('replaces the browse panes instead of floating over them', async () => {
    await mountApp();
    // The browse surface is up: the library column and the fact pane.
    expect(screen.getByTestId('library-header')).toBeInTheDocument();

    await enterManage();

    // Not "still there, behind an overlay" — gone. This is the whole point of
    // the refactor: the detail pane gets the window, not a 900×620 box on top
    // of a browse surface nobody can read.
    expect(screen.queryByTestId('library-header')).not.toBeInTheDocument();
  });

  it('uses one control at one anchor to enter and to leave', async () => {
    await mountApp();
    const gear = screen.getByTestId('toknomitr-manage-btn');
    expect(gear.getAttribute('aria-pressed')).toBe('false');

    await enterManage();
    // The SAME node — not a second control that appeared elsewhere. A way out
    // on the opposite side of the window makes the reader hunt for a button
    // they just used.
    expect(screen.getByTestId('toknomitr-manage-btn')).toBe(gear);
    expect(gear.getAttribute('aria-pressed')).toBe('true');
    expect(gear.getAttribute('aria-label')).toContain('Leave Manage');

    await act(async () => { fireEvent.click(gear); });
    await waitFor(() => expect(screen.queryByTestId('manage-surface')).not.toBeInTheDocument());
    expect(screen.getByTestId('library-header')).toBeInTheDocument();
  });

  it('names the surface you return to, so the destination needs no label in the bar', async () => {
    await mountApp();
    await enterManage();
    expect(screen.getByTestId('toknomitr-manage-btn').getAttribute('title')).toContain('alpha');
  });

  it('leaves on Escape, ahead of the clear-filters binding', async () => {
    await mountApp();
    await enterManage();

    await act(async () => { fireEvent.keyDown(window, { key: 'Escape' }); });

    await waitFor(() => expect(screen.queryByTestId('manage-surface')).not.toBeInTheDocument());
    expect(screen.getByTestId('library-header')).toBeInTheDocument();
  });

  it('drops the browse context chips and the filter, which act on a surface it is not showing', async () => {
    await mountApp();
    expect(screen.getByTestId('toknomitr-search')).toBeInTheDocument();

    await enterManage();

    // Offering a repo switcher or a fact filter here would act on a surface
    // that is not on screen.
    expect(screen.queryByTestId('toknomitr-search')).not.toBeInTheDocument();
    expect(screen.queryByTestId('toknomitr-repo-select')).not.toBeInTheDocument();
  });

  it("a page's Browse both leaves Manage and switches the surface", async () => {
    await mountApp();
    await enterManage();

    // Pick the other repo in the rail, then Browse it. Unlike the top-bar
    // step-out — which leaves the mode and changes nothing — this is a
    // destination, which is why it is the one control that keeps the word.
    await act(async () => { fireEvent.click(screen.getByTestId('repomgr-item-beta')); });
    await act(async () => { fireEvent.click(screen.getByTestId('repo-browse')); });

    await waitFor(() => expect(screen.queryByTestId('manage-surface')).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByTestId('toknomitr-repo-select').textContent).toContain('beta'));
  });

  // Manage is where a remote gets repaired, so every way OUT of it owes the
  // banner a re-read. Browse used to skip it — it flipped the flag itself
  // instead of going through the close path — so fixing a remote and then
  // leaving by Browse left the failure on screen until the recheck timer
  // eventually came round, up to a minute later.
  it('re-reads the remote when Browse is the way out, not just the gear', async () => {
    const m = await primeApi();
    await mountApp();
    await enterManage();

    // The repo you are already browsing: the [state.repo] effect will not fire,
    // so a re-read here can only have come from the close path.
    await act(async () => { fireEvent.click(screen.getByTestId('repomgr-item-alpha')); });
    await waitFor(() => expect(screen.getByTestId('repo-browse')).toBeInTheDocument());
    const before = m.getOrigin.mock.calls.length;

    await act(async () => { fireEvent.click(screen.getByTestId('repo-browse')); });

    await waitFor(() => expect(screen.queryByTestId('manage-surface')).not.toBeInTheDocument());
    await waitFor(() => expect(m.getOrigin.mock.calls.length).toBeGreaterThan(before));
  });

  // Every other shortcut in the app drives the BROWSE surface, which Manage has
  // replaced. Firing them from in here pops the nav stack or ends a history
  // excursion invisibly, and you find out on the way out — looking at a
  // different fact than the one you left. The INPUT/TEXTAREA guard is no help:
  // `noMouseFocus` deliberately leaves focus on <body> after a rail click.
  it('claims the keyboard while it owns the window, except for Escape', async () => {
    await mountApp();

    // Browsing, these are window-level commands and the handler consumes them.
    expect(fireEvent.keyDown(window, { key: 'Backspace' })).toBe(false);
    expect(fireEvent.keyDown(window, { key: '[', metaKey: true })).toBe(false);

    await enterManage();

    // In Manage nothing consumes them, because nothing acts on them.
    expect(fireEvent.keyDown(window, { key: 'Backspace' })).toBe(true);
    expect(fireEvent.keyDown(window, { key: 'Delete' })).toBe(true);
    expect(fireEvent.keyDown(window, { key: '[', metaKey: true })).toBe(true);
    expect(fireEvent.keyDown(window, { key: 'h' })).toBe(true);
    expect(screen.getByTestId('manage-surface')).toBeInTheDocument();

    // Escape is the exception: dismissing the thing on top is unambiguous when
    // the thing on top is the whole window.
    await act(async () => { fireEvent.keyDown(window, { key: 'Escape' }); });
    await waitFor(() => expect(screen.queryByTestId('manage-surface')).not.toBeInTheDocument());
  });

  // ...with one exception of its own. Closing Manage unmounts the connect
  // sub-page, and its commit stream has no abort and no undo: the store swap
  // and index rebuild run on with nothing listening. The manager withholds its
  // rail and the wizard withholds its crumb, but BOTH of the app's exits went
  // straight past them — and Escape is precisely the reflex of a reader who has
  // just found every visible control disabled.
  it('holds Escape and the step-out while a connect commit is writing', async () => {
    const mod = await import('./api');
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
    // Never resolves — the commit is in flight for the rest of the test.
    m.streamCommit.mockImplementation(() => new Promise(() => {}));

    await mountApp();
    await enterManage();
    await act(async () => { fireEvent.click(screen.getByTestId('repomgr-item-alpha')); });
    await act(async () => { fireEvent.click(await screen.findByTestId('remote-connect')); });
    await act(async () => { fireEvent.change(await screen.findByTestId('wizard-url'), { target: { value: 'https://example.com/repo.git' } }); });
    await act(async () => { fireEvent.click(screen.getByTestId('wizard-test')); });
    await act(async () => { fireEvent.click(await screen.findByTestId('wizard-connect')); });
    await waitFor(() => expect((screen.getByTestId('wizard-crumb-back') as HTMLButtonElement).disabled).toBe(true));

    await act(async () => { fireEvent.keyDown(window, { key: 'Escape' }); });
    expect(screen.getByTestId('manage-surface')).toBeInTheDocument();

    // The step-out says so rather than silently swallowing the click: it stays
    // where it was — a control that vanished would read as the window losing
    // its way out rather than holding it — and carries the reason.
    const gear = screen.getByTestId('toknomitr-manage-btn') as HTMLButtonElement;
    expect(gear.disabled).toBe(true);
    expect(gear.getAttribute('title')).toContain('leave it running');
    await act(async () => { fireEvent.click(gear); });
    expect(screen.getByTestId('manage-surface')).toBeInTheDocument();
    expect(screen.getByTestId('remote-connect-wizard')).toBeInTheDocument();
  });

  // ...and it must hold from the moment the lock is VISIBLE, which is a
  // separate claim and was broken.
  //
  // The busy flag is relayed upward by effects — wizard → RepoManager → App —
  // and those were passive. React runs the DOM commit and the passive-effect
  // flush in separate tasks for any update that did not come from a discrete
  // event, and the commit that PAINTS the lock is the wizard's own, while the
  // step reaches 'committing' in a promise continuation. So there was a window
  // in which the reader could see that they were not allowed to leave, press
  // Escape, and leave anyway — mid-write.
  //
  // The test above cannot see that window: `act()` drives React's scheduler
  // itself and collapses the two tasks into one. So this one uses NATIVE events
  // for both the click that starts the commit and the Escape, and fires the
  // Escape from a MutationObserver the instant the lock paints.
  it('refuses Escape pressed in the same task the commit lock paints in, and accepts it once the lock lifts', async () => {
    const mod = await import('./api');
    const m = mod as unknown as Record<string, ReturnType<typeof vi.fn>>;
    m.createSession.mockResolvedValue({ session_id: 'sess-paint' });
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
    // Held open, and the event sink kept, so the control arm below can end the
    // commit and watch the lock lift.
    let endCommit: ((e: unknown) => void) | undefined;
    m.streamCommit.mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) =>
      new Promise(() => { endCommit = onEvent; }));

    await mountApp();
    await enterManage();
    await act(async () => { fireEvent.click(screen.getByTestId('repomgr-item-alpha')); });
    await act(async () => { fireEvent.click(await screen.findByTestId('remote-connect')); });
    await act(async () => { fireEvent.change(await screen.findByTestId('wizard-url'), { target: { value: 'https://example.com/repo.git' } }); });
    await act(async () => { fireEvent.click(screen.getByTestId('wizard-test')); });

    const surfaces = () => document.querySelectorAll('[data-testid=manage-surface]').length;
    const crumb = () => document.querySelector('[data-testid=wizard-crumb-back]') as HTMLButtonElement | null;
    const locked = () => !!crumb()?.disabled;
    let surfaceWhenLockPainted = -1;

    let observer: MutationObserver | undefined;
    const fired = new Promise<void>((done, fail) => {
      // Without this the failure mode of "the lock never paints" is a bare 5s
      // timeout on an await, which says nothing about why.
      const timer = setTimeout(() => fail(new Error(
        'the commit lock never painted: wizard-crumb-back was never disabled')), 4000);
      observer = new MutationObserver(() => {
        if (!locked()) return;
        clearTimeout(timer);
        surfaceWhenLockPainted = surfaces();
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
        done();
      });
      observer.observe(document.body, { childList: true, subtree: true, attributes: true });
    });

    try {
      // NATIVE click, not fireEvent: the commit must start outside act().
      (await screen.findByTestId('wizard-connect')).dispatchEvent(
        new MouseEvent('click', { bubbles: true, cancelable: true }));
      await fired;
    } finally {
      observer?.disconnect();
    }
    await act(async () => { await Promise.resolve(); });

    // Guards the guard: if the lock never painted, the Escape above would be
    // testing nothing and the assertion below would pass for the wrong reason.
    expect(surfaceWhenLockPainted).toBe(1);
    expect(locked()).toBe(true);
    expect(screen.getByTestId('manage-surface')).toBeInTheDocument();

    // THE CONTROL ARM, and it is what stops this test passing merely because
    // nothing was listening: end the commit so the lock lifts, then press the
    // same key the same way. If App's keydown listener were gone, Manage would
    // survive here too and the refusal above would have proved nothing.
    await act(async () => { endCommit?.({ phase: 'error', message: 'remote hung up' }); });
    await waitFor(() => expect(locked()).toBe(false));
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await waitFor(() => expect(screen.queryByTestId('manage-surface')).not.toBeInTheDocument());
  });
});
