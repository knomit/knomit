import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import App from './App';

// Characterization tests for the SSE wiring in App (the effect keyed on
// [state.repo, state.branch]). These pin CURRENT behavior — the diagnostics the
// stream emits, the task-state update, the head refresh on task completion, and
// teardown/resubscribe on a repo switch.
//
// Everything is asserted through OBSERVABLE output: the status-footer task
// pill, the TopBar head chip, the remote-error banner, or the browser console.
// Nothing asserts on dispatch call shapes.
//
// The diagnostics used to land in an in-app console panel and were read here
// off its rendered rows. That panel is gone (see App's `diag`), so they are read
// off console.error/console.info spies instead — the assertions about WHICH
// lines are emitted, how often, and at what level are unchanged, because that
// behavior is unchanged.

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
      // Reached only when a test opens the repo manager; without them
      // RepoDetail throws and the manager renders its error boundary instead.
      getRepo: vi.fn(),
      listBranchNames: vi.fn(),
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
  api.getRepo.mockResolvedValue({ name: 'alpha', description: '' });
  api.listBranchNames.mockResolvedValue(['main', 'machine/test']);
  return api;
}

// ---------------------------------------------------------------------------
// Diagnostic readout. App routes these through `diag`, which is console.error
// for errors and console.info otherwise, so the spies below ARE the log.
// ---------------------------------------------------------------------------
let errorSpy: ReturnType<typeof vi.spyOn>;
let infoSpy: ReturnType<typeof vi.spyOn>;

const linesFrom = (spy: ReturnType<typeof vi.spyOn>): string[] =>
  spy.mock.calls.map((c: unknown[]) => String(c[0]));

function diagLines(): string[] {
  return [...linesFrom(infoSpy), ...linesFrom(errorSpy)];
}

function hasLine(fragment: string): boolean {
  return diagLines().some(l => l.includes(fragment));
}

function errorLines(): string[] {
  return linesFrom(errorSpy);
}

/** Render App and wait until the SSE subscription for the bootstrapped branch exists. */
async function mountApp(): Promise<FakeEventSource> {
  render(<App />);
  await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
  await screen.findByTestId('status-footer');
  return FakeEventSource.instances[0];
}

beforeEach(async () => {
  FakeEventSource.instances = [];
  // NOTE: this jsdom environment provides no localStorage at all (the app's
  // reads are try/catch-wrapped), so there is no persisted context to clear —
  // App always bootstraps onto the first repo from api.repos().
  vi.clearAllMocks();
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
  // Silenced, not passed through: these tests deliberately provoke the error
  // paths, and an un-mocked console.error would bury the real output.
  errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
  infoSpy = vi.spyOn(console, 'info').mockImplementation(() => {});
  await primeApi();
});

