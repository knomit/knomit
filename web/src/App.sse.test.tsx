import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import App from './App';

// Characterization tests for the SSE wiring in App (the effect keyed on
// [state.repo, state.branch]). These pin CURRENT behavior — the console lines
// the stream emits, the task-state update, the head refresh on task
// completion, the 500-entry console ring buffer, and teardown/resubscribe on a
// repo switch — so a refactor that moves console state out of AppState or
// re-shapes the render tree has a safety net.
//
// Everything is asserted through OBSERVABLE output: rendered console lines,
// the status-footer task pill, the TopBar head chip, the remote-error banner,
// or (for the ring buffer) the reducer's resulting state. Nothing asserts on
// dispatch call shapes.

// ---------------------------------------------------------------------------
// Fake EventSource. jsdom has none, and App constructs one directly, so we
// install a global that records instances and lets a test emit events by hand.
// ---------------------------------------------------------------------------
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  url: string;
  readyState = FakeEventSource.OPEN;
  closeCount = 0;
  closedByClient = false;
  private listeners = new Map<string, Set<(e: unknown) => void>>();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, fn: (e: unknown) => void) {
    let set = this.listeners.get(type);
    if (!set) { set = new Set(); this.listeners.set(type, set); }
    set.add(fn);
  }

  removeEventListener(type: string, fn: (e: unknown) => void) {
    this.listeners.get(type)?.delete(fn);
  }

  close() { this.closeCount += 1; this.closedByClient = true; this.readyState = FakeEventSource.CLOSED; }

  /**
   * Deliver an event to every listener registered for `type`. A stream the
   * client closed delivers nothing more, mirroring the real EventSource — this
   * is what makes "the old subscription is really torn down" observable.
   */
  emit(type: string, data?: unknown) {
    if (this.closedByClient) return;
    const payload = { type, data: data === undefined ? '' : JSON.stringify(data) };
    for (const fn of [...(this.listeners.get(type) ?? [])]) fn(payload);
  }
}

// ---------------------------------------------------------------------------
// api mock. Only the network surface (`api`, `fetchVersion`) is stubbed; the
// pure helpers (apiUrl, parseFilterQuery, …) stay real because the render tree
// depends on them.
// ---------------------------------------------------------------------------
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
    api: {
      repos: vi.fn(),
      listLenses: vi.fn(),
      listArchived: vi.fn(),
      getAgentBranch: vi.fn(),
      status: vi.fn(),
      getOrigin: vi.fn(),
      getLens: vi.fn(),
      browse: vi.fn(),
      recent: vi.fn(),
      search: vi.fn(),
      stats: vi.fn(),
      activity: vi.fn(),
      explain: vi.fn(),
      completions: vi.fn(),
      fact: vi.fn(),
      factCommits: vi.fn(),
      commitDetail: vi.fn(),
    },
  };
});

async function apiMock() {
  const { api } = await import('./api');
  return api as unknown as Record<string, ReturnType<typeof vi.fn>>;
}

async function primeApi() {
  const api = await apiMock();
  api.repos.mockResolvedValue([{ name: 'alpha' }, { name: 'beta' }]);
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
  return api;
}

// ---------------------------------------------------------------------------
// Console readout. The console starts collapsed; `expandConsole` clicks the
// status footer to open it. `consoleLines` reads the rendered entry rows — leaf
// divs inside the console whose text starts with the entry timestamp.
// ---------------------------------------------------------------------------
function expandConsole() {
  fireEvent.click(screen.getByTestId('console'));
}

function consoleLines(): string[] {
  const root = screen.getByTestId('console');
  return Array.from(root.querySelectorAll('div'))
    .filter(d => !d.querySelector('div'))
    .map(d => d.textContent ?? '')
    .filter(t => /^\d{1,2}:\d{2}:\d{2}/.test(t));
}

function hasLine(fragment: string): boolean {
  return consoleLines().some(l => l.includes(fragment));
}

/** Render App and wait until the SSE subscription for the bootstrapped branch exists. */
async function mountApp(): Promise<FakeEventSource> {
  render(<App />);
  await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
  await screen.findByTestId('console');
  return FakeEventSource.instances[0];
}

