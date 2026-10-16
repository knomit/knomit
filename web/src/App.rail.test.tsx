import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource, latestStream } from './testEventSource';
import { __resetRepoCreatesForTest } from './useRepoCreates';

// The target rail: the left column that replaced the top bar's repo/lens
// dropdown. It PUSHES the content (it is a column of the body row, not an
// overlay), it is ABSENT from the DOM when collapsed, and its open state is
// remembered under knomit.rail. ⌘B toggles it, ⌘K opens it and focuses its
// search, and Escape never closes it.

const STATUS = {
  head: 'abc1234', branch: 'machine/test', embeddings_enabled: false, ontology_root: 'kb',
  index_state: 'ready', index_done: 0, index_total: 0, index_percent: 100,
};

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    fetchVersion: vi.fn().mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly: false }),
    api: {
      listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
      listOAuthPending: vi.fn().mockResolvedValue(null),
      getFleet: vi.fn().mockRejectedValue(new Error('not in a fleet')), listFleetMembers: vi.fn().mockResolvedValue([]),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(),
      listRepoCreates: vi.fn(), listExperiments: vi.fn(),
    },
  };
});

async function primeApi() {
  const { api } = await import('./api');
  const m = api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  m.repos.mockResolvedValue([{ name: 'alpha', uid: 'u-a' }, { name: 'beta', uid: 'u-b' }]);
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
  m.listRepoCreates.mockResolvedValue([]);
  m.listExperiments.mockResolvedValue({ experiments: [] });
  return m;
}

async function mountApp() {
  render(<App />);
  await screen.findByTestId('status-footer');
  await screen.findByTestId('target-label');
}

const pressMod = (key: string) => {
  act(() => { fireEvent.keyDown(window, { key, metaKey: true }); });
};

beforeEach(() => {
  localStorage.clear();
  __resetRepoCreatesForTest();
  installFakeEventSource();
});
afterEach(() => {
  cleanup();
  uninstallFakeEventSource();
  vi.clearAllMocks();
});

