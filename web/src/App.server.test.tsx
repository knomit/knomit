import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { __resetRepoCreatesForTest } from './useRepoCreates';
import type { OAuthPending } from './api';

// The server pages: Fleet, Logs and Authorizations behind the gear, inside the
// same three-level shell as every target. The gear is lit while they show and
// takes you back to the target and facet you left; a rail pick also leaves
// them; Logs and Authorizations are omitted where they cannot work; and the
// browse keys, Escape included, do nothing here.

const STATUS = {
  head: 'abc1234', branch: 'machine/test', embeddings_enabled: false, ontology_root: 'kb',
  index_state: 'ready', index_done: 0, index_total: 0, index_percent: 100,
};
const POLICY = { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 };

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    fetchVersion: vi.fn(),
    api: {
      listClientSessions: vi.fn(),
      listOAuthPending: vi.fn(),
      getFleet: vi.fn(), listFleetMembers: vi.fn(),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(),
      listRepoCreates: vi.fn(), listExperiments: vi.fn(), listPushedBranches: vi.fn(),
      // The repository draft's wizard.
      ontologyPresets: vi.fn(), ontologyPresetYAML: vi.fn(), validateOntology: vi.fn(), ontologySchema: vi.fn(), listTemplates: vi.fn(),
    },
  };
});

const pending = (id: string, name: string): OAuthPending => ({
  id, client_id: `https://${name}.example/meta`, client_name: name,
  redirect_uri: 'http://127.0.0.1:1/callback', scopes: ['read'], resource: 'http://localhost:1',
  remote_addr: '127.0.0.1:2', user_agent: 'ua',
  created_at: '2026-09-24T12:00:00Z', expires_at: '2099-09-24T12:10:00Z',
});

async function primeApi({ readOnly = false, oauth = [] as OAuthPending[] | null } = {}) {
  const mod = await import('./api');
  const m = mod.api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  vi.mocked(mod.fetchVersion).mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly } as Awaited<ReturnType<typeof mod.fetchVersion>>);
  m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }]);
  m.listLenses.mockResolvedValue([]);
  m.listArchived.mockResolvedValue([]);
  m.getAgentBranch.mockResolvedValue('machine/test');
  m.status.mockResolvedValue(STATUS);
  m.getOrigin.mockResolvedValue(null);
  m.browse.mockResolvedValue({ path: 'kb', children: [{ name: 'foo.md', is_dir: false, title: 'Foo', type: 'observation' }] });
  m.recent.mockResolvedValue({ facts: [], total: 0 });
  m.search.mockResolvedValue({ results: [] });
  m.stats.mockResolvedValue(null);
  m.activity.mockResolvedValue(null);
  m.completions.mockResolvedValue([]);
  m.fact.mockResolvedValue({
    path: 'kb/foo.md', title: 'Foo', body: 'body', type: 'observation', confidence: 0.9, sources: 1,
    domain: [], entities: [], refs: [], commit_hash: 'aaa1111', commit_date: '2026-05-01T00:00:00Z',
  });
  m.factCommits.mockResolvedValue({ entries: [] });
  m.explain.mockResolvedValue({ incoming: [], outgoing: [] });
  m.commitDetail.mockResolvedValue(null);
  m.getRepo.mockImplementation(async (name: string) => ({ name, description: '' }));
  m.listBranchNames.mockResolvedValue(['main', 'machine/test']);
  m.listRepoCreates.mockResolvedValue([]);
  m.listExperiments.mockResolvedValue({ experiments: [] });
  m.listPushedBranches.mockResolvedValue([]);
  m.listClientSessions.mockResolvedValue({ truncated: false, policy: POLICY, sessions: [] });
  m.listOAuthPending.mockResolvedValue(oauth);
  m.getFleet.mockResolvedValue({ state: 'standalone', agent_id: 'agent-7f3a' });
  m.listFleetMembers.mockResolvedValue([]);
  m.ontologyPresets.mockResolvedValue([{ name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] }]);
  m.ontologyPresetYAML.mockResolvedValue('id: general\nname: General\ntopics:\n  people:\n');
  m.validateOntology.mockResolvedValue({ ok: true, id: 'general', name: 'General', topics: ['people'], rule_count: 0 });
  m.ontologySchema.mockResolvedValue([]);
  m.listTemplates.mockResolvedValue([]);
  return m;
}

async function mountApp() {
  render(<App />);
  await screen.findByTestId('status-footer');
  await screen.findByTestId('target-label');
}

