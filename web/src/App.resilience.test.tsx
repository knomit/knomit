import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';

// P1.7 evidence + resilience tests for the App shell:
//   1. a diagnostic line costs ZERO panel renders;
//   2. App-local state churn (a splitter drag) no longer re-renders the panels;
//   3. one crashing panel leaves the rest of the app mounted and usable;
//   4. the SSE outage warning re-arms after a reconnect.
//
// (1) and (2) are measured, not asserted by inspection: './Library' is mocked
// with a counting pass-through. Library sits UNDER the memoized LeftPanel, so
// the counter only advances when LeftPanel actually re-renders — remove the
// memo and these fail.
//
// (1) was originally the evidence for moving a console ring buffer out of
// AppState: a log line had to re-render the console panel and nothing else. The
// panel is gone and these lines now go to the browser console (App's `diag`), so
// the render cost is structurally zero — but the assertion is kept, because the
// thing it actually guards is that an SSE error event does not smuggle an
// AppState change along with its log line.

const counts = vi.hoisted(() => ({ library: 0, appBody: 0 }));
const crash = vi.hoisted(() => ({ rightPanel: false }));

vi.mock('./Library', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./Library')>();
  return {
    Library: (props: React.ComponentProps<typeof actual.Library>) => {
      counts.library += 1;
      return <actual.Library {...props} />;
    },
  };
});

// NOTE ON WHAT THIS COUNTER MEASURES. The mock is an UNMEMOIZED pass-through,
// so it re-renders whenever AppBody re-renders and hands the real (memoized)
// RightPanel its props — `counts.appBody` therefore measures AppBody renders in
// the RightPanel slot, NOT RightPanel's own memo. That is exactly the right
// instrument for the two assertions below (a log line / a splitter drag must not
// re-render the app body at all), but it is not memo evidence: RightPanel's memo
// is covered by RightPanel.memo.test.tsx. The mock also injects the crash used
// by the error-boundary test further down.
vi.mock('./RightPanel', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./RightPanel')>();
  return {
    RightPanel: (props: React.ComponentProps<typeof actual.RightPanel>) => {
      counts.appBody += 1;
      if (crash.rightPanel) throw new Error('right panel exploded');
      return <actual.RightPanel {...props} />;
    },
  };
});

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  url: string;
  readyState = FakeEventSource.OPEN;
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
  removeEventListener(type: string, fn: (e: unknown) => void) { this.listeners.get(type)?.delete(fn); }
  close() { this.closedByClient = true; this.readyState = FakeEventSource.CLOSED; }

  emit(type: string, data?: unknown) {
    if (this.closedByClient) return;
    const payload = { type, data: data === undefined ? '' : JSON.stringify(data) };
    for (const fn of [...(this.listeners.get(type) ?? [])]) fn(payload);
  }
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
    api: {
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(),
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
}

async function mountApp(): Promise<FakeEventSource> {
  const App = (await import('./App')).default;
  render(<App />);
  await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
  await screen.findByTestId('status-footer');
  // Let the mount-time effect cascade settle so the counters start from a
  // quiescent tree rather than mid-bootstrap.
  await act(async () => { await Promise.resolve(); });
  return FakeEventSource.instances[0];
}

let errorSpy: ReturnType<typeof vi.spyOn>;

// The diagnostics App emits, read off the console method `diag` routes them to.
function diagLines(): string[] {
  return errorSpy.mock.calls.map((c: unknown[]) => String(c[0]));
}

beforeEach(async () => {
  FakeEventSource.instances = [];
  counts.library = 0;
  counts.appBody = 0;
  crash.rightPanel = false;
  vi.clearAllMocks();
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
  // Silenced, not passed through: these tests provoke the error paths on
  // purpose, and React's own boundary logging rides the same channel.
  errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
  await primeApi();
});

afterEach(() => {
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
  errorSpy.mockRestore();
});