beforeEach(async () => {
  FakeEventSource.instances = [];
  // NOTE: this jsdom environment provides no localStorage at all (the app's
  // reads are try/catch-wrapped), so there is no persisted context to clear —
  // App always bootstraps onto the first repo from api.repos().
  vi.clearAllMocks();
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
  await primeApi();
});

afterEach(() => {
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
});

describe('App SSE subscription', () => {
  it('subscribes once the bootstrapped branch is known, on the repo/branch URL', async () => {
    const es = await mountApp();
    expect(es.url).toContain('/api/v1/repos/alpha/branches/machine:test/events');
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('logs "[events] reconnected" only after an outage it actually reported', async () => {
    const es = await mountApp();
    act(() => { es.emit('open'); });
    expandConsole();
    expect(hasLine('[events] reconnected')).toBe(false);

    // A second open with no disconnect between is not a recovery — there was
    // nothing to recover from. (This used to log on ANY repeat open, which is
    // one half of the flap noise Fix 6 removes.)
    act(() => { es.emit('open'); });
    expect(hasLine('[events] reconnected')).toBe(false);

    // A reported outage, then an open: now it is a recovery and says so.
    es.readyState = FakeEventSource.CONNECTING;
    act(() => { es.emit('error'); });
    expect(hasLine('[events] connection lost')).toBe(true);
    act(() => { es.emit('open'); });
    expect(hasLine('[events] reconnected')).toBe(true);
  });

  it('logs "stream closed" once per outage when the stream is CLOSED', async () => {
    const es = await mountApp();
    es.readyState = FakeEventSource.CLOSED;
    act(() => { es.emit('error'); es.emit('error'); });

    expandConsole();
    const closed = consoleLines().filter(l => l.includes('[events] stream closed'));
    expect(closed).toHaveLength(1);
    expect(closed[0]).toContain('head pill may be stale');
    expect(hasLine('[events] connection lost')).toBe(false);
  });

  it('logs "connection lost — retrying" when the stream is still reconnecting', async () => {
    const es = await mountApp();
    es.readyState = FakeEventSource.CONNECTING;
    act(() => { es.emit('error'); es.emit('error'); });

    expandConsole();
    const lost = consoleLines().filter(l => l.includes('[events] connection lost — retrying'));
    expect(lost).toHaveLength(1);
    expect(hasLine('[events] stream closed')).toBe(false);
  });

  // Fix 6. The 'open' re-arm that makes a genuine second outage reportable also
  // re-arms on every EventSource retry, so a backend that accepts and then
  // immediately drops produced a disconnect + reconnect PAIR per cycle. At the
  // ~3s retry that is ~40 lines/min — the 500-entry ring flushes the task and
  // remote lines the console exists for in about 12 minutes. Outages are now
  // counted over a rolling window and go quiet behind one summary line.
  it('a flapping stream stops spamming the ring after a few cycles', async () => {
    const es = await mountApp();
    expandConsole();

    // Ten accept-then-drop cycles inside the flap window.
    es.readyState = FakeEventSource.CONNECTING;
    for (let i = 0; i < 10; i++) {
      act(() => { es.emit('error'); });
      act(() => { es.emit('open'); });
    }

    const lost  = consoleLines().filter(l => l.includes('[events] connection lost')).length;
    const recon = consoleLines().filter(l => l.includes('[events] reconnected')).length;
    // Unbounded, this is 10 + 10. Bounded, it is FLAP_LIMIT of each…
    expect(lost).toBe(3);
    expect(recon).toBe(3);
    // …plus exactly one summary line, emitted once and not repeated.
    expect(consoleLines().filter(l => l.includes('[events] stream flapping')).length).toBe(1);
    // The whole storm costs well under a tenth of the 500-entry ring.
    expect(lost + recon + 1).toBeLessThan(10);
  });
});

describe('App SSE — task events', () => {
  it('a running task both updates the task state and writes one console line', async () => {
    const es = await mountApp();
    act(() => { es.emit('task', { op: 'sync', status: 'running', message: 'syncing…' }); });

    // SET_TASK: the collapsed status footer shows the active task pill.
    expect(screen.getByText('[sync] syncing…')).toBeTruthy();

    // CONSOLE_LOG: the same event also produced exactly one console line.
    expandConsole();
    const lines = consoleLines().filter(l => l.includes('[sync] syncing…'));
    expect(lines).toHaveLength(1);
  });

  it('prefixes the console line with the event repo when the event carries one', async () => {
    const es = await mountApp();
    act(() => { es.emit('task', { op: 'index', status: 'running', message: 'building', repo: 'beta' }); });

    expandConsole();
    expect(hasLine('[beta/index] building')).toBe(true);
  });

  it('falls back to the status when the event has no message', async () => {
    const es = await mountApp();
    act(() => { es.emit('task', { op: 'gc', status: 'running', message: '' }); });

    expandConsole();
    expect(hasLine('[gc] running')).toBe(true);
  });

  it('refreshes head on a done task, updating the TopBar commit chip', async () => {
    const api = await apiMock();
    const es = await mountApp();
    const callsBefore = api.status.mock.calls.length;
    api.status.mockResolvedValue({ ...STATUS, head: 'bbbbbbb2222' });

    await act(async () => { es.emit('task', { op: 'sync', status: 'done', message: 'ok' }); });

    expect(api.status.mock.calls.length).toBe(callsBefore + 1);
    expect(api.status).toHaveBeenLastCalledWith('alpha', 'machine/test');
    await waitFor(() => expect(screen.getByTestId('toknomitr-commit')).toHaveTextContent('bbbbbbb'));
  });

  it('refreshes head on an error task too, and logs the line at error level', async () => {
    const api = await apiMock();
    const es = await mountApp();
    const callsBefore = api.status.mock.calls.length;
    api.status.mockResolvedValue({ ...STATUS, head: 'ccccccc3333' });

    await act(async () => { es.emit('task', { op: 'sync', status: 'error', message: 'boom' }); });

    expect(api.status.mock.calls.length).toBe(callsBefore + 1);
    await waitFor(() => expect(screen.getByTestId('toknomitr-commit')).toHaveTextContent('ccccccc'));

    expandConsole();
    expect(hasLine('[sync] boom')).toBe(true);
    // Level is observable through the console's error counter.
    expect(screen.getByText('1 err')).toBeTruthy();
  });

  it('does NOT refresh head for a running task', async () => {
    const api = await apiMock();
    const es = await mountApp();
    const callsBefore = api.status.mock.calls.length;

    await act(async () => { es.emit('task', { op: 'sync', status: 'running', message: 'go' }); });

    expect(api.status.mock.calls.length).toBe(callsBefore);
  });

  it('logs "[status] refresh failed" when the post-task status refresh rejects', async () => {
    const api = await apiMock();
    const es = await mountApp();
    api.status.mockRejectedValue(new Error('nope'));

    await act(async () => { es.emit('task', { op: 'sync', status: 'done', message: 'ok' }); });

    expandConsole();
    await waitFor(() => expect(hasLine('[status] refresh failed: Error: nope')).toBe(true));
  });
});

describe('App SSE — status and remote events', () => {
  it('a status event with a head updates the TopBar commit chip', async () => {
    const es = await mountApp();
    act(() => { es.emit('status', { head: 'ddddddd4444' }); });
    expect(screen.getByTestId('toknomitr-commit')).toHaveTextContent('ddddddd');
  });

  it('a remote error raises the banner and logs "[remote] <error>"', async () => {
    const es = await mountApp();
    act(() => { es.emit('sync_error', { error: 'auth failed' }); });

    expect(screen.getByTestId('remote-error-banner')).toHaveTextContent('auth failed');
    expandConsole();
    expect(hasLine('[remote] auth failed')).toBe(true);
  });

  it('a subsequent clean remote event clears the banner', async () => {
    const es = await mountApp();
    act(() => { es.emit('push_error', { error: 'auth failed' }); });
    expect(screen.queryByTestId('remote-error-banner')).toBeTruthy();

    act(() => { es.emit('push_ok', {}); });
    expect(screen.queryByTestId('remote-error-banner')).toBeNull();
  });

  it('summarizes a sync_ok reconcile (main + agent) as one console line', async () => {
    const es = await mountApp();
    act(() => {
      es.emit('sync_ok', { main: { mode: 'ff' }, agent: { mode: 'rebase', num_replayed: 3 } });
    });

    expandConsole();
    expect(hasLine('[remote] main fast-forwarded, 3 commit(s) replayed onto agent (rewind)')).toBe(true);
  });

  it('summarizes a rewind + merge reconcile', async () => {
    const es = await mountApp();
    act(() => {
      es.emit('sync_ok', { main: { mode: 'rewound' }, agent: { mode: 'merge' } });
    });

    expandConsole();
    expect(hasLine('[remote] main rewound, main merged into agent')).toBe(true);
  });

  it('emits no reconcile line when nothing changed on either side', async () => {
    const es = await mountApp();
    act(() => { es.emit('sync_ok', { main: { mode: 'noop' }, agent: { mode: 'noop' } }); });

    expandConsole();
    expect(consoleLines().filter(l => l.includes('[remote]'))).toHaveLength(0);
  });

  it('emits no reconcile line for a bare sync_ok with no reconcile detail', async () => {
    const es = await mountApp();
    act(() => { es.emit('sync_ok', {}); });

    expandConsole();
    expect(consoleLines().filter(l => l.includes('[remote]'))).toHaveLength(0);
  });
});

describe('App SSE — teardown and resubscribe', () => {
  it('closes the old stream and opens a new one when the repo changes', async () => {
    const es = await mountApp();
    expect(es.closeCount).toBe(0);

    // Switch repos through the TopBar context switcher — the only user-facing
    // path that changes state.repo.
    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    await act(async () => {
      fireEvent.click(screen.getByTestId('toknomitr-repo-option-beta'));
    });

    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    expect(es.closeCount).toBe(1);
    expect(FakeEventSource.instances[1].url).toContain('/repos/beta/branches/machine:test/events');
  });

  it('after a resubscribe the new stream drives the app and the old one is dead', async () => {
    const es = await mountApp();
    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    await act(async () => {
      fireEvent.click(screen.getByTestId('toknomitr-repo-option-beta'));
    });
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));

    // The closed stream delivers nothing (see FakeEventSource.emit).
    act(() => { es.emit('task', { op: 'stale', status: 'running', message: 'from-old' }); });
    expect(screen.queryByText('[stale] from-old')).toBeNull();

    act(() => { FakeEventSource.instances[1].emit('task', { op: 'sync', status: 'running', message: 'from-new' }); });
    expect(screen.getByText('[sync] from-new')).toBeTruthy();
  });

  it('unmounting closes the stream', async () => {
    const { unmount } = render(<App />);
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
    const es = FakeEventSource.instances[0];
    unmount();
    expect(es.closeCount).toBe(1);
  });
});