const gear = () => screen.getByTestId('server-toggle');
const tabNames = () => screen.getAllByRole('tab').map(t => t.firstChild?.textContent ?? '');
async function openServer() {
  await act(async () => { fireEvent.click(gear()); });
  await screen.findByTestId('fleet-page');
}

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem('knomit.context', JSON.stringify({ kind: 'repo', repo: 'ops-kb' }));
  __resetRepoCreatesForTest();
  installFakeEventSource();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  vi.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => {
  // cleanup FIRST: afterEach runs in stack order (see test-setup.ts).
  cleanup();
  vi.restoreAllMocks();
  uninstallFakeEventSource();
  vi.clearAllMocks();
});

describe('the gear opens the server pages', () => {
  it('labels the bar "Server" with this host, and has Fleet, Logs and Authorizations when there is an issuer', async () => {
    await primeApi({ oauth: [] });
    await mountApp();
    // Control: on a target the label names it.
    expect(screen.getByTestId('target-label')).toHaveTextContent('ops-kb');
    await openServer();

    const label = screen.getByTestId('target-label');
    expect(label).toHaveTextContent(/^Server/);
    expect(label).not.toHaveTextContent('ops-kb');
    expect(screen.getByTestId('server-host')).toHaveTextContent(/^localhost:3000$/);
    await waitFor(() => expect(tabNames()).toEqual(['Fleet', 'Logs', 'Authorizations']));
    expect(screen.getByRole('tablist', { name: 'Server pages' })).toBeInTheDocument();
    expect(screen.queryByTestId('facet-tabs')).toBeNull();
    expect(screen.getByTestId('server-tab-fleet')).toHaveAttribute('aria-selected', 'true');
  });

  it('has only Fleet on a read-only server, and never probes for an issuer', async () => {
    localStorage.setItem('knomit.rail', '1');
    const m = await primeApi({ readOnly: true, oauth: [] });
    await mountApp();
    // The read-only flag lands after boot; wait until it is observable (the
    // rail's `+` refuses) so the assertions cannot pass by running first.
    await waitFor(() => expect(screen.getByTestId('rail-new-repo')).toBeDisabled());
    await openServer();
    await waitFor(() => expect(tabNames()).toEqual(['Fleet']));
    // Give a probe every chance to have run.
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(tabNames()).toEqual(['Fleet']);
    expect(m.listOAuthPending).not.toHaveBeenCalled();
  });

  it('has no Authorizations tab on a server with no OAuth issuer', async () => {
    const m = await primeApi({ oauth: null });
    await mountApp();
    await openServer();
    await waitFor(() => expect(m.listOAuthPending).toHaveBeenCalled());
    await act(async () => { await Promise.resolve(); });
    expect(tabNames()).toEqual(['Fleet', 'Logs']);
  });

  it('badges Authorizations with the waiting count, and shows no badge at zero', async () => {
    const m = await primeApi({ oauth: [pending('p1', 'alpha'), pending('p2', 'beta')] });
    await mountApp();
    await openServer();
    expect(await screen.findByTestId('server-tab-auth-badge')).toHaveTextContent(/^2$/);

    // Zero waiting: the tab stays, the badge goes.
    cleanup();
    m.listOAuthPending.mockResolvedValue([]);
    await mountApp();
    await openServer();
    expect(await screen.findByTestId('server-tab-auth')).toBeInTheDocument();
    expect(screen.queryByTestId('server-tab-auth-badge')).toBeNull();
  });

  it('opens each page in the body: Fleet and Authorizations as indexed pages, Logs with its toolbar', async () => {
    await primeApi({ oauth: [pending('p1', 'alpha')] });
    await mountApp();
    await openServer();
    expect(within(screen.getByTestId('fleet-page')).getByTestId('settings-index-heading')).toHaveTextContent('Fleet');

    fireEvent.click(await screen.findByTestId('server-tab-logs'));
    expect(await screen.findByTestId('logs-facet')).toBeInTheDocument();
    expect(screen.getByTestId('facet-toolbar')).toBeInTheDocument();
    expect(screen.queryByTestId('fleet-page')).toBeNull();

    fireEvent.click(screen.getByTestId('server-tab-auth'));
    const auth = await screen.findByTestId('auth-page');
    expect(within(auth).getByTestId('settings-index-heading')).toHaveTextContent('Authorizations');
    expect(await within(auth).findByTestId('toc-request-p1')).toHaveTextContent('alpha');
  });
});

