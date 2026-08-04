import { describe, it, expect, vi, beforeEach } from 'vitest';
import { parseSearchQuery, parseFilterQuery, parseNDJSONLine, api } from './api';

describe('parseSearchQuery', () => {
  it('parses plain text', () => {
    const r = parseSearchQuery('hello world');
    expect(r).toEqual({ text: 'hello world', domains: [], entities: [] });
  });

  it('extracts domain filter', () => {
    const r = parseSearchQuery('domain:science foo');
    expect(r).toEqual({ text: 'foo', domains: ['science'], entities: [] });
  });

  it('extracts entity filter', () => {
    const r = parseSearchQuery('entity:alice bar');
    expect(r).toEqual({ text: 'bar', domains: [], entities: ['alice'] });
  });

  it('handles multiple filters', () => {
    const r = parseSearchQuery('domain:a domain:b entity:x search text');
    expect(r).toEqual({ text: 'search text', domains: ['a', 'b'], entities: ['x'] });
  });

  it('extracts quoted phrases', () => {
    const r = parseSearchQuery('"exact phrase" other');
    expect(r).toEqual({ text: 'exact phrase other', domains: [], entities: [] });
  });

  it('handles empty string', () => {
    const r = parseSearchQuery('');
    expect(r).toEqual({ text: '', domains: [], entities: [] });
  });

  it('handles filter-only input with no free text', () => {
    const r = parseSearchQuery('domain:x entity:y');
    expect(r).toEqual({ text: '', domains: ['x'], entities: ['y'] });
  });

  it('ignores empty filter value after colon', () => {
    const r = parseSearchQuery('domain: text');
    expect(r).toEqual({ text: 'text', domains: [], entities: [] });
  });

  it('extracts quoted entity with spaces', () => {
    const r = parseSearchQuery('entity:"Composer 2"');
    expect(r).toEqual({ text: '', domains: [], entities: ['Composer 2'] });
  });

  it('extracts quoted domain with spaces', () => {
    const r = parseSearchQuery('domain:"machine learning" foo');
    expect(r).toEqual({ text: 'foo', domains: ['machine learning'], entities: [] });
  });

  it('mixes quoted and unquoted filters', () => {
    const r = parseSearchQuery('entity:"Composer 2" domain:php search text');
    expect(r).toEqual({ text: 'search text', domains: ['php'], entities: ['Composer 2'] });
  });

  it('handles bare quoted string alongside quoted filter', () => {
    const r = parseSearchQuery('entity:"Composer 2" "exact phrase"');
    expect(r).toEqual({ text: 'exact phrase', domains: [], entities: ['Composer 2'] });
  });
});

