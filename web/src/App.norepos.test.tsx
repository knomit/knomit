import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, within, act, fireEvent, cleanup } from '@testing-library/react';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { __resetRepoCreatesForTest } from './useRepoCreates';

// Zero repos is an ordinary state, not an error: a fresh install creates none,
// and the last repo can be archived. The empty screen must therefore be a
// STARTING POINT. It is the ordinary shell with the repository DRAFT as its
// target: the rail is forced open with the draft as its only Repositories row,
// the body is the create wizard, there is no Cancel (there is nothing to go
// back to), and the gear still reaches the server pages.

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    fetchVersion: vi.fn(),
    api: {
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), listRepoCreates: vi.fn(),
      listOAuthPending: vi.fn(), getFleet: vi.fn(), listFleetMembers: vi.fn(),
    },
  };
});

async function primeEmptyApi({ readOnly = false } = {}) {
  const mod = await import('./api');
  const m = mod.api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  vi.mocked(mod.fetchVersion).mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly } as Awaited<ReturnType<typeof mod.fetchVersion>>);
  m.repos.mockResolvedValue([]);
  m.listLenses.mockResolvedValue([]);
  m.listArchived.mockResolvedValue([]);
  m.listRepoCreates.mockResolvedValue([]);
  m.listOAuthPending.mockResolvedValue(null);
  m.getFleet.mockRejectedValue(new Error('not in a fleet'));
  m.listFleetMembers.mockResolvedValue([]);
  m.getAgentBranch.mockResolvedValue('machine/test');
  m.status.mockResolvedValue(null);
  m.getOrigin.mockResolvedValue(null);
  return m;
}

beforeEach(() => {
  localStorage.clear();
  __resetRepoCreatesForTest();
  installFakeEventSource();
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  // cleanup FIRST: afterEach runs in stack order (see uninstallFakeEventSource).
  cleanup();
  vi.restoreAllMocks();
  uninstallFakeEventSource();
  vi.clearAllMocks();
});

async function mountApp() {
  const App = (await import('./App')).default;
  render(<App />);
  await screen.findByTestId('no-repos');
}

describe('the zero-repo shell', () => {
  it('is the repository draft: the wizard is the body, labelled "New repository", with no tabs', async () => {
    await primeEmptyApi();
    await mountApp();
    // `step-source` is the wizard's first screen: the create surface rendered.
    expect(await screen.findByTestId('step-source')).toBeInTheDocument();
    expect(screen.getByTestId('draft-repo')).toBeVisible();
    expect(screen.getByTestId('target-label')).toHaveTextContent('New repository');
    expect(screen.queryAllByRole('tab')).toHaveLength(0);
    // Not the boot screen: there is nothing to open, and that is the answer.
    expect(screen.queryByTestId('boot-screen')).toBeNull();
    // The old copy pointed at `knomit init`, a subcommand that does not exist.
    expect(screen.queryByText(/knomit init/)).not.toBeInTheDocument();
  });

  it('forces the rail open with the draft as the only Repositories row', async () => {
    await primeEmptyApi();
    await mountApp();
    // Nothing persisted said open: the rail is forced.
    expect(localStorage.getItem('knomit.rail')).not.toBe('1');
    const section = await screen.findByTestId('rail-repos');
    const rows = within(section).getAllByRole('button').filter(b => b.dataset.testid !== 'rail-new-repo');
    expect(rows.map(r => r.dataset.testid)).toEqual(['rail-draft-repo']);
    expect(rows[0]).toHaveAttribute('aria-current', 'true');
  });

  it('offers no Cancel, because there is nothing to go back to', async () => {
    await primeEmptyApi();
    await mountApp();
    await screen.findByTestId('step-source');
    // Control: the wizard's own footer is there (its Next lives beside where
    // Cancel would be once a local source is chosen).
    fireEvent.click(screen.getByTestId('choose-local'));
    expect(screen.getByRole('button', { name: 'Next' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
  });

  it('still reaches the server pages through the gear, and comes back to the draft', async () => {
    await primeEmptyApi();
    await mountApp();
    await screen.findByTestId('step-source');
    await act(async () => { fireEvent.click(screen.getByTestId('toknomitr-manage-btn')); });
    expect(await screen.findByTestId('manage-surface')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-fleet')).toBeInTheDocument();
    await act(async () => { fireEvent.click(screen.getByTestId('toknomitr-manage-btn')); });
    await waitFor(() => expect(screen.queryByTestId('manage-surface')).toBeNull());
    expect(screen.getByTestId('step-source')).toBeInTheDocument();
  });

  // A read-only instance cannot create, so the draft says so instead of
  // offering a form whose submit would be refused.
  it('explains itself on a read-only instance instead of offering a form it cannot submit', async () => {
    await primeEmptyApi({ readOnly: true });
    await mountApp();
    expect(await screen.findByTestId('create-blocked-repository')).toBeInTheDocument();
    expect(screen.queryByTestId('step-source')).toBeNull();
    expect(screen.getByTestId('rail-new-repo')).toBeDisabled();
  });
});