describe('the gear is lit while they show, and takes you back', () => {
  it('returns to the previous target AND facet', async () => {
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    await screen.findByTestId('activity-facet');
    expect(gear()).toHaveAttribute('aria-pressed', 'false');

    await openServer();
    expect(gear()).toHaveAttribute('aria-pressed', 'true');
    expect(screen.queryByTestId('activity-facet')).toBeNull();

    await act(async () => { fireEvent.click(gear()); });
    expect(await screen.findByTestId('activity-facet')).toBeInTheDocument();
    expect(gear()).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByTestId('target-label')).toHaveTextContent('ops-kb');
    expect(screen.getByTestId('facet-tab-activity')).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByTestId('fleet-page')).toBeNull();
  });

  it('a rail pick while on Logs leaves them for the picked target, on its facet', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    await screen.findByTestId('repo-settings');
    await openServer();
    // The rail stays beside the server pages, with no row selected: none of
    // them is what the body shows.
    expect(screen.getByTestId('target-rail')).toBeInTheDocument();
    expect(screen.getByTestId('rail-repo-ops-kb')).not.toHaveAttribute('aria-current');
    fireEvent.click(await screen.findByTestId('server-tab-logs'));
    await screen.findByTestId('logs-facet');

    fireEvent.click(screen.getByTestId('rail-repo-core-kb'));
    await waitFor(() => expect(screen.getByTestId('target-label')).toHaveTextContent('core-kb'));
    expect(screen.queryByTestId('logs-facet')).toBeNull();
    expect(gear()).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByTestId('facet-tab-settings')).toHaveAttribute('aria-selected', 'true');
    expect(await screen.findByTestId('repo-settings')).toBeInTheDocument();
  });

  it('a pick of the target already under them also leaves them', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    await openServer();
    fireEvent.click(screen.getByTestId('rail-repo-ops-kb'));
    await waitFor(() => expect(screen.queryByTestId('fleet-page')).toBeNull());
    expect(screen.getByTestId('target-label')).toHaveTextContent('ops-kb');
    expect(screen.getByTestId('facet-tab-browse')).toHaveAttribute('aria-selected', 'true');
  });
});

describe('the keyboard on a server page', () => {
  // The fixture is a fact open on Browse: Escape there clears it (CLEAR_FILTERS
  // drops factPath), so a handler that still answered under the server pages
  // would change what the reader comes back to.
  async function openFact() {
    fireEvent.click(await screen.findByTestId('dir-entry'));
    await screen.findByTestId('retract-btn');
  }

  it('Escape on Browse clears the open fact (control for the fixture)', async () => {
    await primeApi();
    await mountApp();
    await openFact();
    act(() => { fireEvent.keyDown(window, { key: 'Escape' }); });
    await waitFor(() => expect(screen.queryByTestId('retract-btn')).toBeNull());
  });

  it('Escape does nothing: the server pages stay, and the fact is still open behind them', async () => {
    await primeApi();
    await mountApp();
    await openFact();
    await openServer();

    act(() => { fireEvent.keyDown(window, { key: 'Escape' }); });
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByTestId('fleet-page')).toBeInTheDocument();
    expect(gear()).toHaveAttribute('aria-pressed', 'true');

    await act(async () => { fireEvent.click(gear()); });
    expect(await screen.findByTestId('retract-btn')).toBeInTheDocument();
  });

  it('Backspace on Browse goes back off the open fact (control for the fixture)', async () => {
    await primeApi();
    await mountApp();
    await openFact();
    act(() => { fireEvent.keyDown(window, { key: 'Backspace' }); });
    await waitFor(() => expect(screen.queryByTestId('retract-btn')).toBeNull());
  });

  it('Backspace does not go back behind them', async () => {
    await primeApi();
    await mountApp();
    await openFact();
    await openServer();
    act(() => { fireEvent.keyDown(window, { key: 'Backspace' }); });
    await act(async () => { await Promise.resolve(); });
    await act(async () => { fireEvent.click(gear()); });
    expect(await screen.findByTestId('retract-btn')).toBeInTheDocument();
  });
});

// A draft is a form someone is half-way through. Looking at the server pages
// must not cost them what they typed: the draft stays mounted, hidden, under
// the pages, and is there as they left it when the gear brings them back.
describe('a draft under the server pages', () => {
  it('keeps what was typed into the repository draft across a visit to the server pages', async () => {
    localStorage.setItem('knomit.rail', '1');
    await primeApi();
    await mountApp();
    fireEvent.click(screen.getByTestId('rail-new-repo'));
    await screen.findByTestId('step-source');
    fireEvent.click(screen.getByTestId('choose-remote'));
    const url = await screen.findByTestId('create-url') as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://example.com/half-typed.git' } });
    expect(url).toBeVisible();

    await openServer();
    // Control: hidden while the server pages show — but the same node, still
    // mounted, still holding the text.
    expect(url).not.toBeVisible();
    expect(screen.getByTestId('create-url')).toBe(url);

    await act(async () => { fireEvent.click(gear()); });
    await waitFor(() => expect(screen.queryByTestId('server-pages')).toBeNull());
    const back = screen.getByTestId('create-url') as HTMLInputElement;
    expect(back).toBeVisible();
    expect(back.value).toBe('https://example.com/half-typed.git');
    expect(screen.getByTestId('target-label')).toHaveTextContent('New repository');
  });
});