describe('parseFilterQuery', () => {
  it('extracts domain and type chips with free text', () => {
    const r = parseFilterQuery('domain:go type:concept free text');
    expect(r).toEqual({
      chips: [{ category: 'domain', value: 'go' }, { category: 'type', value: 'concept' }],
      text: 'free text',
      warnings: [],
    });
  });

  it('extracts quoted entity and unquoted path chips', () => {
    const r = parseFilterQuery('entity:"supply chain" path:kb/go');
    expect(r).toEqual({
      chips: [{ category: 'entity', value: 'supply chain' }, { category: 'path', value: 'kb/go' }],
      text: '',
      warnings: [],
    });
  });

  it('ep: prefix is recognized as a filter chip', () => {
    const r = parseFilterQuery('ep:learn domain:go goroutine scheduling');
    expect(r.chips).toHaveLength(2);
    expect(r.chips).toContainEqual({ category: 'ep', value: 'learn' });
    expect(r.chips).toContainEqual({ category: 'domain', value: 'go' });
    expect(r.text).toBe('goroutine scheduling');
  });

  it('origin: prefix is recognized as a filter chip', () => {
    const r = parseFilterQuery('origin:discovered domain:go emergent facts');
    expect(r.chips).toHaveLength(2);
    expect(r.chips).toContainEqual({ category: 'origin', value: 'discovered' });
    expect(r.chips).toContainEqual({ category: 'domain', value: 'go' });
    expect(r.text).toBe('emergent facts');
  });

  it('multiple type chips from typed syntax', () => {
    const r = parseFilterQuery('type:concept type:principle');
    expect(r.chips).toHaveLength(2);
    expect(r.chips[0]).toEqual({ category: 'type', value: 'concept' });
    expect(r.chips[1]).toEqual({ category: 'type', value: 'principle' });
    expect(r.text).toBe('');
  });

  it('path chip with deep path', () => {
    const r = parseFilterQuery('path:kb/technology/ai/anthropic');
    expect(r.chips).toEqual([{ category: 'path', value: 'kb/technology/ai/anthropic' }]);
  });

  it('mixed domain entity type and free text', () => {
    const r = parseFilterQuery('domain:go entity:goroutine type:concept scheduling');
    expect(r.chips).toHaveLength(3);
    expect(r.text).toBe('scheduling');
  });

  it('quoted entity with spaces preserved', () => {
    const r = parseFilterQuery('entity:"supply chain security"');
    expect(r.chips).toEqual([{ category: 'entity', value: 'supply chain security' }]);
  });

  it('empty input returns no chips and empty text', () => {
    const r = parseFilterQuery('');
    expect(r.chips).toHaveLength(0);
    expect(r.text).toBe('');
  });

  it('multiple ep chips for history filtering', () => {
    const r = parseFilterQuery('ep:learn ep:retract');
    expect(r.chips).toHaveLength(2);
    expect(r.chips[0]).toEqual({ category: 'ep', value: 'learn' });
    expect(r.chips[1]).toEqual({ category: 'ep', value: 'retract' });
    expect(r.text).toBe('');
  });

  it('ep chip with free text for commit message search', () => {
    const r = parseFilterQuery('ep:retract cybersecurity apt28');
    expect(r.chips).toEqual([{ category: 'ep', value: 'retract' }]);
    expect(r.text).toBe('cybersecurity apt28');
  });

  it('ep and domain chips can coexist', () => {
    const r = parseFilterQuery('ep:learn domain:go');
    expect(r.chips).toHaveLength(2);
    expect(r.chips).toContainEqual({ category: 'ep', value: 'learn' });
    expect(r.chips).toContainEqual({ category: 'domain', value: 'go' });
  });
});

describe('api.fact', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  it('appends ?fallback=before only when commit is provided AND opts.fallback is set', async () => {
    const calls: string[] = [];
    globalThis.fetch = vi.fn().mockImplementation(async (url: string) => {
      calls.push(url);
      return { ok: true, status: 200, json: async () => ({ path: 'kb/x.md', title: 'X', body: '', as_of: { commit: 'abc1234' } }) };
    });

    // Commit + fallback → query string appended.
    await api.fact('alpha', 'main', 'kb/x.md', 'abc1234', { fallback: 'before' });
    expect(calls[0]).toContain('/commits/abc1234/facts/kb/x.md');
    expect(calls[0]).toContain('?fallback=before');

    // Commit but no fallback opt → no query string.
    await api.fact('alpha', 'main', 'kb/x.md', 'abc1234');
    expect(calls[1]).toContain('/commits/abc1234/facts/kb/x.md');
    expect(calls[1]).not.toContain('fallback=');

    // No commit (HEAD-anchored) but fallback opt set → still no query string,
    // because fallback is meaningless for HEAD reads.
    await api.fact('alpha', 'main', 'kb/x.md', undefined, { fallback: 'before' });
    expect(calls[2]).not.toContain('/commits/');
    expect(calls[2]).not.toContain('fallback=');
  });
});

