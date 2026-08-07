import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';

// Zero repos is an ordinary state, not an error: a fresh install creates none,
// and the last repo can be archived. The empty screen must therefore be a
// STARTING POINT — the top bar that normally opens the repo manager is below
// App's early return, so without a way out from here the app is unreachable
// except through the REST API.

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  url: string;
  readyState = FakeEventSource.OPEN;
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
  close() { this.readyState = FakeEventSource.CLOSED; }
}

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

async function primeEmptyApi() {
  const { api } = await import('./api');
  const m = api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  m.repos.mockResolvedValue([]);
  m.listLenses.mockResolvedValue([]);
  m.listArchived.mockResolvedValue([]);
  m.getAgentBranch.mockResolvedValue('machine/test');
  m.status.mockResolvedValue(null);
  m.getOrigin.mockResolvedValue(null);
  m.browse.mockResolvedValue({ path: 'kb', children: [] });
  m.recent.mockResolvedValue({ facts: [], total: 0 });
  m.search.mockResolvedValue({ results: [] });
  m.stats.mockResolvedValue(null);
  m.activity.mockResolvedValue(null);
  m.completions.mockResolvedValue([]);
}

beforeEach(async () => {
  FakeEventSource.instances = [];
  vi.clearAllMocks();
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
  await primeEmptyApi();
});

afterEach(() => {
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
});

describe('the zero-repo screen', () => {
  it('offers a way to create the first repo', async () => {
    const App = (await import('./App')).default;
    render(<App />);

    await screen.findByTestId('no-repos');
    // The old copy pointed at `knomit init`, a subcommand that does not exist.
    expect(screen.queryByText(/knomit init/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId('no-repos-create'));

    // The manager opens straight onto the create form: with no repos there is
    // no detail pane to land on.
    await waitFor(() => expect(screen.getByTestId('create-name')).toBeInTheDocument());
  });
});
