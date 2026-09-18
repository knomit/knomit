import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api, statusFromBranchBody } from './api';

// A branch-root body as the server builds it — the same shape the branch GET
// returns and the same shape the repo and lens resources embed.
const branchBody = {
  name: 'agent/test',
  head: 'h1',
  index_commit: 'i1',
  embeddings_enabled: true,
  index_state: 'ready',
  index_done: 3,
  index_total: 3,
  index_percent: 100,
  _links: { self: { href: '/api/v1/repos/alpha/branches/agent:test' } },
};

function mockFetchOnce(body: unknown) {
  return vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    headers: new Headers({ 'content-type': 'application/hal+json' }),
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
}

describe('embedded branch roots', () => {
  const realFetch = globalThis.fetch;
  beforeEach(() => { vi.restoreAllMocks(); });
  afterEach(() => { globalThis.fetch = realFetch; });

  it('getRepo lifts _embedded.branch onto branch as a normalised Status', async () => {
    globalThis.fetch = mockFetchOnce({
      name: 'alpha', read_branch: 'agent/test', agent_branch: 'agent/test',
      _embedded: { branch: branchBody },
    }) as any;

    const details = await api.getRepo('alpha');

    expect(details.name).toBe('alpha');
    // Parsed by the SAME normaliser the branch fetch uses, so the two paths
    // cannot produce different Statuses for identical bytes.
    expect(details.branch).toEqual(statusFromBranchBody(branchBody, 'agent/test'));
    expect(details.branch?.branch).toBe('agent/test');
    expect(details.branch?.head).toBe('h1');
    expect(details.branch?.index_percent).toBe(100);
    // The raw envelope must not leak through as a second shape.
    expect((details as any)._embedded).toBeUndefined();
  });

  it('getRepo leaves branch undefined when the server omitted the embed', async () => {
    globalThis.fetch = mockFetchOnce({ name: 'alpha', read_branch: 'agent/test' }) as any;

    const details = await api.getRepo('alpha');

    expect(details.name).toBe('alpha');
    expect(details.branch).toBeUndefined();
  });

  it('getLens lifts _embedded.write_branch, and omits it for a subscription write member', async () => {
    globalThis.fetch = mockFetchOnce({
      name: 'eng', write: { uid: 'u1', name: 'alpha' }, reads: [],
      _embedded: { write_branch: branchBody },
    }) as any;
    const withBranch = await api.getLens('eng');
    expect(withBranch.write_branch).toEqual(statusFromBranchBody(branchBody, 'agent/test'));

    // A subscription write member has no agent branch, so the key is absent.
    globalThis.fetch = mockFetchOnce({
      name: 'eng', write: { uid: 'u1', name: 'alpha' }, reads: [],
    }) as any;
    const without = await api.getLens('eng');
    expect(without.write_branch).toBeUndefined();
  });

  it('ignores an embed carrying no branch name rather than inventing one', async () => {
    globalThis.fetch = mockFetchOnce({
      name: 'alpha', _embedded: { branch: { ...branchBody, name: '' } },
    }) as any;

    const details = await api.getRepo('alpha');
    expect(details.branch).toBeUndefined();
  });
});
