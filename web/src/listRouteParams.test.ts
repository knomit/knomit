import { describe, it, expect, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { api, branchEventsUrl, subscribeRepoEvents } from './api';
import { materializeApi } from './materializeApi';
import type { MatOptions, MatTarget } from './materializeApi';
import type { ListFilters } from './listSearch';

const MAT: MatTarget = { repo: 'r', branch: 'b', path: 'kb/x.md' };
const MAT_OPTS: MatOptions = { depth: 2, references: 9, crossKb: true, unpin: true };

// The four REST list routes answer 400 to a query parameter they do not
// declare. Their declared sets are their openapi.yaml parameter lists (the Go
// test TestListRouteParams_MatchOpenAPI pins those to the handlers), so this
// reads the spec, and checks the REAL requests the results lists send —
// captured from api.search, api.recent, lensSearch and listLensFacts with
// every option set — against them. A chip that sends a name the route does
// not declare would be a 400 in the UI.

const SPEC = readFileSync(resolve(__dirname, '../../internal/edge/web/static/openapi.yaml'), 'utf8');

/** The query parameter names openapi.yaml declares on route's GET ($refs to
 *  components/parameters resolved). A small reader for this file's own shape:
 *  inline `- { name: x, in: query, … }` rows and `- $ref:` rows. */
function specQueryParams(route: string): string[] {
  const lines = SPEC.split('\n');
  const start = lines.indexOf(`  ${route}:`);
  if (start < 0) throw new Error(`openapi.yaml has no ${route}`);
  let end = lines.findIndex((l, i) => i > start && /^ {2}\S/.test(l));
  if (end < 0) end = lines.length;
  const block = lines.slice(start, end);
  const getAt = block.findIndex(l => l === '    get:');
  const names: string[] = [];
  const get = block.slice(getAt);
  for (let i = 0; i < get.length; i++) {
    const l = get[i];
    if (/^ {4}\S/.test(l) && l !== '    get:') break; // the next method
    const inline = l.match(/^\s+- \{ name: ([^,\s]+),\s+in: query/);
    if (inline) { names.push(inline[1]); continue; }
    // a block row: `- name: x` with `in: query` on one of the next lines
    const blockRow = l.match(/^(\s+)- name: (\S+)/);
    if (blockRow) {
      const inner = blockRow[1] + '  ';
      for (let j = i + 1; j < get.length && get[j].startsWith(inner); j++) {
        if (get[j].trim() === 'in: query') { names.push(blockRow[2]); break; }
      }
      continue;
    }
    const ref = l.match(/^\s+- \$ref: "#\/components\/parameters\/(\w+)"/);
    if (ref) {
      const at = lines.indexOf(`    ${ref[1]}:`);
      const name = lines[at + 1]?.match(/^\s+name: (\S+)/);
      const where = lines[at + 2]?.match(/^\s+in: (\S+)/);
      if (name && where?.[1] === 'query') names.push(name[1]);
    }
  }
  return names;
}

function mockFetch() {
  const f = vi.fn().mockResolvedValue({
    ok: true, status: 200,
    json: async () => ({ count: 0, _embedded: { facts: [], results: [] }, facts: [], results: [], total: 0 }),
  });
  globalThis.fetch = f as unknown as typeof fetch;
  return f;
}

const sentNames = (f: ReturnType<typeof vi.fn>) =>
  [...new Set([...new URL(String(f.mock.calls[0][0]), 'http://x').searchParams.keys()])].sort();

const undeclared = (sent: string[], declared: string[]) => sent.filter(k => !declared.includes(k));

const EVERY: ListFilters = {
  types: ['observation'], excludeType: 'policy', kinds: ['epistemic'], excludeKinds: ['pragmatic'],
  origins: ['authored'], eps: ['learn'], domains: ['d'], entities: ['E'],
  motifs: ['a-b'], motifMatch: 'stem', sinceFork: true,
};

// Sabotage: drop a name the lists send (e.g. `ep`) from a route's spec list,
// or make api.recent send a parameter the spec does not declare, and the
// matching row lists the stray name.
describe('the results lists send only the parameters their route declares', () => {
  it('reads each route’s declared set from openapi.yaml', () => {
    expect(specQueryParams('/repos/{repo}/branches/{branch}/facts')).toContain('offset');
    expect(specQueryParams('/lenses/{lens}/search')).toContain('repo');
    // The relevance lists page by offset like Recent (user report 2026-11-02).
    expect(specQueryParams('/repos/{repo}/branches/{branch}/search')).toContain('offset');
    expect(specQueryParams('/lenses/{lens}/search')).toContain('offset');
  });

  it('api.search → repo /search', async () => {
    const f = mockFetch();
    await api.search('r', 'b', 'text domain:x entity:y', 'kb/x/', 0.5, EVERY, { limit: 50, offset: 50 });
    const sent = sentNames(f);
    expect(sent).toEqual(expect.arrayContaining(['since_fork', 'limit', 'offset']));
    expect(undeclared(sent, specQueryParams('/repos/{repo}/branches/{branch}/search'))).toEqual([]);
  });

  it('api.recent → repo /facts', async () => {
    const f = mockFetch();
    await api.recent('r', 'b', 'kb/x/', 'text', 50, 10, EVERY);
    const sent = sentNames(f);
    expect(sent).toEqual(expect.arrayContaining(['exclude_type', 'limit', 'offset']));
    expect(undeclared(sent, specQueryParams('/repos/{repo}/branches/{branch}/facts'))).toEqual([]);
  });

  it('lensSearch → lens /search', async () => {
    const f = mockFetch();
    await api.lensSearch('eng', 'text', ['alpha'], { ...EVERY, path: 'kb/x/' }, { limit: 50, offset: 50 });
    const sent = sentNames(f);
    expect(sent).toEqual(expect.arrayContaining(['repo', 'limit', 'offset']));
    expect(undeclared(sent, specQueryParams('/lenses/{lens}/search'))).toEqual([]);
  });

  it('listLensFacts → lens /facts', async () => {
    const f = mockFetch();
    await api.listLensFacts('eng', { ...EVERY, path: 'kb/x/', query: 'text', limit: 50, offset: 10, repos: ['alpha'] });
    const sent = sentNames(f);
    expect(sent).toEqual(expect.arrayContaining(['repo', 'limit', 'offset']));
    expect(undeclared(sent, specQueryParams('/lenses/{lens}/facts'))).toEqual([]);
  });
});

// The other knowledge-base read routes refuse undeclared parameters too
// (#718's rule, extended). Every client call, with every option it can send,
// against its route's spec list. The call's own result is not the point: a
// mock body that does not fit it may throw after the request was made.
//
// Sabotage: make fetchMotifs send `order` (or drop `sort` from the /motifs
// spec rows) and the motifs rows list the stray name.
describe('the knowledge-base reads send only the parameters their route declares', () => {
  const B = '/repos/{repo}/branches/{branch}';
  const cases: { name: string; spec: string; call: () => Promise<unknown>; sends?: string[] }[] = [
    { name: 'browse → /topics/{path}', spec: `${B}/topics/{path}`, call: () => api.browse('r', 'b', 'kb/x', 'kb') },
    { name: 'branchChanges → /changes', spec: `${B}/changes`, call: () => api.branchChanges('r', 'b', 'abc'), sends: ['since'] },
    { name: 'commitDetail → /commits/{sha}', spec: `${B}/commits/{sha}`, call: () => api.commitDetail('r', 'b', 'abc') },
    { name: 'fact at a commit → /commits/{sha}/facts/{path}', spec: `${B}/commits/{sha}/facts/{path}`, call: () => api.fact('r', 'b', 'kb/x.md', 'abc', { fallback: 'before' }), sends: ['fallback'] },
    { name: 'fact at HEAD → /facts/{path}', spec: `${B}/facts/{path}`, call: () => api.fact('r', 'b', 'kb/x.md', undefined, { fallback: 'before' }) },
    { name: 'explain at a commit → /commits/{sha}/facts/{path}/incoming', spec: `${B}/commits/{sha}/facts/{path}`, call: () => api.explain('r', 'b', 'kb/x.md', 'abc', { fallback: 'before' }) },
    { name: 'factCommits → /facts/{path}/commits', spec: `${B}/facts/{path}/commits`, call: () => api.factCommits('r', 'b', 'kb/x.md', 'a', 'f', 'z'), sends: ['after', 'from', 'before'] },
    { name: 'activity → /activity', spec: `${B}/activity`, call: () => api.activity('r', 'b', 'kb'), sends: ['path'] },
    { name: 'completions → /completions', spec: `${B}/completions`, call: () => api.completions('r', 'b', 'domain', 'x'), sends: ['category', 'prefix'] },
    { name: 'stats → /stats', spec: `${B}/stats`, call: () => api.stats('r', 'b', 'kb', 'recent'), sends: ['path', 'axis'] },
    { name: 'motifs → /motifs', spec: `${B}/motifs`, call: () => api.motifs('r', 'b', { q: 'x', path: 'kb', sort: 'name', limit: 10, offset: 5 }), sends: ['q', 'path', 'sort', 'limit', 'offset'] },
    { name: 'motifCluster → /motifs/{key}', spec: `${B}/motifs/{key}`, call: () => api.motifCluster('r', 'b', 'k') },
    { name: 'getLensFact → lens /facts/{path}', spec: '/lenses/{lens}/facts/{path}', call: () => api.getLensFact('eng', 'kb/x.md') },
    { name: 'lensCompletions → lens /completions', spec: '/lenses/{lens}/completions', call: () => api.lensCompletions('eng', 'repo', 'a'), sends: ['category', 'prefix'] },
    { name: 'lensMotifs → lens /motifs', spec: '/lenses/{lens}/motifs', call: () => api.lensMotifs('eng', { q: 'x', path: 'kb', sort: 'df', limit: 10, offset: 5 }), sends: ['q', 'path', 'sort', 'limit', 'offset'] },
    { name: 'lensMotifCluster → lens /motifs/{key}', spec: '/lenses/{lens}/motifs/{key}', call: () => api.lensMotifCluster('eng', 'k') },
    { name: 'getLensStats → lens /stats', spec: '/lenses/{lens}/stats', call: () => api.getLensStats('eng', 'kb', 'impact', ['alpha']), sends: ['path', 'axis', 'repo'] },
    { name: 'lensBrowse → lens /topics/{path}', spec: '/lenses/{lens}/topics/{path}', call: () => api.lensBrowse('eng', 'kb/x', 'kb', ['alpha']), sends: ['repo'] },
    { name: 'factDiff → /commits/{sha}/facts/{path} (both sides)', spec: `${B}/commits/{sha}/facts/{path}`, call: () => api.factDiff('r', 'b', 'kb/x.md', 'abc', 'def') },
    { name: 'materializeApi.page first call → /facts/{path}/materialize/pages', spec: `${B}/facts/{path}/materialize/pages`, call: () => materializeApi.page(MAT, MAT_OPTS, null), sends: ['depth', 'max_nodes', 'unpin', 'cross_kb', 'limit'] },
    { name: 'materializeApi.page cursor call → /facts/{path}/materialize/pages', spec: `${B}/facts/{path}/materialize/pages`, call: () => materializeApi.page(MAT, MAT_OPTS, 'c1'), sends: ['cursor'] },
    { name: 'materializeApi.page at a commit → the same parameters', spec: `${B}/facts/{path}/materialize/pages`, call: () => materializeApi.page({ ...MAT, commit: 'abc' }, MAT_OPTS, null), sends: ['depth', 'limit'] },
    { name: 'materializeApi.exportMarkdown → /facts/{path}/materialize', spec: `${B}/facts/{path}/materialize`, call: () => materializeApi.exportMarkdown(MAT, MAT_OPTS), sends: ['depth', 'max_nodes', 'unpin', 'cross_kb'] },
  ];
  for (const c of cases) {
    it(c.name, async () => {
      const f = mockFetch();
      await c.call().catch(() => undefined);
      expect(f).toHaveBeenCalled();
      const declared = specQueryParams(c.spec);
      for (let i = 0; i < f.mock.calls.length; i++) {
        const sent = [...new Set([...new URL(String(f.mock.calls[i][0]), 'http://x').searchParams.keys()])];
        expect(undeclared(sent, declared)).toEqual([]);
      }
      if (c.sends) expect(sentNames(f)).toEqual(expect.arrayContaining(c.sends));
    });
  }

  // App.tsx opens the branch stream at branchEventsUrl (an EventSource, so no
  // fetch to capture): its URL is checked against /events directly.
  it('App’s branch stream → /events', () => {
    const u = new URL(branchEventsUrl('r', 'agent/x'), 'http://x');
    expect(u.pathname).toBe('/api/v1/repos/r/branches/agent:x/events');
    expect(undeclared([...u.searchParams.keys()], specQueryParams(`${B}/events`))).toEqual([]);
  });
});

// The management API's GET routes refuse undeclared parameters too (the same
// rule on internal/edge/web/api/manage). Every web client read of them, against
// its route's spec list: none sends a parameter today, and none may start to
// without declaring it.
//
// Sabotage: make listFleetActivity ask for `/activity?limit=50` with `limit`
// dropped from the spec row, or make getRepo append a cache-buster, and that
// row lists the stray name; point a function at another route (listTemplates
// at /ontologies/presets) and its row's path check fails.
describe('the management reads send only the parameters their route declares', () => {
  const cases: { name: string; spec: string; call: () => Promise<unknown> }[] = [
    { name: 'getFleet → /fleet', spec: '/fleet', call: () => api.getFleet() },
    { name: 'listFleetMembers → /fleet/members', spec: '/fleet/members', call: () => api.listFleetMembers() },
    { name: 'listFleetActivity → /fleet/activity', spec: '/fleet/activity', call: () => api.listFleetActivity() },
    { name: 'exportFleetRoot → /fleet/root', spec: '/fleet/root', call: () => api.exportFleetRoot() },
    { name: 'listArchived → /archived', spec: '/archived', call: () => api.listArchived() },
    { name: 'repos → /repos', spec: '/repos', call: () => api.repos() },
    { name: 'listRepoCreates → /repo-creates', spec: '/repo-creates', call: () => api.listRepoCreates() },
    { name: 'getRepoCreate → /repo-creates/{id}', spec: '/repo-creates/{id}', call: () => api.getRepoCreate('j1') },
    { name: 'ontologyPresets → /ontologies/presets', spec: '/ontologies/presets', call: () => api.ontologyPresets() },
    { name: 'ontologyPresetYAML → /ontologies/presets/{name}', spec: '/ontologies/presets/{name}', call: () => api.ontologyPresetYAML('default') },
    { name: 'ontologySchema → /ontologies/schema', spec: '/ontologies/schema', call: () => api.ontologySchema() },
    { name: 'listTemplates → /templates', spec: '/templates', call: () => api.listTemplates() },
    { name: 'getRepo → /repos/{repo}', spec: '/repos/{repo}', call: () => api.getRepo('r') },
    { name: 'getOrigin → /repos/{repo}/origin', spec: '/repos/{repo}/origin', call: () => api.getOrigin('r') },
    { name: 'listBranchNames → /repos/{repo}/branches', spec: '/repos/{repo}/branches', call: () => api.listBranchNames('r') },
    { name: 'listPushedBranches → /repos/{repo}/pushed-branches', spec: '/repos/{repo}/pushed-branches', call: () => api.listPushedBranches('r') },
    { name: 'listExperiments → /repos/{repo}/experiments', spec: '/repos/{repo}/experiments', call: () => api.listExperiments('r') },
    { name: 'listLenses → /lenses', spec: '/lenses', call: () => api.listLenses() },
    { name: 'getLens → /lenses/{lens}', spec: '/lenses/{lens}', call: () => api.getLens('eng') },
    { name: 'status → /repos/{repo}/branches/{branch}', spec: '/repos/{repo}/branches/{branch}', call: () => api.status('r', 'agent/x') },
  ];
  // The spec route as a full-path pattern under /api/v1: each {param} is one
  // non-empty segment. A row whose call hits another route fails here, so the
  // query check below is made against the route actually requested.
  const routePattern = (spec: string) =>
    new RegExp('^/api/v1' + spec.split(/\{[^}]+\}/).map(s => s.replace(/[.*+?^$()|[\]\\]/g, '\\$&')).join('[^/]+') + '$');
  for (const c of cases) {
    it(c.name, async () => {
      const f = mockFetch();
      await c.call().catch(() => undefined);
      expect(f).toHaveBeenCalled();
      const declared = specQueryParams(c.spec);
      for (let i = 0; i < f.mock.calls.length; i++) {
        const u = new URL(String(f.mock.calls[i][0]), 'http://x');
        expect(u.pathname).toMatch(routePattern(c.spec));
        const sent = [...new Set([...u.searchParams.keys()])];
        expect(undeclared(sent, declared)).toEqual([]);
      }
    });
  }

  // subscribeRepoEvents opens an EventSource (no fetch to capture): the URL it
  // opens is checked against /repo-events directly.
  it('subscribeRepoEvents → /repo-events', () => {
    const urls: string[] = [];
    class FakeEventSource {
      static CLOSED = 2;
      readyState = 0;
      onopen = null;
      onmessage = null;
      onerror = null;
      url: string;
      constructor(url: string) { this.url = url; urls.push(url); }
      addEventListener() {}
      removeEventListener() {}
      close() {}
    }
    const prev = globalThis.EventSource;
    globalThis.EventSource = FakeEventSource as unknown as typeof EventSource;
    try {
      subscribeRepoEvents(() => {})();
    } finally {
      globalThis.EventSource = prev;
    }
    expect(urls).toHaveLength(1);
    const u = new URL(urls[0], 'http://x');
    expect(u.pathname).toBe('/api/v1/repo-events');
    expect(undeclared([...u.searchParams.keys()], specQueryParams('/repo-events'))).toEqual([]);
  });
});