// The console ring buffer itself is unit-tested in consoleStore.test.ts (cap,
// trim direction, emission order, level, id uniqueness, reducer purity). Those
// reducer cases used to be duplicated verbatim here; they exercised the store,
// not the stream, so they have been removed rather than kept in two places.
// What earns its place in THIS file is the end-to-end path below: SSE event →
// dispatch → store → rendered row.

describe('console ring buffer — end to end through the SSE stream', () => {
  it('a burst past the cap keeps the newest lines and drops the oldest', async () => {
    const es = await mountApp();
    await act(async () => {
      for (let i = 0; i < 520; i++) {
        es.emit('task', { op: 'burst', status: 'running', message: `n${String(i).padStart(4, '0')}` });
      }
    });

    expandConsole();
    // The console header's info counter is derived straight from the entry
    // list, so it pins the cap without depending on how rows are keyed/rendered.
    expect(screen.getByText('500')).toBeTruthy();
    // Direction of the trim: newest survive, oldest are evicted. (The exact
    // ordering at the boundary is pinned deterministically by the reducer
    // tests above.)
    expect(hasLine('[burst] n0519')).toBe(true);
    expect(hasLine('[burst] n0000')).toBe(false);
    expect(hasLine('[burst] n0019')).toBe(false);
  });
});
