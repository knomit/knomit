import { describe, it, expect, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { api } from './api';
import type { ListFilters } from './listSearch';

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
  for (const l of block.slice(getAt)) {
    if (/^ {4}\S/.test(l) && l !== '    get:') break; // the next method
    const inline = l.match(/^\s+- \{ name: ([^,\s]+),\s+in: query/);
    if (inline) { names.push(inline[1]); continue; }
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
    expect(specQueryParams('/repos/{repo}/branches/{branch}/search')).not.toContain('offset');
  });

  it('api.search → repo /search', async () => {
    const f = mockFetch();
    await api.search('r', 'b', 'text domain:x entity:y', 'kb/x/', 0.5, EVERY);
    const sent = sentNames(f);
    expect(sent).toContain('since_fork');
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
    await api.lensSearch('eng', 'text', ['alpha'], { ...EVERY, path: 'kb/x/' });
    const sent = sentNames(f);
    expect(sent).toContain('repo');
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
