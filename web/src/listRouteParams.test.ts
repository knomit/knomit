import { describe, it, expect } from 'vitest';
import { listSearchParams, lensListSearchParams } from './listSearch';
import type { ListFilters } from './listSearch';

// The four REST list routes answer 400 to a query parameter they do not
// declare (internal/edge/web/api/kb/params_declared.go, pinned there against
// openapi.yaml). These are those declared sets; every parameter the results
// lists' builders can send, with every option set, must be in its route's set,
// or a chip click becomes a 400.
const DECLARED = {
  repoSearch: ['q', 'entities', 'domain', 'domain_exact', 'path', 'min_confidence', 'min_similarity', 'limit',
    'type', 'exclude_type', 'kind', 'exclude_kind', 'origin', 'ep', 'motifs', 'motif_match',
    'expired', 'expires_before', 'expires_after', 'since_fork'],
  repoFacts: ['limit', 'offset', 'path', 'topic', 'q', 'domain', 'entity', 'entities',
    'type', 'exclude_type', 'kind', 'exclude_kind', 'origin', 'ep', 'motifs', 'motif_match',
    'expired', 'expires_before', 'expires_after', 'min_confidence', 'since_fork', 'sort'],
  lensSearch: ['q', 'limit', 'path', 'repo', 'entity', 'entities', 'domain', 'domain_exact', 'applies_to',
    'type', 'exclude_type', 'kind', 'exclude_kind', 'origin', 'ep', 'min_confidence', 'min_similarity',
    'motifs', 'motif_match', 'expired', 'expires_before', 'expires_after'],
  lensFacts: ['limit', 'offset', 'query', 'q', 'path', 'topic', 'repo', 'domain', 'domain_exact', 'applies_to',
    'entity', 'entities', 'type', 'exclude_type', 'kind', 'exclude_kind', 'origin', 'ep',
    'min_confidence', 'min_similarity', 'motifs', 'motif_match', 'expired', 'expires_before', 'expires_after'],
};

const EVERY: ListFilters & { path?: string } = {
  types: ['observation'], excludeType: 'policy', kinds: ['epistemic'], excludeKinds: ['pragmatic'],
  origins: ['authored'], eps: ['learn'], domains: ['d'], entities: ['E'],
  motifs: ['a-b'], motifMatch: 'stem', sinceFork: true, path: 'kb/x/',
};

const keys = (p: URLSearchParams) => [...new Set([...p.keys()])].sort();

// Sabotage: make a builder send a name no route declares (rename `ep` to
// `eps` in listSearchParams) and its rows list the stray name.
describe('the results lists send only declared parameters', () => {
  it('repo /search (relevance list)', () => {
    const p = listSearchParams('relevance', 'text domain:x entity:y', 'kb/x/', 0.5, EVERY);
    p.set('limit', '50');
    expect(keys(p).filter(k => !DECLARED.repoSearch.includes(k))).toEqual([]);
    expect(keys(p)).toContain('since_fork');
  });
  it('repo /facts (recent list)', () => {
    const p = listSearchParams('recent', 'text', 'kb/x/', 0, EVERY);
    p.set('sort', 'recent'); p.set('offset', '0'); p.set('limit', '50'); // api.recent's paging
    expect(keys(p).filter(k => !DECLARED.repoFacts.includes(k))).toEqual([]);
    expect(keys(p)).toContain('exclude_type');
  });
  it('lens /search and /facts', () => {
    const s = lensListSearchParams('relevance', 'text', ['alpha'], EVERY);
    expect(keys(s).filter(k => !DECLARED.lensSearch.includes(k))).toEqual([]);
    const f = lensListSearchParams('recent', 'text', ['alpha'], EVERY);
    f.set('offset', '0'); f.set('limit', '50');
    expect(keys(f).filter(k => !DECLARED.lensFacts.includes(k))).toEqual([]);
    expect(keys(f)).toContain('repo');
  });
});