describe('api.getAgentBranch', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  it('prefers the server-reported agent_branch over the alphabetical-first agent/* branch', async () => {
    // A repo connected to a shared remote: two agent/* branches, the foreign one
    // sorting first. The branches heuristic alone would pick the wrong (foreign)
    // branch; the repo-details agent_branch is authoritative.
    globalThis.fetch = vi.fn().mockImplementation(async (url: string) => {
      if (url.endsWith('/branches')) {
        return { ok: true, status: 200, json: async () => ({ _embedded: { branches: [
          { name: 'agent/Alexs-MacBook-Air-6.local-60def18b' },
          { name: 'agent/mindev.local-8ef0cd32' },
        ] } }) };
      }
      return { ok: true, status: 200, json: async () => ({ name: 'core', agent_branch: 'agent/mindev.local-8ef0cd32' }) };
    });

    const branch = await api.getAgentBranch('core');
    expect(branch).toBe('agent/mindev.local-8ef0cd32');
  });

  it('falls back to the branch-list heuristic when the server omits agent_branch', async () => {
    globalThis.fetch = vi.fn().mockImplementation(async (url: string) => {
      if (url.endsWith('/branches')) {
        return { ok: true, status: 200, json: async () => ({ _embedded: { branches: [
          { name: 'main' },
          { name: 'agent/host-1' },
        ] } }) };
      }
      return { ok: true, status: 200, json: async () => ({ name: 'core' }) }; // no agent_branch
    });

    const branch = await api.getAgentBranch('core');
    expect(branch).toBe('agent/host-1');
  });
});

describe('api.getOrigin', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  it('returns the parsed origin on 200', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      { ok: true, status: 200, json: async () => ({ url: 'https://example.com/r.git' }) });
    const o = await api.getOrigin('core');
    expect(o).toEqual({ url: 'https://example.com/r.git' });
  });

  it('returns null when no origin is configured (204)', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(
      { ok: false, status: 204, json: async () => { throw new Error('no body'); } });
    expect(await api.getOrigin('core')).toBeNull();
  });

  it('throws on a server error instead of parsing the error body as an origin', async () => {
    // Regression: getOrigin used to call r.json() on any non-204 response, so a
    // 500 error body was rendered as a bogus "connected" origin panel.
    globalThis.fetch = vi.fn().mockResolvedValue(
      { ok: false, status: 500, statusText: 'Internal Server Error', json: async () => ({ error: 'boom' }) });
    await expect(api.getOrigin('core')).rejects.toThrow('origin → 500');
  });
});

