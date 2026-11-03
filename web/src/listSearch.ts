// The search a results list sends, built in ONE place: the repo lists (api.search,
// api.recent), the lens lists (lensSearch, listLensFacts) and the FactSet of a
// list (factsetApi.first) all read these builders, so the FactSet's first call
// is the list's own request by construction. Kept out of api.ts so the many
// tests that replace './api' with a mock still build the same parameters.

import type { MotifMatch } from './api';

export function parseSearchQuery(raw: string): { text: string; domains: string[]; entities: string[] } {
  const domains: string[] = [];
  const entities: string[] = [];
  const textTokens: string[] = [];

  // Extract prefix:"quoted value" patterns first (e.g. entity:"Composer 2").
  const quoted: string[] = [];
  const stripped = raw.replace(/(domain|entity):"([^"]+)"/g, (_m, prefix, value) => {
    if (prefix === 'domain') domains.push(value);
    else entities.push(value);
    return '';
  }).replace(/"([^"]+)"/g, (_m, g) => { quoted.push(g); return ''; });

  for (const token of stripped.trim().split(/\s+/)) {
    if (!token) continue;
    if (token.startsWith('domain:')) { const v = token.slice(7); if (v) domains.push(v); }
    else if (token.startsWith('entity:')) { const v = token.slice(7); if (v) entities.push(v); }
    else textTokens.push(token);
  }

  // Combine quoted phrases and remaining free text
  const allText = [...quoted, ...textTokens].join(' ').trim();
  return { text: allText, domains, entities };
}


export function setMotifParams(p: URLSearchParams, opts?: { motifs?: string[]; motifMatch?: MotifMatch }): void {
  if (!opts?.motifs?.length) return;
  p.set('motifs', opts.motifs.join(','));
  if (opts.motifMatch && opts.motifMatch !== 'exact') p.set('motif_match', opts.motifMatch);
}

/** The content filters a results list sends, in the app's own names. */
export interface ListFilters {
  types?: string[]; excludeType?: string; kinds?: string[]; excludeKinds?: string[]; origins?: string[];
  eps?: string[]; domains?: string[]; entities?: string[];
  motifs?: string[]; motifMatch?: MotifMatch; sinceFork?: boolean;
}

/** Which list: 'relevance' (repo /search, lens /search) or 'recent' (the
 *  /facts collections, recency-ordered). */
export type ListKind = 'relevance' | 'recent';

/**
 * listSearchParams is the ONE builder of a repo results list's search: exactly
 * the parameters the list sends, minus the list's own paging (sort, offset,
 * limit). api.search and api.recent send it plus their paging, and a FactSet of
 * the list sends it plus `sort` and its own options, so "the FactSet IS the
 * list" holds by construction on the client, as the /search routes' one ranked
 * list (paged by offset, as deep as a FactSet goes) makes it hold on the server.
 */
export function listSearchParams(kind: ListKind, q: string, path: string, minConfidence: number, opts?: ListFilters): URLSearchParams {
  if (kind === 'relevance') {
    const { text, domains, entities } = parseSearchQuery(q);
    const allDomains = [...domains, ...(opts?.domains || [])];
    const allEntities = [...entities, ...(opts?.entities || [])];
    const p = new URLSearchParams();
    if (text) p.set('q', text);
    if (allDomains.length) p.set('domain', allDomains.join(','));
    if (allEntities.length) p.set('entities', allEntities.join(','));
    if (path) p.set('path', path);
    if (minConfidence) p.set('min_confidence', String(minConfidence));
    if (opts?.types?.length) p.set('type', opts.types.join(','));
    if (opts?.kinds?.length) p.set('kind', opts.kinds.join(','));
    if (opts?.excludeKinds?.length) p.set('exclude_kind', opts.excludeKinds.join(','));
    if (opts?.origins?.length) p.set('origin', opts.origins.join(','));
    if (opts?.eps?.length) p.set('ep', opts.eps.join(','));
    // since_fork is only ever sent from inside an experiment. The server
    // answers 400 off one rather than ignoring it, so this must not be set
    // speculatively — the caller's own state decides, not this function.
    if (opts?.sinceFork) p.set('since_fork', '1');
    setMotifParams(p, opts);
    return p;
  }
  const p = new URLSearchParams({ path });
  if (q) p.set('q', q);
  // CSV, like every other multi-value facet here: `type` is OR-combined
  // server-side (splitCSV → SearchOptions.IncludeTypes), so a second type chip
  // must widen the match, not be dropped. This took a single `typeFilter`
  // string until the chips stopped routing through /search, at which point two
  // chips collapsed to undefined and silently removed all type filtering.
  if (opts?.types?.length) p.set('type', opts.types.join(','));
  if (opts?.excludeType) p.set('exclude_type', opts.excludeType);
  if (opts?.kinds?.length) p.set('kind', opts.kinds.join(','));
  if (opts?.excludeKinds?.length) p.set('exclude_kind', opts.excludeKinds.join(','));
  if (opts?.origins?.length) p.set('origin', opts.origins.join(','));
  if (opts?.domains?.length) p.set('domain', opts.domains.join(','));
  if (opts?.entities?.length) p.set('entities', opts.entities.join(','));
  if (opts?.eps?.length) p.set('ep', opts.eps.join(','));
  if (opts?.sinceFork) p.set('since_fork', '1');
  setMotifParams(p, opts);
  return p;
}

/**
 * lensListSearchParams is listSearchParams for a lens results list: the lens
 * /search (relevance) or the lens /facts (recent) parameters, with the list's
 * repo= narrowing, minus paging. The text goes as `q` on both (the lens /facts
 * reads `query` or `q`), the name a FactSet takes.
 */
export function lensListSearchParams(kind: ListKind, q: string, repos: string[] | undefined, opts?: ListFilters & { path?: string }): URLSearchParams {
  const p = new URLSearchParams();
  if (kind === 'relevance') {
    if (q) p.set('q', q);
    if (opts?.path) p.set('path', opts.path);
  } else {
    if (opts?.path) p.set('path', opts.path);
    if (q) p.set('q', q);
  }
  if (opts?.types?.length) p.set('type', opts.types.join(','));
  if (opts?.kinds?.length) p.set('kind', opts.kinds.join(','));
  if (opts?.origins?.length) p.set('origin', opts.origins.join(','));
  if (opts?.eps?.length) p.set('ep', opts.eps.join(','));
  if (opts?.domains?.length) p.set('domain', opts.domains.join(','));
  if (opts?.entities?.length) p.set('entities', opts.entities.join(','));
  setMotifParams(p, opts);
  for (const repo of repos ?? []) p.append('repo', repo);
  return p;
}

