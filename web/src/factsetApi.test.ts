import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, lensListSearchParams, listSearchParams } from './api';
import type { ListFilters } from './api';
import { DEFAULT_OPTIONS } from './materializeApi';
import { FS_PAGE_LIMIT, FsError, factsetApi } from './factsetApi';
import type { FsSearch, FsTarget } from './factsetApi';

afterEach(() => vi.unstubAllGlobals());

const page = { facts: [], cursor: null, has_more: false, units: 0 };

function stubFetch(res: () => Response) {
  const f = vi.fn(async (_url: string, _init?: RequestInit) => res());
  vi.stubGlobal('fetch', f);
  return f;
}
const urlOf = (f: ReturnType<typeof stubFetch>, i: number) => new URL(f.mock.calls[i][0], 'http://x');

/** Every key of u with its values, sorted, minus the ignored keys. */
function keysOf(u: URL, ignore: string[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const k of [...new Set(u.searchParams.keys())].sort()) {
    if (!ignore.includes(k)) out[k] = u.searchParams.getAll(k);
  }
  return out;
}

const OWN = ['sort', 'depth', 'max_nodes', 'unpin', 'cross_kb', 'limit', 'facts'];
const PAGING = ['sort', 'offset', 'limit'];

const filters: ListFilters = {
  types: ['observation', 'policy'], kinds: ['epistemic'], origins: ['authored'], eps: ['x'],
  domains: ['web'], entities: ['Library'], motifs: ['drift-config'], motifMatch: 'stem', sinceFork: true,
};

// Test 1 (plan 7.1): the FactSet IS the list. For each list the app reads, the
// list's own request and the FactSet's first call agree key for key, except the
// list's paging (offset, limit, sort) and the FactSet's own options. Each case
// asserts that the shared keys are really there (so an empty agreement cannot
// pass) and which values they carry.
//
// Sabotage: drop one key from the FactSet side (firstParams deletes 'type').
describe('the FactSet first call is the list search (test 1)', () => {
  const repoT: FsTarget = { kind: 'repo', repo: 'kb', branch: 'agent/x' };

  it('relevance: api.search and the FactSet agree, chips, motifs and since_fork included', async () => {
    const f = stubFetch(() => new Response(JSON.stringify({ results: [] })));
    await api.search('kb', 'agent/x', 'goroutine domain:go', 'kb/tech', 0, filters);
    const search: FsSearch = { target: repoT, sort: 'relevance', params: listSearchParams('relevance', 'goroutine domain:go', 'kb/tech', 0, filters).toString() };
    await factsetApi.first(search, DEFAULT_OPTIONS);
    const list = urlOf(f, 0), fs = urlOf(f, 1);
    expect(list.pathname).toBe('/api/v1/repos/kb/branches/agent:x/search');
    expect(fs.pathname).toBe('/api/v1/repos/kb/branches/agent:x/search/factset');
    const want = keysOf(list, PAGING);
    expect(keysOf(fs, OWN)).toEqual(want);
    expect(want).toMatchObject({
      q: ['goroutine'], domain: ['go,web'], entities: ['Library'], path: ['kb/tech'], type: ['observation,policy'],
      kind: ['epistemic'], origin: ['authored'], ep: ['x'], since_fork: ['1'], motifs: ['drift-config'], motif_match: ['stem'],
    });
    expect(fs.searchParams.get('sort')).toBe('relevance');
  });

  it('recent (a chip turned Path into Recent): api.recent and the FactSet agree', async () => {
    const f = stubFetch(() => new Response(JSON.stringify({ facts: [], count: 0 })));
    await api.recent('kb', 'agent/x', 'kb/tech', '', 50, 100, filters);
    const search: FsSearch = { target: repoT, sort: 'recent', params: listSearchParams('recent', '', 'kb/tech', 0, filters).toString() };
    await factsetApi.first(search, DEFAULT_OPTIONS);
    const list = urlOf(f, 0), fs = urlOf(f, 1);
    expect(list.searchParams.get('offset')).toBe('100');
    expect(fs.searchParams.has('offset')).toBe(false);
    const want = keysOf(list, PAGING);
    expect(keysOf(fs, OWN)).toEqual(want);
    expect(want).toMatchObject({ path: ['kb/tech'], type: ['observation,policy'], since_fork: ['1'], motifs: ['drift-config'] });
    expect(fs.searchParams.get('sort')).toBe('recent');
  });

  it('lens relevance and lens recent: the lens list and the lens FactSet agree, repo= narrowing included', async () => {
    const f = stubFetch(() => new Response(JSON.stringify({ results: [], facts: [], total: 0 })));
    const lensT: FsTarget = { kind: 'lens', lens: 'dev' };
    const lensOpts = { ...filters, sinceFork: undefined, path: 'kb/tech' };
    await api.lensSearch('dev', 'goroutine', ['alpha', 'beta'], lensOpts);
    await factsetApi.first({ target: lensT, sort: 'relevance', params: lensListSearchParams('relevance', 'goroutine', ['alpha', 'beta'], lensOpts).toString() }, DEFAULT_OPTIONS);
    await api.listLensFacts('dev', { ...lensOpts, limit: 50, offset: 0, repos: ['alpha'] });
    await factsetApi.first({ target: lensT, sort: 'recent', params: lensListSearchParams('recent', '', ['alpha'], lensOpts).toString() }, DEFAULT_OPTIONS);
    expect(urlOf(f, 1).pathname).toBe('/api/v1/lenses/dev/search/factset');
    expect(keysOf(urlOf(f, 1), OWN)).toEqual(keysOf(urlOf(f, 0), PAGING));
    expect(keysOf(urlOf(f, 0), PAGING)).toMatchObject({ q: ['goroutine'], repo: ['alpha', 'beta'], type: ['observation,policy'] });
    expect(keysOf(urlOf(f, 3), OWN)).toEqual(keysOf(urlOf(f, 2), PAGING));
    expect(keysOf(urlOf(f, 2), PAGING)).toMatchObject({ path: ['kb/tech'], repo: ['alpha'], domain: ['web'] });
  });
});