describe('api.explain (grouping)', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  type RawRef = { path: string; title: string; type?: string; commit?: string; committed_at?: number; deleted?: boolean };
  function mockExplainResponses(incoming: RawRef[], outgoing: RawRef[]) {
    globalThis.fetch = vi.fn().mockImplementation(async (url: string) => {
      const refs = url.endsWith('/incoming') ? incoming : outgoing;
      return { ok: true, status: 200, json: async () => ({ _embedded: { refs } }) };
    });
  }

  it('groups multiple ref-events with the same path into one group, newest-first', async () => {
    mockExplainResponses(
      [
        { path: 'kb/A.md', title: 'A', commit: 'aaaaaaa', committed_at: 1000 },
        { path: 'kb/A.md', title: 'A', commit: 'bbbbbbb', committed_at: 2000 },
        { path: 'kb/B.md', title: 'B', commit: 'ccccccc', committed_at: 1500 },
      ],
      [],
    );
    const r = await api.explain('alpha', 'main', 'kb/x.md');
    expect(r.incoming).toHaveLength(2);
    const groupA = r.incoming.find(g => g.path === 'kb/A.md')!;
    const groupB = r.incoming.find(g => g.path === 'kb/B.md')!;
    expect(groupA.versions).toHaveLength(2);
    expect(groupB.versions).toHaveLength(1);
    // Newest-first: bbbbbbb (committed_at 2000) before aaaaaaa (committed_at 1000).
    expect(groupA.versions[0].commit).toBe('bbbbbbb');
    expect(groupA.versions[1].commit).toBe('aaaaaaa');
    // Title comes from the latest version.
    expect(groupA.title).toBe('A');
  });

  it('single-version: one ref produces one group with versions.length === 1', async () => {
    mockExplainResponses(
      [{ path: 'kb/only.md', title: 'Only', commit: 'aaaaaaa', committed_at: 100 }],
      [],
    );
    const r = await api.explain('alpha', 'main', 'kb/x.md');
    expect(r.incoming).toHaveLength(1);
    expect(r.incoming[0].versions).toHaveLength(1);
    expect(r.incoming[0].versions[0].commit).toBe('aaaaaaa');
  });

  it('orders versions strictly by committed_at descending', async () => {
    mockExplainResponses(
      [
        { path: 'kb/A.md', title: 'A', commit: 'old', committed_at: 100 },
        { path: 'kb/A.md', title: 'A', commit: 'mid', committed_at: 200 },
        { path: 'kb/A.md', title: 'A', commit: 'new', committed_at: 300 },
      ],
      [],
    );
    const r = await api.explain('alpha', 'main', 'kb/x.md');
    expect(r.incoming[0].versions.map(v => v.commit)).toEqual(['new', 'mid', 'old']);
  });

  it('falls back to backend insertion order when committed_at is missing', async () => {
    mockExplainResponses(
      [
        { path: 'kb/A.md', title: 'A', commit: 'first' },
        { path: 'kb/A.md', title: 'A', commit: 'second' },
      ],
      [],
    );
    const r = await api.explain('alpha', 'main', 'kb/x.md');
    expect(r.incoming[0].versions.map(v => v.commit)).toEqual(['first', 'second']);
  });

  // OUTGOING keeps backend order and never re-sorts by recency: `commit` is the
  // edge's target_commit — the version this fact reasoned over — and choosing
  // the later of two would resolve the ref against a version the referrer never
  // saw (kb/principles/philosophy/historical-not-current). A correctly-indexed
  // source version carries exactly ONE edge per target, so this only bites on
  // the duplicate-edge defect logged in
  // .claude/harness/scratch/duplicate-derived-from-edges.md.
  it('outgoing keeps backend order — recency does not pick the pinned commit', async () => {
    mockExplainResponses(
      [],
      [
        { path: 'kb/T.md', title: 'T', commit: 'as-referenced', committed_at: 100, deleted: false },
        { path: 'kb/T.md', title: 'T', commit: 'later', committed_at: 200, deleted: true },
      ],
    );
    const r = await api.explain('alpha', 'main', 'kb/x.md');
    expect(r.outgoing[0].versions[0].commit).toBe('as-referenced');
    // Group fields come from the SAME entry the group is pinned to, so the row
    // is not marked retracted on the strength of a version never referenced.
    expect(r.outgoing[0].deleted).toBe(false);
  });

  it('incoming still leads with the most recent citing version', async () => {
    mockExplainResponses(
      [
        { path: 'kb/S.md', title: 'S', commit: 'older-source', committed_at: 100 },
        { path: 'kb/S.md', title: 'S', commit: 'newer-source', committed_at: 200 },
      ],
      [],
    );
    const r = await api.explain('alpha', 'main', 'kb/x.md');
    // "Who cites me" is a question about the present: several versions of one
    // source citing this fact is the intended multi-edge case.
    expect(r.incoming[0].versions[0].commit).toBe('newer-source');
  });

  it('uses the commit-anchored URL when commit is provided', async () => {
    const calls: string[] = [];
    globalThis.fetch = vi.fn().mockImplementation(async (url: string) => {
      calls.push(url);
      return { ok: true, status: 200, json: async () => ({ _embedded: { refs: [] } }) };
    });
    await api.explain('alpha', 'main', 'kb/x.md', 'abc1234');
    expect(calls).toHaveLength(2);
    // Commit-anchored edges use the commit-anchored URL but without fallback
    // by default (fallback only applied when explicitly requested via opts).
    expect(calls[0]).toBe('/api/v1/repos/alpha/branches/main/commits/abc1234/facts/kb/x.md/incoming');
    expect(calls[1]).toBe('/api/v1/repos/alpha/branches/main/commits/abc1234/facts/kb/x.md/outgoing');
  });

  it('uses the HEAD-anchored URL when commit is omitted', async () => {
    const calls: string[] = [];
    globalThis.fetch = vi.fn().mockImplementation(async (url: string) => {
      calls.push(url);
      return { ok: true, status: 200, json: async () => ({ _embedded: { refs: [] } }) };
    });
    await api.explain('alpha', 'main', 'kb/x.md');
    expect(calls[0]).toBe('/api/v1/repos/alpha/branches/main/facts/kb/x.md/incoming');
    expect(calls[1]).toBe('/api/v1/repos/alpha/branches/main/facts/kb/x.md/outgoing');
  });

  it('propagates type from each ref entry into RefVersion and RefGroup', async () => {
    mockExplainResponses(
      [
        { path: 'kb/p.md', title: 'P', type: 'principle', commit: 'aaaaaaa', committed_at: 1000 },
      ],
      [
        { path: 'kb/c.md', title: 'C', type: 'concept', commit: 'bbbbbbb', committed_at: 2000 },
      ],
    );
    const { incoming, outgoing } = await api.explain('alpha', 'main', 'kb/x.md');

    expect(incoming).toHaveLength(1);
    expect(incoming[0].type).toBe('principle');
    expect(incoming[0].versions[0].type).toBe('principle');

    expect(outgoing).toHaveLength(1);
    expect(outgoing[0].type).toBe('concept');
    expect(outgoing[0].versions[0].type).toBe('concept');
  });

  it('explain appends fallback=before only when opts.fallback is set', async () => {
    const calls: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      calls.push(url);
      return new Response(JSON.stringify({ _embedded: { refs: [] } }), { status: 200, headers: { 'content-type': 'application/json' } });
    }));
    await api.explain('r', 'b', 'kb/a.md', 'abc1234');                       // history, no fallback opt
    await api.explain('r', 'b', 'kb/a.md', 'abc1234', { fallback: 'before' }); // history + fallback
    expect(calls.some(u => u.includes('/commits/abc1234/') && !u.includes('fallback'))).toBe(true);
    expect(calls.some(u => u.includes('fallback=before'))).toBe(true);
  });
});

