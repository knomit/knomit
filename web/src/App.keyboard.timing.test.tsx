import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import App from './App';

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

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  readyState = 1;
  constructor(url: string) { this.url = url; FakeEventSource.instances.push(this); }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

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

beforeEach(async () => {
  FakeEventSource.instances = [];
  vi.clearAllMocks();
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
  vi.spyOn(console, 'error').mockImplementation(() => {});
  vi.spyOn(console, 'info').mockImplementation(() => {});
  await primeApi();
});
afterEach(() => {
  vi.restoreAllMocks();
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
});

describe('Manage holds the keyboard from the moment the lock is visible', () => {
  it('refuses Escape pressed in the same task the commit lock paints in', async () => {
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
    m.streamCommit.mockImplementation(() => new Promise(() => {}));

    render(<App />);
    await screen.findByTestId('status-footer');
    await act(async () => { fireEvent.click(screen.getByTestId('toknomitr-manage-btn')); });
    await waitFor(() => expect(screen.getByTestId('manage-surface')).toBeInTheDocument());
    await act(async () => { fireEvent.click(screen.getByTestId('repomgr-item-alpha')); });
    await act(async () => { fireEvent.click(await screen.findByTestId('remote-connect')); });
    await act(async () => { fireEvent.change(await screen.findByTestId('wizard-url'), { target: { value: 'https://example.com/repo.git' } }); });
    await act(async () => { fireEvent.click(screen.getByTestId('wizard-test')); });

    let surfaceWhenLockPainted = 0;
    const surface = () => document.querySelectorAll('[data-testid=manage-surface]').length;
    const lockPainted = () => {
      const b = document.querySelector('[data-testid=wizard-crumb-back]') as HTMLButtonElement | null;
      return !!b && b.disabled;
    };

    const fired = new Promise<void>(done => {
      const mo = new MutationObserver(() => {
        if (!lockPainted()) return;
        mo.disconnect();
        surfaceWhenLockPainted = surface();
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
        done();
      });
      mo.observe(document.body, { childList: true, subtree: true, attributes: true });
    });

    // The commit is started by a DISCRETE click, but the step only reaches
    // 'committing' in a later promise continuation — the concurrent lane.
    (await screen.findByTestId('wizard-connect') as HTMLElement).dispatchEvent(
      new MouseEvent('click', { bubbles: true, cancelable: true }));
    await fired;
    // Let every pending effect and render settle, so this asserts the settled
    // outcome rather than the instant before React caught up.
    await act(async () => { await Promise.resolve(); });

    // Guards the guard: if the lock never painted, the Escape below would be
    // testing nothing and the assertion would pass for the wrong reason.
    expect(surfaceWhenLockPainted).toBe(1);
    expect((screen.getByTestId('wizard-crumb-back') as HTMLButtonElement).disabled).toBe(true);
    // The commit is still in flight, so Manage must still be here.
    expect(screen.getByTestId('manage-surface')).toBeInTheDocument();
    expect(screen.getByTestId('remote-connect-wizard')).toBeInTheDocument();
  });
});