// Test 2 (plan 7.2): the URLs of every call; a cursor call carries ONLY the
// cursor (+ unit); the error kind and Retry-After come from the response.
//
// Sabotage: send the options on a cursor call (next carries depth), or read
// Retry-After as minutes.
describe('factsetApi calls (test 2)', () => {
  const T: FsTarget = { kind: 'repo', repo: 'kb', branch: 'main' };
  const S: FsSearch = { target: T, sort: 'recent', params: 'path=kb%2Fu' };

  it('first carries the list, sort, depth, max_nodes = references + 1, limit 5 and no facts', async () => {
    const f = stubFetch(() => new Response(JSON.stringify(page)));
    await factsetApi.first(S, { depth: 3, references: 99, crossKb: true, unpin: true });
    const u = urlOf(f, 0);
    expect(Object.fromEntries(u.searchParams)).toEqual({
      path: 'kb/u', sort: 'recent', depth: '3', max_nodes: '100', unpin: 'true', cross_kb: 'true', limit: String(FS_PAGE_LIMIT),
    });
    expect(FS_PAGE_LIMIT).toBe(5);
  });

  it('a lens first call never sends cross_kb (a lens refuses it)', async () => {
    const f = stubFetch(() => new Response(JSON.stringify(page)));
    await factsetApi.first({ ...S, target: { kind: 'lens', lens: 'one' } }, { ...DEFAULT_OPTIONS, crossKb: true });
    const u = urlOf(f, 0);
    expect(u.pathname).toBe('/api/v1/lenses/one/search/factset');
    expect(u.searchParams.get('depth')).toBe('4');
    expect(u.searchParams.has('cross_kb')).toBe(false);
  });

  it('next, seek, export and close carry only the cursor (and unit)', async () => {
    const f = stubFetch(() => new Response(JSON.stringify(page)));
    await factsetApi.next(T, 'ID.2.3');
    await factsetApi.seek(T, 'ID.2.3', 7);
    f.mockImplementation(async () => new Response('# doc', { headers: { 'Content-Disposition': 'attachment; filename="fs-x.md"' } }));
    const doc = await factsetApi.exportMarkdown(T, 'ID.2.3');
    f.mockImplementation(async () => new Response(null, { status: 204 }));
    await factsetApi.close(T, 'ID.2.3');
    expect(Object.fromEntries(urlOf(f, 0).searchParams)).toEqual({ cursor: 'ID.2.3' });
    expect(Object.fromEntries(urlOf(f, 1).searchParams)).toEqual({ cursor: 'ID.2.3', unit: '7' });
    expect(urlOf(f, 2).pathname).toBe('/api/v1/repos/kb/branches/main/search/factset/export');
    expect(Object.fromEntries(urlOf(f, 2).searchParams)).toEqual({ cursor: 'ID.2.3' });
    expect(doc).toEqual({ text: '# doc', serverName: 'fs-x.md' });
    expect(urlOf(f, 3).pathname).toBe('/api/v1/repos/kb/branches/main/search/factset');
    expect(f.mock.calls[3][1]).toMatchObject({ method: 'DELETE' });
    expect(Object.fromEntries(urlOf(f, 3).searchParams)).toEqual({ cursor: 'ID.2.3' });
  });

  it('a 503 with Retry-After: 1 is kind busy, retryAfter 1, with the server detail', async () => {
    stubFetch(() => new Response(JSON.stringify({ title: 'Busy', detail: 'the index moved while the search ran; retry shortly' }),
      { status: 503, headers: { 'Retry-After': '1' } }));
    const err = await factsetApi.first(S, DEFAULT_OPTIONS).catch(e => e);
    expect(err).toBeInstanceOf(FsError);
    expect(err.kind).toBe('busy');
    expect(err.retryAfter).toBe(1);
    expect(err.status).toBe(503);
    expect(err.detail).toBe('the index moved while the search ran; retry shortly');
  });

  it('404 is expired, 400 and 422 are invalid, 500 and a network error are failed', async () => {
    const kinds: string[] = [];
    for (const status of [404, 400, 422, 500]) {
      stubFetch(() => new Response('{}', { status }));
      kinds.push((await factsetApi.next(T, 'c').catch(e => e)).kind);
    }
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch'); }));
    const net = await factsetApi.next(T, 'c').catch(e => e);
    expect(kinds).toEqual(['expired', 'invalid', 'invalid', 'failed']);
    expect(net).toBeInstanceOf(FsError);
    expect(net.kind).toBe('failed');
    expect(net.status).toBeUndefined();
  });
});