describe('api.factCommits', () => {
  it('exists on the api object', () => {
    expect(typeof api.factCommits).toBe('function');
  });
});

describe('api.factDiff', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  it('returns both sides on success', async () => {
    globalThis.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ path: 'kb/x.md', title: 'X', body: 'old', as_of: { commit: 'aaaaaaa' } }) })
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ path: 'kb/x.md', title: 'X', body: 'new', as_of: { commit: 'bbbbbbb' } }) });
    const r = await api.factDiff('alpha', 'main', 'kb/x.md', 'aaaaaaa', 'bbbbbbb');
    expect(r.from?.body).toBe('old');
    expect(r.to?.body).toBe('new');
  });

  it('returns null for the side that 404s (created in to)', async () => {
    globalThis.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 404, json: async () => ({}) })
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ path: 'kb/x.md', title: 'X', body: 'new', as_of: { commit: 'bbbbbbb' } }) });
    const r = await api.factDiff('alpha', 'main', 'kb/x.md', 'aaaaaaa', 'bbbbbbb');
    expect(r.from).toBeNull();
    expect(r.to?.body).toBe('new');
  });

  it('honors AbortController', async () => {
    const controller = new AbortController();
    globalThis.fetch = vi.fn().mockImplementation((_url: string, opts?: { signal?: AbortSignal }) =>
      new Promise((_, reject) => {
        opts?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
      })
    );
    const promise = api.factDiff('alpha', 'main', 'kb/x.md', 'aaaaaaa', 'bbbbbbb', controller.signal);
    controller.abort();
    await expect(promise).rejects.toThrow();
  });
});

