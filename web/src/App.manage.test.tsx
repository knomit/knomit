import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { SUBSCRIPTION_TITLE } from './state';

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
      listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
    // null = this server has no OAuth issuer (404), so no Authorizations tab.
    listOAuthPending: vi.fn().mockResolvedValue(null),
    getFleet: vi.fn().mockRejectedValue(new Error('not in a fleet')), listFleetMembers: vi.fn().mockResolvedValue([]),
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
  // Restore the shared baseline, never delete it: this afterEach runs before
  // test-setup.ts's cleanup() (vitest "stack" hook order), see
  // uninstallFakeEventSource.
  uninstallFakeEventSource();
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
    expect(screen.getByTestId('browse-toolbar')).toBeInTheDocument();
    expect(screen.getByTestId('target-label')).toBeInTheDocument();

    await enterManage();

    // Naming the browsed target, or offering a fact filter, here would be
    // about a surface that is not on screen.
    expect(screen.queryByTestId('browse-toolbar')).not.toBeInTheDocument();
    expect(screen.queryByTestId('target-label')).not.toBeInTheDocument();
  });

  // The repo page's Browse button is gone (the Browse tab replaced it), and
  // with it the two tests of leaving Manage by it; App.facets.test.tsx pins
  // the re-read on leaving Settings instead.

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

  // The connect commit's lock is the Settings facet's now (Manage hosts no
  // connect flow); App.facets.test.tsx pins that the rail, the tabs and the
  // gear hold while it writes.
});

// #324: the instance actions (+ repo, + lens — the rail's now — and the Logs
// tab) were gated on the BROWSED repo's read-only state. A fresh origin has no remembered
// context, so pickRepo lands on the first sorted repo — here a subscription —
// and the instance looked read-only although the server accepts every write.
describe('Manage on a writable instance while browsing a subscription', () => {
  beforeEach(() => { localStorage.clear(); localStorage.setItem('knomit.rail', '1'); });

  it('keeps the instance actions available and fact authoring blocked', async () => {
    const m = await primeApi();
    m.getRepo.mockResolvedValue({ name: 'alpha', description: '', mode: 'subscribe', read_branch: 'main' });
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
    // Manage assertions below cannot pass merely by running before it arrives.
    fireEvent.click(await screen.findByTestId('dir-entry'));
    await waitFor(() => {
      const retract = screen.getByTestId('retract-btn');
      expect(retract).toBeDisabled();
      expect(retract).toHaveAttribute('title', SUBSCRIPTION_TITLE);
    });

    expect(screen.getByTestId('rail-new-repo')).toBeEnabled();
    expect(screen.getByTestId('rail-new-lens')).toBeEnabled();
    await enterManage();
    expect(await screen.findByTestId('repomgr-logs')).toBeInTheDocument();
  });
});