describe('P1.7 — diagnostic logging does not re-render the app', () => {
  it('a burst of diagnostic lines is emitted without re-rendering the panels', async () => {
    const es = await mountApp();
    const libraryBefore = counts.library;
    const appBodyBefore = counts.appBody;

    // An 'error' event produces a diagnostic and nothing else — the cleanest
    // isolation of "a log line" from any AppState change.
    await act(async () => {
      es.readyState = FakeEventSource.CONNECTING;
      es.emit('error');
      es.emit('open');       // re-arms, so the next error logs again
      es.emit('error');
      es.emit('open');
      es.emit('error');
    });

    // The lines were emitted — they were not merely dropped…
    expect(diagLines().filter(l => l.includes('[events] connection lost')).length).toBe(3);
    // …and cost zero panel renders. Before P1.7 each line minted a new AppState
    // and re-rendered the entire tree; a line that went back through dispatch
    // would do so again.
    expect(counts.library).toBe(libraryBefore);
    expect(counts.appBody).toBe(appBodyBefore);

    // Control: the counters are live — a real AppState change still re-renders.
    await act(async () => { es.emit('status', { head: 'ddddddd4444' }); });
    expect(counts.library).toBeGreaterThan(libraryBefore);
  });

  it('dragging the splitter does not re-render the library', async () => {
    const es = await mountApp();
    const before = counts.library;

    const splitter = screen.getByTestId('library-splitter');
    fireEvent.mouseDown(splitter, { clientX: 400 });
    // One act() per frame, so React commits each move separately instead of
    // batching all 40 into a single render — this models the real drag.
    for (let x = 401; x <= 440; x++) {
      await act(async () => {
        document.dispatchEvent(new MouseEvent('mousemove', { clientX: x }));
      });
    }
    await act(async () => { document.dispatchEvent(new MouseEvent('mouseup')); });

    // leftPanelWidth is App-local state: it re-renders App on every frame, but
    // the panels' props never move, so React.memo absorbs all 40 commits.
    // Without the memo this is 40 extra Library renders (and, before the row
    // extraction, 40 × N row renders under it).
    expect(counts.library).toBe(before);

    // Control: the counter is live. A real AppState change (a new HEAD) still
    // re-renders the library, so the zero above is memoization at work, not a
    // dead instrument.
    await act(async () => { es.emit('status', { head: 'ddddddd4444' }); });
    expect(counts.library).toBeGreaterThan(before);
  });
});

describe('P1.7 — panel error boundaries', () => {
  it('a crashing right panel is contained; the rest of the app stays mounted', async () => {
    const err = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      crash.rightPanel = true;
      await mountApp();

      // The failed pane shows its inline fallback…
      const fallback = await screen.findByTestId('panel-error');
      expect(fallback).toHaveTextContent('right panel exploded');
      // …contained, not a full-viewport overlay (that is the repo manager's
      // treatment, and would black out the app for one bad fact body).
      //
      // Asserted on the fallback ROOT. The previous version called
      // `fallback.querySelector('[style*="position: fixed"]')`, which searches
      // DESCENDANTS — and in the overlay variant the fixed element IS the root,
      // so it could never have matched. The check was true by construction, and
      // doubly so because `data-testid="panel-error"` only exists on the inline
      // variant in the first place.
      expect(fallback.style.position).not.toBe('fixed');
      expect(fallback.style.inset).toBe('');
      expect(fallback.style.zIndex).toBe('');
      // And it does not claim growth in the column it landed in (Fix 1).
      expect(fallback.style.flexGrow).toBe('');
      expect(screen.queryByRole('alertdialog')).toBeNull();

      // Everything around it is still there and interactive.
      expect(screen.getByTestId('status-footer')).toBeTruthy();
      expect(screen.getByTestId('left-panel')).toBeTruthy();
      expect(screen.getByTestId('library-splitter')).toBeTruthy();
    } finally {
      err.mockRestore();
    }
  });
});

describe('P1.7 — SSE outage warning re-arms on reconnect', () => {
  it('logs a second outage after the stream recovers', async () => {
    const es = await mountApp();

    es.readyState = FakeEventSource.CONNECTING;
    act(() => { es.emit('error'); es.emit('error'); });
    expect(diagLines().filter(l => l.includes('[events] connection lost')).length).toBe(1);

    // A successful open ends the outage; the NEXT one must be reportable again.
    // Before this fix `loggedDisconnect` was never cleared, so "once per outage"
    // was really once per subscription lifetime and this second warning was lost.
    act(() => { es.emit('open'); });
    act(() => { es.emit('error'); });
    expect(diagLines().filter(l => l.includes('[events] connection lost')).length).toBe(2);
  });
});