describe('api lens client', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  it('listLenses unwraps the HAL _embedded.lenses collection', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200,
      json: async () => ({ count: 2, _embedded: { lenses: [
        { name: 'dev', write: 'work', reads: [{ repo: 'core' }] },
        { name: 'ops', write: 'ops', reads: [] },
      ] } }),
    });
    const lenses = await api.listLenses();
    expect(lenses).toHaveLength(2);
    expect(lenses[0].name).toBe('dev');
    expect((globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls[0][0]).toBe('/api/v1/lenses');
  });

  it('listLenses falls back to [] when the embedded shape is missing', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) });
    expect(await api.listLenses()).toEqual([]);
  });

  it('getLens GETs /api/v1/lenses/{name}', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200,
      json: async () => ({ name: 'dev', write: 'work', reads: [{ repo: 'core', branch: 'main' }] }),
    });
    const lens = await api.getLens('dev');
    expect(lens.write).toBe('work');
    expect(lens.reads[0].branch).toBe('main');
    expect((globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls[0][0]).toBe('/api/v1/lenses/dev');
  });

  it('createLens POSTs the assembled body and returns the created lens', async () => {
    const calls: Array<[string, RequestInit]> = [];
    globalThis.fetch = vi.fn().mockImplementation(async (url: string, init: RequestInit) => {
      calls.push([url, init]);
      return { ok: true, status: 201, json: async () => ({ name: 'dev', write: 'work', reads: [{ repo: 'core' }] }) };
    });
    const body = { name: 'dev', write: 'work', reads: [{ repo: 'core' }] };
    const lens = await api.createLens(body);
    expect(lens.name).toBe('dev');
    expect(calls[0][0]).toBe('/api/v1/lenses');
    expect(calls[0][1].method).toBe('POST');
    expect(JSON.parse(calls[0][1].body as string)).toEqual(body);
  });

  it('createLens throws surfacing the problem+json detail on non-2xx', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false, status: 409, statusText: 'Conflict',
      json: async () => ({ detail: 'lens "dev" already exists' }),
    });
    await expect(api.createLens({ name: 'dev', write: 'work', reads: [] }))
      .rejects.toThrow('lens "dev" already exists');
  });

  it('deleteLens DELETEs /api/v1/lenses/{name} and resolves on 204', async () => {
    const calls: Array<[string, RequestInit]> = [];
    globalThis.fetch = vi.fn().mockImplementation(async (url: string, init: RequestInit) => {
      calls.push([url, init]);
      return { ok: true, status: 204, json: async () => { throw new Error('no body'); } };
    });
    await expect(api.deleteLens('dev')).resolves.toBeUndefined();
    expect(calls[0][0]).toBe('/api/v1/lenses/dev');
    expect(calls[0][1].method).toBe('DELETE');
  });

  it('deleteLens throws surfacing the problem detail on 404', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false, status: 404, statusText: 'Not Found',
      json: async () => ({ detail: 'no such lens' }),
    });
    await expect(api.deleteLens('nope')).rejects.toThrow('no such lens');
  });
});

