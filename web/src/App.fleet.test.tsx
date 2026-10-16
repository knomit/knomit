import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import App from './App';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { __resetRepoCreatesForTest } from './useRepoCreates';

// The fleet as a rail target (PR 7, Q1): its row opens Members, Activity and
// Settings in the shell, the crumb carries its chip, and leaving it restores
// the facet the reader had.

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
      getFleet: vi.fn(), listFleetMembers: vi.fn(), listFleetActivity: vi.fn(),
      repos: vi.fn(), listLenses: vi.fn(), listArchived: vi.fn(), getAgentBranch: vi.fn(),
      status: vi.fn(), getOrigin: vi.fn(), getLens: vi.fn(), updateLens: vi.fn(), browse: vi.fn(), recent: vi.fn(),
      search: vi.fn(), stats: vi.fn(), activity: vi.fn(), explain: vi.fn(), completions: vi.fn(),
      fact: vi.fn(), factCommits: vi.fn(), commitDetail: vi.fn(), getRepo: vi.fn(), listBranchNames: vi.fn(),
      listRepoCreates: vi.fn(), listExperiments: vi.fn(), listPushedBranches: vi.fn(),
      // The repository draft's wizard.
      ontologyPresets: vi.fn(), ontologyPresetYAML: vi.fn(), validateOntology: vi.fn(), ontologySchema: vi.fn(), listTemplates: vi.fn(),
    },
  };
});


async function primeApi({ readOnly = false, oauth = [] as unknown[] | null } = {}) {
  const mod = await import('./api');
  const m = mod.api as unknown as Record<string, ReturnType<typeof vi.fn>>;
  vi.mocked(mod.fetchVersion).mockResolvedValue({ version: '0.0.0', commit: 'abc', full: '0.0.0.abc', readOnly } as Awaited<ReturnType<typeof mod.fetchVersion>>);
  m.repos.mockResolvedValue([{ name: 'core-kb', uid: 'u-core' }, { name: 'ops-kb', uid: 'u-ops' }, { name: 'fleet', uid: 'u-fleet' }]);
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
  m.getFleet.mockResolvedValue({
    state: 'registered', agent_id: 'agent-7f3a', fleet_repo: 'fleet', master: true, local: true, hosting: 'none', record_state: 'active',
    host: { listener_on: true, external_addresses: ['https://h.example'] }, fleet_urls: ['https://h.example/git/fleet'], follows_host: true,
  });
  m.listFleetActivity.mockResolvedValue([]);
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

const tabNames = () => within(screen.getByTestId('facet-tabs')).getAllByRole('tab').map(t => t.firstChild?.textContent ?? '');

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem('knomit.context', JSON.stringify({ kind: 'repo', repo: 'ops-kb' }));
  localStorage.setItem('knomit.rail', '1');
  __resetRepoCreatesForTest();
  installFakeEventSource();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  vi.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  uninstallFakeEventSource();
  vi.clearAllMocks();
});

describe('the fleet target', () => {
  it('the rail row opens Members with the crumb and chip, then Activity and Settings; a repo row comes back to Browse', async () => {
    await primeApi();
    await mountApp();
    const row = await screen.findByTestId('rail-fleet-row');
    // The fleet repository is the fleet row, not a repository row.
    expect(screen.queryByTestId('rail-repo-fleet')).toBeNull();
    await act(async () => { fireEvent.click(row); });
    expect(await screen.findByTestId('fleet-members')).toBeInTheDocument();
    expect(screen.getByTestId('target-label')).toHaveTextContent('fleet');
    expect(screen.getByTestId('target-label-chip')).toHaveTextContent('master');
    expect(tabNames()).toEqual(['Members', 'Activity', 'Settings']);
    fireEvent.click(screen.getByTestId('facet-tab-activity'));
    expect(await screen.findByTestId('fleet-activity')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    expect(await screen.findByTestId('fleet-settings')).toBeInTheDocument();
    expect(screen.queryByTestId('repo-settings')).toBeNull();
    await act(async () => { fireEvent.click(screen.getByTestId('rail-repo-ops-kb')); });
    expect(await screen.findByTestId('repo-settings')).toBeInTheDocument();
    expect(tabNames()).toEqual(['Browse', 'Activity', 'Settings']);
  });

  it('the gear says logs and authorizations only, and has no Fleet tab', async () => {
    await primeApi();
    await mountApp();
    const gear = screen.getByTestId('server-toggle');
    expect(gear).toHaveAttribute('title', 'Server: logs and authorizations');
    await act(async () => { fireEvent.click(gear); });
    await screen.findByTestId('logs-facet');
    expect(screen.queryByTestId('server-tab-fleet')).toBeNull();
  });

  it('leaves the fleet target when GET /fleet says the fleet is gone', async () => {
    const m = await primeApi();
    await mountApp();
    await act(async () => { fireEvent.click(await screen.findByTestId('rail-fleet-row')); });
    await screen.findByTestId('fleet-members');
    m.getFleet.mockResolvedValue({ state: 'standalone', agent_id: 'agent-7f3a', local: true, host: { listener_on: false, external_addresses: [] }, fleet_urls: [] });
    await act(async () => { await new Promise(r => setTimeout(r, 5200)); });
    await waitFor(() => expect(screen.queryByTestId('fleet-members')).toBeNull());
    expect(screen.getByTestId('target-label')).toHaveTextContent('ops-kb');
    expect(screen.getByTestId('rail-fleet-none')).toHaveTextContent('Not in a fleet');
  }, 15000);
});