describe('the target rail in the app', () => {
  it('is collapsed by default, and absent rather than hidden', async () => {
    await primeApi();
    await mountApp();
    // Positive control: the toggle that would open it is there.
    expect(screen.getByTestId('rail-toggle')).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByTestId('target-rail')).toBeNull();
  });

  it('⌘B opens it and remembers that; a second ⌘B removes it from the DOM', async () => {
    await primeApi();
    await mountApp();

    pressMod('b');
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
    await waitFor(() => expect(localStorage.getItem('knomit.rail')).toBe('1'));
    expect(screen.getByTestId('rail-toggle')).toHaveAttribute('aria-pressed', 'true');

    pressMod('b');
    expect(screen.queryByTestId('target-rail')).toBeNull();
    await waitFor(() => expect(localStorage.getItem('knomit.rail')).toBe('0'));
  });

  it('the toggle opens and closes it too', async () => {
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-toggle'));
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('rail-toggle'));
    expect(screen.queryByTestId('target-rail')).toBeNull();
  });

  it('opens on load when knomit.rail says it was open', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
  });

  it('⌘K with the rail closed opens it and focuses its search', async () => {
    await primeApi();
    await mountApp();
    expect(screen.queryByTestId('target-rail')).toBeNull();

    pressMod('k');
    const search = await screen.findByTestId('rail-search');
    await waitFor(() => expect(document.activeElement).toBe(search));
  });

  it('⌘K with the rail already open focuses its search and leaves it open', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    const search = screen.getByTestId('rail-search');
    expect(document.activeElement).not.toBe(search);

    pressMod('k');
    await waitFor(() => expect(document.activeElement).toBe(search));
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
  });

  it('a ⌘K that has been served does not steal focus again when the rail later reopens', async () => {
    await primeApi();
    await mountApp();

    // Control: ⌘K does focus the search.
    pressMod('k');
    await waitFor(() => expect(document.activeElement).toBe(screen.getByTestId('rail-search')));

    // Close it, put focus back on the page, then reopen with the toggle.
    pressMod('b');
    expect(screen.queryByTestId('target-rail')).toBeNull();
    act(() => { (document.activeElement as HTMLElement | null)?.blur?.(); });
    expect(document.activeElement).toBe(document.body);
    fireEvent.click(screen.getByTestId('rail-toggle'));
    const search = await screen.findByTestId('rail-search');
    // One more frame for any effect that would focus.
    await act(async () => { await Promise.resolve(); });
    expect(document.activeElement).not.toBe(search);
    expect(document.activeElement).toBe(document.body);
  });

  it('nor on the way in and out of the server pages', async () => {
    await primeApi();
    await mountApp();
    pressMod('k');
    await waitFor(() => expect(document.activeElement).toBe(screen.getByTestId('rail-search')));
    act(() => { (document.activeElement as HTMLElement).blur(); });

    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    await screen.findByTestId('server-pages');
    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    const search = await screen.findByTestId('rail-search');
    await act(async () => { await Promise.resolve(); });
    expect(document.activeElement).not.toBe(search);
  });

  it('after ⌘K, typing and picking a repo row, the page shortcuts work again', async () => {
    await primeApi();
    await mountApp();
    pressMod('k');
    const search = await screen.findByTestId('rail-search');
    await waitFor(() => expect(document.activeElement).toBe(search));
    fireEvent.change(search, { target: { value: 'bet' } });

    await act(async () => { fireEvent.click(screen.getByTestId('rail-repo-beta')); });
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('beta'));
    // The pick hands the keyboard back to the page. By id, not identity: a
    // repo switch still remounts the shell in this PR (BootScreen), and a
    // remounted search that grabbed focus would be a different element.
    expect(document.activeElement?.id).not.toBe('rail-search');
    await screen.findByTestId('browse-toolbar');
    act(() => { fireEvent.keyDown(window, { key: '/' }); });
    expect(document.activeElement?.id).toBe('filter-input');
  });

  it('after ⌘K, typing and picking a lens row (no remount), the page shortcuts work again', async () => {
    const api = await primeApi();
    const dev = {
      name: 'dev', uid: 'u-dev',
      write: { uid: 'u-a', name: 'alpha' },
      reads: [{ uid: 'u-a', name: 'alpha' }, { uid: 'u-b', name: 'beta' }],
    };
    api.listLenses.mockResolvedValue([dev]);
    api.getLens.mockResolvedValue(dev);
    await mountApp();
    pressMod('k');
    const search = await screen.findByTestId('rail-search');
    await waitFor(() => expect(document.activeElement).toBe(search));
    fireEvent.change(search, { target: { value: 'de' } });

    await act(async () => { fireEvent.click(await screen.findByTestId('rail-lens-dev')); });
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('dev'));
    // The same input element is still mounted, so identity is the right check.
    expect(screen.getByTestId('rail-search')).toBe(search);
    expect(document.activeElement).not.toBe(search);
    act(() => { fireEvent.keyDown(window, { key: '/' }); });
    expect(document.activeElement?.id).toBe('filter-input');
  });

  it('Escape leaves an open rail open, from the page and from its own search', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();

    act(() => { fireEvent.keyDown(window, { key: 'Escape' }); });
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();

    const search = screen.getByTestId('rail-search');
    act(() => { search.focus(); });
    act(() => { fireEvent.keyDown(search, { key: 'Escape' }); });
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
    expect(localStorage.getItem('knomit.rail')).toBe('1');
  });

  it('picking repo beta in the rail switches the context to it', async () => {
    localStorage.setItem('knomit.rail', '1');
    const api = await primeApi();
    await mountApp();
    expect(screen.getByTestId('target-label').textContent).toBe('alpha');
    expect(api.status.mock.calls.some(c => c[0] === 'beta')).toBe(false);

    await act(async () => { fireEvent.click(screen.getByTestId('rail-repo-beta')); });

    await waitFor(() => expect(api.status.mock.calls.some(c => c[0] === 'beta')).toBe(true));
    await waitFor(() => expect(screen.getByTestId('target-label').textContent).toBe('beta'));
    // Choosing a target does not close the rail: it is a column, not a menu.
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
  });

  it('pushes the content: the rail is the first column of the row the Library sits in', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    const rail = screen.getByTestId('target-rail');
    const column = rail.nextElementSibling as HTMLElement;
    expect(column).not.toBeNull();
    expect(within(column).getByTestId('library-splitter')).toBeInTheDocument();
    expect(rail.parentElement).toHaveStyle({ display: 'flex' });
  });

  it('is hidden over the server pages, the toggle is off, and ⌘B and ⌘K do nothing; it returns when they close', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
    expect(screen.getByTestId('rail-toggle')).toBeEnabled();

    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    await screen.findByTestId('server-pages');
    expect(screen.queryByTestId('target-rail')).toBeNull();
    expect(screen.getByTestId('rail-toggle')).toBeDisabled();
    expect(screen.getByTestId('rail-toggle')).toHaveAttribute('title', 'Repositories, lenses and fleet are hidden over the server pages');
    pressMod('b');
    fireEvent.click(screen.getByTestId('rail-toggle'));
    pressMod('k');
    expect(screen.queryByTestId('target-rail')).toBeNull();
    expect(screen.getByTestId('server-pages')).toBeInTheDocument();
    // The preference is not the shown state: it was not rewritten.
    expect(localStorage.getItem('knomit.rail')).toBe('1');

    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    expect(await screen.findByTestId('target-rail')).toBeInTheDocument();
    expect(screen.getByTestId('rail-toggle')).toBeEnabled();
    expect(screen.getByTestId('rail-toggle')).toHaveAttribute('aria-pressed', 'true');
    // ⌘K over the pages left no focus request behind to fire on the remount.
    expect(document.activeElement).not.toBe(screen.getByTestId('rail-search'));
  });

  it('a closed rail stays closed through the server pages, and the toggle works again after', async () => {
    await primeApi();
    await mountApp();
    expect(screen.queryByTestId('target-rail')).toBeNull();
    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    await screen.findByTestId('server-pages');
    pressMod('b');
    expect(screen.queryByTestId('target-rail')).toBeNull();
    await act(async () => { fireEvent.click(screen.getByTestId('server-toggle')); });
    await waitFor(() => expect(screen.queryByTestId('server-pages')).toBeNull());
    expect(screen.queryByTestId('target-rail')).toBeNull();
    pressMod('b');
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
  });
});