describe('api lens read surface', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  function mockJSON(body: unknown): string[] {
    const calls: string[] = [];
    globalThis.fetch = vi.fn().mockImplementation(async (url: string) => {
      calls.push(url);
      return { ok: true, status: 200, json: async () => body };
    });
    return calls;
  }

  it('listLensFacts builds the union facts URL with path, query, paging, and repeated repo=', async () => {
    const calls = mockJSON({
      facts: [{ path: 'kb/a.md', title: 'A', committed_at: 10, score: 0.5,
        source: { repo: 'core', id: 'abc123def456', branch: 'main' } }],
      total: 1,
    });
    const res = await api.listLensFacts('dev', {
      path: 'kb/tech', query: 'goroutine', limit: 20, offset: 40, repos: ['core', 'beta'],
    });
    expect(calls[0]).toBe(
      '/api/v1/lenses/dev/facts?path=kb%2Ftech&query=goroutine&limit=20&offset=40&repo=core&repo=beta');
    expect(res.total).toBe(1);
    expect(res.facts[0].source).toEqual({ repo: 'core', id: 'abc123def456', branch: 'main' });
  });

  it('listLensFacts emits a bare /facts URL when no options are given', async () => {
    const calls = mockJSON({ facts: [], total: 0 });
    await api.listLensFacts('dev', {});
    expect(calls[0]).toBe('/api/v1/lenses/dev/facts');
  });

  it('lensSearch builds ?q= with repeated repo= and unwraps the flat results array', async () => {
    const calls = mockJSON({
      results: [{ path: 'kb://abc123def456/kb/b.md', title: 'B', body: '', score: 0.9,
        source: { repo: 'beta', id: 'abc123def456', branch: 'main' } }],
      total: 1,
    });
    const res = await api.lensSearch('dev', 'query text', ['core', 'beta']);
    expect(calls[0]).toBe('/api/v1/lenses/dev/search?q=query+text&repo=core&repo=beta');
    expect(res).toHaveLength(1);
    expect(res[0].source).toEqual({ repo: 'beta', id: 'abc123def456', branch: 'main' });
  });

  it('lensSearch omits repo= when no repos are passed', async () => {
    const calls = mockJSON({ results: [], total: 0 });
    await api.lensSearch('dev', 'x');
    expect(calls[0]).toBe('/api/v1/lenses/dev/search?q=x');
  });

  it('lensCompletions builds the category+prefix URL', async () => {
    const calls = mockJSON({ values: ['core', 'beta'] });
    const res = await api.lensCompletions('dev', 'repo', 'co');
    expect(calls[0]).toBe('/api/v1/lenses/dev/completions?category=repo&prefix=co');
    expect(res.values).toEqual(['core', 'beta']);
  });

  it('lensCompletions defaults prefix to empty', async () => {
    const calls = mockJSON({ values: [] });
    await api.lensCompletions('dev', 'domain');
    expect(calls[0]).toBe('/api/v1/lenses/dev/completions?category=domain&prefix=');
  });

  it('getLensFact URL-encodes an entire kb:// qualified path as one segment', async () => {
    const calls = mockJSON({
      path: 'kb://abc123def456/kb/foo.md', title: 'Foo', body: 'hi',
      as_of: { commit: 'deadbee' },
      source: { repo: 'beta', id: 'abc123def456', branch: 'main' },
    });
    const f = await api.getLensFact('dev', 'kb://abc123def456/kb/foo.md');
    expect(calls[0]).toBe(
      '/api/v1/lenses/dev/facts/kb%3A%2F%2Fabc123def456%2Fkb%2Ffoo.md');
    // normalized fact body preserved, as_of.commit hoisted, source attached
    expect(f.body).toBe('hi');
    expect(f.commit_hash).toBe('deadbee');
    expect(f.source).toEqual({ repo: 'beta', id: 'abc123def456', branch: 'main' });
  });

  it('getLensFact encodes a bare write-repo path', async () => {
    const calls = mockJSON({
      path: 'kb/x.md', title: 'X', body: '',
      source: { repo: 'core', id: 'abc123def456', branch: 'main' },
    });
    await api.getLensFact('dev', 'kb/x.md');
    expect(calls[0]).toBe('/api/v1/lenses/dev/facts/kb%2Fx.md');
  });

  it('getLensStats builds the lens stats URL with the encoded path and returns the flat envelope', async () => {
    const calls = mockJSON({
      total: 250, repo_count: 2, last_commit: '2026-07-20T10:00:00Z', avg_confidence: 0.82,
      domains: { go: 7 }, entities: {},
      repos: [{ id: 'abc123def456', name: 'core', source: '', branch: 'agent/main', is_write: true,
        total: 200, avg_confidence: 0.9, domains: { go: 5 }, entities: {},
        last_commit: '2026-07-19T09:00:00Z', changes_7d: 1, changes_30d: 2, changes_90d: 3 }],
    });
    const res = await api.getLensStats('dev', 'kb/tech');
    expect(calls[0]).toBe('/api/v1/lenses/dev/stats?path=kb%2Ftech');
    expect(res.total).toBe(250);
    expect(res.repo_count).toBe(2);
    expect(res.repos[0].is_write).toBe(true);
  });

  it('lensBrowse strips the ontology root and emits the bare /topics URL at the root', async () => {
    const calls = mockJSON({ path: 'kb', children: [] });
    const res = await api.lensBrowse('dev', 'kb', 'kb');
    expect(calls[0]).toBe('/api/v1/lenses/dev/topics');
    expect(res.path).toBe('kb');
    expect(res.children).toEqual([]);
  });

  it('lensBrowse builds the node URL with repeated repo= and surfaces leaf path/source', async () => {
    const calls = mockJSON({
      path: 'kb/decisions',
      children: [
        { name: 'lens', is_dir: true },
        { name: 'a.md', is_dir: false, type: 'decision', title: 'A',
          path: 'kb://abc123def456/kb/decisions/a.md', source: { repo: 'docs', id: 'abc123def456' } },
      ],
    });
    const res = await api.lensBrowse('dev', 'kb/decisions', 'kb', ['core', 'docs']);
    expect(calls[0]).toBe('/api/v1/lenses/dev/topics/decisions?repo=core&repo=docs');
    expect(res.children).toHaveLength(2);
    expect(res.children[0]).toEqual({ name: 'lens', is_dir: true });
    expect(res.children[1].path).toBe('kb://abc123def456/kb/decisions/a.md');
    expect(res.children[1].source).toEqual({ repo: 'docs', id: 'abc123def456' });
  });

  it('lensBrowse tolerates a missing children array', async () => {
    const calls = mockJSON({ path: 'kb' });
    const res = await api.lensBrowse('dev', 'kb', 'kb');
    expect(calls[0]).toBe('/api/v1/lenses/dev/topics');
    expect(res.children).toEqual([]);
  });

  it('updateLens PATCHes the body and returns the updated lens with description', async () => {
    const calls: Array<[string, RequestInit]> = [];
    globalThis.fetch = vi.fn().mockImplementation(async (url: string, init: RequestInit) => {
      calls.push([url, init]);
      return { ok: true, status: 200, json: async () => ({
        name: 'dev', write: 'work', description: 'my lens', reads: [{ repo: 'core' }],
      }) };
    });
    const body = { write: 'work', description: 'my lens', reads: [{ repo: 'core' }] };
    const lens = await api.updateLens('dev', body);
    expect(calls[0][0]).toBe('/api/v1/lenses/dev');
    expect(calls[0][1].method).toBe('PATCH');
    expect(JSON.parse(calls[0][1].body as string)).toEqual(body);
    expect(lens.description).toBe('my lens');
  });

  it('updateLens surfaces the problem+json detail on non-2xx', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false, status: 422, statusText: 'Unprocessable Entity',
      json: async () => ({ detail: 'unknown repo "ghost"' }),
    });
    await expect(api.updateLens('dev', { write: 'ghost' })).rejects.toThrow('unknown repo "ghost"');
  });
});