afterEach(() => {
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
  errorSpy.mockRestore();
  infoSpy.mockRestore();
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

    const closed = diagLines().filter(l => l.includes('[events] stream closed'));
    expect(closed).toHaveLength(1);
    expect(closed[0]).toContain('head pill may be stale');
    expect(hasLine('[events] connection lost')).toBe(false);
  });

  it('logs "connection lost — retrying" when the stream is still reconnecting', async () => {
    const es = await mountApp();
    es.readyState = FakeEventSource.CONNECTING;
    act(() => { es.emit('error'); es.emit('error'); });

    const lost = diagLines().filter(l => l.includes('[events] connection lost — retrying'));
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

    // Ten accept-then-drop cycles inside the flap window.
    es.readyState = FakeEventSource.CONNECTING;
    for (let i = 0; i < 10; i++) {
      act(() => { es.emit('error'); });
      act(() => { es.emit('open'); });
    }

    const lost  = diagLines().filter(l => l.includes('[events] connection lost')).length;
    const recon = diagLines().filter(l => l.includes('[events] reconnected')).length;
    // Unbounded, this is 10 + 10. Bounded, it is FLAP_LIMIT of each…
    expect(lost).toBe(3);
    expect(recon).toBe(3);
    // …plus exactly one summary line, emitted once and not repeated.
    expect(diagLines().filter(l => l.includes('[events] stream flapping')).length).toBe(1);
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

    // The same event also produced exactly one diagnostic line.
    const lines = diagLines().filter(l => l.includes('[sync] syncing…'));
    expect(lines).toHaveLength(1);
  });

  it('prefixes the console line with the event repo when the event carries one', async () => {
    const es = await mountApp();
    act(() => { es.emit('task', { op: 'index', status: 'running', message: 'building', repo: 'beta' }); });

    expect(hasLine('[beta/index] building')).toBe(true);
  });

  it('falls back to the status when the event has no message', async () => {
    const es = await mountApp();
    act(() => { es.emit('task', { op: 'gc', status: 'running', message: '' }); });

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

    // Level is observable through WHICH console method received it.
    expect(errorLines().filter(l => l.includes('[sync] boom'))).toHaveLength(1);
  });

  // The post-task refresh reads the branch status but used to keep only the
  // head off it, so index_state was thrown away — a rebuild that repaired the
  // index left the "indexing did not complete" banner on screen.
  it('applies the WHOLE status on a completed task, not just the head', async () => {
    const api = await apiMock();
    api.status.mockResolvedValue({ ...STATUS, index_state: 'error' });
    const es = await mountApp();
    await waitFor(() => expect(screen.getByTestId('index-error-banner')).toBeTruthy());

    api.status.mockResolvedValue({ ...STATUS, index_state: 'ready' });
    await act(async () => { es.emit('task', { op: 'rebuild', status: 'done', message: 'rebuild complete' }); });

    await waitFor(() => expect(screen.queryByTestId('index-error-banner')).toBeNull());
  });

  // Nothing ever returned a task to 'idle', so the footer kept the LAST
  // terminal result for the rest of the session — "[sync] ok" from hours ago
  // reading as something happening now.
  it('a finished task stops occupying the footer once it goes stale', async () => {
    const es = await mountApp();
    vi.useFakeTimers();
    try {
      await act(async () => { es.emit('task', { op: 'sync', status: 'done', message: 'ok' }); });
      expect(screen.getByText('[sync] ok')).toBeTruthy();

      await act(async () => { vi.advanceTimersByTime(10_000); });
      expect(screen.queryByText('[sync] ok')).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps a RUNNING task on the footer no matter how long it runs', async () => {
    const es = await mountApp();
    vi.useFakeTimers();
    try {
      await act(async () => { es.emit('task', { op: 'rebuild', status: 'running', message: '40/900' }); });
      await act(async () => { vi.advanceTimersByTime(120_000); });
      expect(screen.getByText('[rebuild] 40/900')).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
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

    expect(hasLine('[remote] main fast-forwarded, 3 commit(s) replayed onto agent (rewind)')).toBe(true);
  });

  it('summarizes a rewind + merge reconcile', async () => {
    const es = await mountApp();
    act(() => {
      es.emit('sync_ok', { main: { mode: 'rewound' }, agent: { mode: 'merge' } });
    });

    expect(hasLine('[remote] main rewound, main merged into agent')).toBe(true);
  });

  it('emits no reconcile line when nothing changed on either side', async () => {
    const es = await mountApp();
    act(() => { es.emit('sync_ok', { main: { mode: 'noop' }, agent: { mode: 'noop' } }); });

    expect(diagLines().filter(l => l.includes('[remote]'))).toHaveLength(0);
  });

  it('emits no reconcile line for a bare sync_ok with no reconcile detail', async () => {
    const es = await mountApp();
    act(() => { es.emit('sync_ok', {}); });

    expect(diagLines().filter(l => l.includes('[remote]'))).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// The banner is a view of the PERSISTED remote status, not just a latch on the
// last event. It used to be raise-only: a failure that healed on a tick with
// nothing to pull left it standing for the life of the session, which is
// exactly what a long-lived desktop window hit after a transient DNS outage.
// ---------------------------------------------------------------------------
describe('App — remote-error banner lifecycle', () => {
  const ORIGIN_OK = {
    name: 'origin', url: 'https://example.test/kb.git', branch: 'main', interval: 300,
    last_sync_at: '2026-08-04T14:30:29Z', last_status: 'ok', last_error: null,
    push_interval: 300, last_push_at: '2026-08-04T14:30:29Z', last_push_status: 'ok',
    last_push_error: null, auth_method: 'token',
  };
  const ORIGIN_FAILING = { ...ORIGIN_OK, last_status: 'error', last_error: 'dial tcp: no such host' };

  it('seeds the banner from a persisted sync failure on load', async () => {
    const api = await apiMock();
    api.getOrigin.mockResolvedValue(ORIGIN_FAILING);
    await mountApp();

    await waitFor(() => expect(screen.getByTestId('remote-error-banner')).toHaveTextContent('dial tcp: no such host'));
  });

  it('seeds it from a persisted PUSH failure too', async () => {
    const api = await apiMock();
    api.getOrigin.mockResolvedValue({ ...ORIGIN_OK, last_push_status: 'error', last_push_error: 'token expired' });
    await mountApp();

    await waitFor(() => expect(screen.getByTestId('remote-error-banner')).toHaveTextContent('token expired'));
  });

  it('lowers a stale banner on reconnect when the stored status has recovered', async () => {
    const api = await apiMock();
    api.getOrigin.mockResolvedValue(ORIGIN_FAILING);
    const es = await mountApp();
    await waitFor(() => expect(screen.queryByTestId('remote-error-banner')).toBeTruthy());

    // The reconcile loop healed while the stream was down, so the sync_ok that
    // would have cleared this never reached us. Reconnecting re-reads the truth.
    api.getOrigin.mockResolvedValue(ORIGIN_OK);
    es.readyState = FakeEventSource.CONNECTING;
    await act(async () => { es.emit('error'); });
    await act(async () => { es.emit('open'); });

    await waitFor(() => expect(screen.queryByTestId('remote-error-banner')).toBeNull());
  });

  // The two halves of a remote fail independently, and each event speaks for
  // one of them. A sync_ok that lowered a banner an expired push token had
  // raised would be undone by the very next failing push tick — a banner
  // blinking once per reconcile interval for as long as the token stayed dead,
  // and one that disagrees with the persisted status the poll reads back.
  it('a clean sync does NOT clear a banner raised by a failing push', async () => {
    const es = await mountApp();
    await act(async () => { es.emit('push_error', { error: 'token expired' }); });
    expect(screen.getByTestId('remote-error-banner')).toHaveTextContent('token expired');

    // The fetch half is fine — origin is reachable, it is the push that is
    // rejected — so sync_ok arrives on every tick that pulls anything.
    await act(async () => { es.emit('sync_ok', { main: { mode: 'ff' }, agent: { mode: 'noop' } }); });
    expect(screen.getByTestId('remote-error-banner')).toHaveTextContent('token expired');

    // Only the push recovering takes it down.
    await act(async () => { es.emit('push_ok', {}); });
    expect(screen.queryByTestId('remote-error-banner')).toBeNull();
  });

  it('a clean push does NOT clear a banner raised by a failing sync', async () => {
    const es = await mountApp();
    await act(async () => { es.emit('sync_error', { error: 'no such host' }); });
    await act(async () => { es.emit('push_ok', {}); });
    expect(screen.getByTestId('remote-error-banner')).toHaveTextContent('no such host');
  });

  it('keeps rechecking while only the PUSH half is failing', async () => {
    const api = await apiMock();
    const es = await mountApp();
    vi.useFakeTimers();
    try {
      await act(async () => { es.emit('push_error', { error: 'token expired' }); });
      expect(screen.getByTestId('remote-error-banner')).toBeTruthy();

      // The recheck is gated on "either side is failing", not on the sync side
      // alone, or a stale push banner would never be re-read.
      api.getOrigin.mockResolvedValue(ORIGIN_OK);
      await act(async () => { vi.advanceTimersByTime(70_000); });
      expect(screen.queryByTestId('remote-error-banner')).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('leaves the banner alone when the origin read itself fails', async () => {
    const api = await apiMock();
    const es = await mountApp();
    act(() => { es.emit('sync_error', { error: 'auth failed' }); });

    // A failed read teaches us nothing about the remote — clearing here would
    // hide a real failure behind an unrelated network hiccup.
    api.getOrigin.mockRejectedValue(new Error('offline'));
    es.readyState = FakeEventSource.CONNECTING;
    await act(async () => { es.emit('error'); });
    await act(async () => { es.emit('open'); });

    expect(screen.getByTestId('remote-error-banner')).toHaveTextContent('auth failed');
  });

  it('clears the banner when the repo has no origin at all', async () => {
    const api = await apiMock();
    const es = await mountApp();
    act(() => { es.emit('sync_error', { error: 'auth failed' }); });

    // A disconnected repo cannot be out of sync, so a leftover banner is stale.
    api.getOrigin.mockResolvedValue(null);
    es.readyState = FakeEventSource.CONNECTING;
    await act(async () => { es.emit('error'); });
    await act(async () => { es.emit('open'); });

    await waitFor(() => expect(screen.queryByTestId('remote-error-banner')).toBeNull());
  });

  // Every other way down is edge-triggered — a remote event, a reconnect, a
  // repo switch, a manager close. A stream that stalls SILENTLY fires none of
  // them (no 'error', no 'open'), and sync/push events have no reconnect replay
  // at all, so without a poll the banner could still outlive its failure.
  it('re-checks the stored status on a timer while the banner is up', async () => {
    const api = await apiMock();
    const es = await mountApp();
    // Fake timers go in BEFORE the banner is raised: the recheck interval is
    // armed by the effect that reacts to it, and a real-timer interval would
    // never see advanceTimersByTime.
    vi.useFakeTimers();
    try {
      await act(async () => { es.emit('sync_error', { error: 'auth failed' }); });
      expect(screen.getByTestId('remote-error-banner')).toBeTruthy();

      api.getOrigin.mockResolvedValue(ORIGIN_OK);
      await act(async () => { vi.advanceTimersByTime(70_000); });
      expect(screen.queryByTestId('remote-error-banner')).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('does NOT poll while the banner is down', async () => {
    const api = await apiMock();
    await mountApp();
    await waitFor(() => expect(api.getOrigin.mock.calls.length).toBeGreaterThan(0));
    const callsBefore = api.getOrigin.mock.calls.length;

    vi.useFakeTimers();
    try {
      // A healthy remote costs nothing: the recheck exists only to bound how
      // long a STALE banner can survive, not to poll the origin forever.
      await act(async () => { vi.advanceTimersByTime(600_000); });
      expect(api.getOrigin.mock.calls.length).toBe(callsBefore);
    } finally {
      vi.useRealTimers();
    }
  });

  it('the dismiss button clears the banner', async () => {
    const es = await mountApp();
    act(() => { es.emit('sync_error', { error: 'auth failed' }); });

    await act(async () => { fireEvent.click(screen.getByTestId('remote-error-dismiss')); });
    expect(screen.queryByTestId('remote-error-banner')).toBeNull();
  });

  it('Reconnect… clears the banner and opens the repo manager', async () => {
    const es = await mountApp();
    act(() => { es.emit('sync_error', { error: 'auth failed' }); });

    await act(async () => { fireEvent.click(screen.getByTestId('remote-error-reconnect')); });

    expect(screen.getByRole('dialog', { name: 'Repo Manager' })).toBeTruthy(); // the manager is up
    expect(screen.queryByTestId('remote-error-banner')).toBeNull();
  });

  it('re-reads the stored status when the repo manager closes', async () => {
    const api = await apiMock();
    api.getOrigin.mockResolvedValue(ORIGIN_FAILING);
    await mountApp();
    await waitFor(() => expect(screen.queryByTestId('remote-error-banner')).toBeTruthy());

    // Open the manager, "fix" the remote there, close it: the banner must
    // reflect the repaired connection without waiting for a reconcile tick.
    await act(async () => { fireEvent.click(screen.getByTestId('remote-error-reconnect')); });
    api.getOrigin.mockResolvedValue(ORIGIN_OK);
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Close' })); });

    await waitFor(() => expect(screen.queryByTestId('remote-error-banner')).toBeNull());
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