describe('remote health reaches the rail and the toggle', () => {
  it('a remote edge for another repo lights the toggle and that row, and its recovery clears both', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    await waitFor(() => expect(latestStream('/api/v1/repo-events')).toBeDefined());
    // Before: nothing failing anywhere.
    expect(screen.queryByTestId('rail-toggle-dot')).toBeNull();
    expect(screen.queryByTestId('rail-health-beta')).toBeNull();

    act(() => { latestStream('/api/v1/repo-events')!.emit('remote', { repo: 'beta', sync: 'ok', push: 'error' }); });
    await waitFor(() => expect(screen.getByTestId('rail-toggle-dot')).toBeInTheDocument());
    expect(screen.getByTestId('rail-health-beta')).toBeInTheDocument();
    // alpha is current and healthy: only beta's row is marked.
    expect(screen.queryByTestId('rail-health-alpha')).toBeNull();

    act(() => { latestStream('/api/v1/repo-events')!.emit('remote', { repo: 'beta', sync: 'ok', push: 'ok' }); });
    await waitFor(() => expect(screen.queryByTestId('rail-toggle-dot')).toBeNull());
    expect(screen.queryByTestId('rail-health-beta')).toBeNull();
  });

  it('a failing listing for the CURRENT repo alone does not light the toggle', async () => {
    const api = await primeApi();
    api.repos.mockResolvedValue([
      { name: 'alpha', uid: 'u-a', remote: { sync: 'error', push: 'ok' } },
      { name: 'beta', uid: 'u-b', remote: { sync: 'ok', push: 'ok' } },
    ]);
    await mountApp();
    expect(screen.getByTestId('rail-toggle')).toBeInTheDocument();
    expect(screen.queryByTestId('rail-toggle-dot')).toBeNull();
  });
});

describe('the Browse toolbar', () => {
  it('holds the branch picker and the filter, which left the top bar', async () => {
    await primeApi();
    await mountApp();
    const toolbar = screen.getByTestId('browse-toolbar');
    expect(within(toolbar).getByTestId('toknomitr-branch')).toBeInTheDocument();
    expect(toolbar.querySelector('#filter-input')).not.toBeNull();
    // And not in the bar any more.
    expect(within(screen.getByTestId('toknomitr-bar')).queryByTestId('toknomitr-branch')).toBeNull();
  });

  it('is a 40px row in the toolbar tone', async () => {
    await primeApi();
    await mountApp();
    expect(screen.getByTestId('browse-toolbar')).toHaveStyle({
      height: '40px', background: '#0f0f0f', borderBottom: '1px solid #1c1c1c', padding: '0px 12px',
    });
  });

  it('/ still focuses the filter in its new home', async () => {
    await primeApi();
    await mountApp();
    act(() => { fireEvent.keyDown(window, { key: '/' }); });
    expect(document.activeElement?.id).toBe('filter-input');
  });
});