describe('parseNDJSONLine', () => {
  it('parses a progress line', () => {
    const e = parseNDJSONLine('{"type":"progress","step":"clone","message":"x","pct":40}');
    expect(e?.type).toBe('progress');
    expect(e?.pct).toBe(40);
  });
  it('parses a done line with repo', () => {
    const e = parseNDJSONLine('{"type":"done","repo":{"name":"work"}}');
    expect(e?.type).toBe('done');
    expect(e?.repo?.name).toBe('work');
  });
  it('returns null for blank/garbage lines', () => {
    expect(parseNDJSONLine('   ')).toBeNull();
    expect(parseNDJSONLine('not json')).toBeNull();
  });
});

describe('Stats highlights contract', () => {
  it('carries highlights, types and default_axis', async () => {
    const payload = {
      total: 2,
      avg_confidence: 0.8,
      domains: {}, entities: {},
      types: { synthesis: 1, observation: 1 },
      default_axis: 'impact',
      highlights: [{
        path: 'kb/s/a.md', title: 'A', type: 'synthesis',
        confidence: 0.8, impact: 7, committed_at: 1780000000,
      }],
      _links: { self: { href: '/x' } },
    };
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true, status: 200,
      headers: new Headers({ 'content-type': 'application/hal+json' }),
      json: async () => payload,
    }) as unknown as typeof fetch;

    const s = await api.stats('core', 'main', '');
    expect(s.default_axis).toBe('impact');
    expect(s.types.synthesis).toBe(1);
    expect(s.highlights).toHaveLength(1);
    expect(s.highlights[0].impact).toBe(7);
    expect(s.highlights[0].committed_at).toBe(1780000000);
  });
});
