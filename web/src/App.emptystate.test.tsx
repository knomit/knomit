import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';

// Zero repos is a first-class state: knomit has no default repo, so a fresh
// install serves an empty repo collection until the user creates one. The empty
// state must therefore be a way IN — it used to be a dead end that told the
// user to run a command (`knomit init`) that does not exist, with the repo
// manager unmounted behind it so there was no way to create anything at all.

class SilentEventSource {
  url: string;
  constructor(url: string) { this.url = url; }
  addEventListener() {}
  removeEventListener() {}
  close() {}
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
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(),
    },
  };
});

beforeEach(async () => {
  vi.clearAllMocks();
  (globalThis as unknown as { EventSource: unknown }).EventSource = SilentEventSource;
  const { api } = await import('./api');
  const m = api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  m.repos.mockResolvedValue([]); // the server has no repos
  m.listLenses.mockResolvedValue([]);
  m.listArchived.mockResolvedValue([]);
  m.getOrigin.mockResolvedValue(null);
});

afterEach(() => {
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
});

describe('zero repos', () => {
  it('renders an empty state that leads to creating a repo', async () => {
    const App = (await import('./App')).default;
    render(<App />);

    const empty = await screen.findByTestId('no-repos');
    // It must not send the user to a command line — least of all a subcommand
    // that was never registered.
    expect(empty.textContent).not.toMatch(/knomit init/);

    // The affordance opens the manager, which lands on its create form because
    // there is no current repo to select.
    fireEvent.click(screen.getByTestId('no-repos-create'));
    await waitFor(() => expect(screen.getByLabelText('Repo Manager')).toBeInTheDocument());
    expect(screen.getByTestId('create-name')).toBeInTheDocument();
  });

  // Cancelling the create form must go back to the same place opening the
  // dialog lands — NOT to a detail pane for the empty repo name, which is a
  // non-null selection that slips past the manager's fallback and asks the
  // server about a repo called "".
  it('cancelling the create form does not fall back to an empty repo name', async () => {
    const { api } = await import('./api');
    const App = (await import('./App')).default;
    render(<App />);

    fireEvent.click(await screen.findByTestId('no-repos-create'));
    await screen.findByTestId('create-name');
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    // Still the create form, and nothing was asked about repo "".
    expect(await screen.findByTestId('create-name')).toBeInTheDocument();
    expect(api.getOrigin).not.toHaveBeenCalled();
    expect(api.getRepo).not.toHaveBeenCalled();
  });

  it('never asks the server about an empty repo name', async () => {
    const { api } = await import('./api');
    const App = (await import('./App')).default;
    render(<App />);
    await screen.findByTestId('no-repos');
    expect(api.getOrigin).not.toHaveBeenCalled();
  });
});
