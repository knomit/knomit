function encodeBranch(name: string): string {
  return name.replaceAll('/', ':');
}

// fetchJSON wraps fetch with a uniform error-on-non-2xx guarantee. Until this
// helper landed, most api.* functions called `.then(r => r.json())` directly,
// which silently parsed problem+json bodies as success and returned `{}` /
// missing fields to callers — backend 500s became indistinguishable from
// "no results" in the UI.
async function fetchJSON<T = unknown>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, init);
  if (!r.ok) {
    // Best-effort: surface a problem+json `title`/`detail` if present.
    let detail = r.statusText;
    try {
      const body = await r.json();
      detail = body?.detail || body?.title || body?.error || detail;
    } catch {
      // Non-JSON body; keep the statusText.
    }
    throw new Error(`${url} → ${r.status} ${detail}`);
  }
  return r.json() as Promise<T>;
}

function repoBase(repo: string): string {
  return `/api/v1/repos/${repo}`;
}

function branchBase(repo: string, branch: string): string {
  return `${repoBase(repo)}/branches/${encodeBranch(branch)}`;
}

export interface RepoInfo { name: string }

export interface DirChild { name: string; is_dir: boolean; type?: string; title?: string; fullPath?: string }
export interface BrowseResponse { path: string; children: DirChild[] }
export interface Fact { path: string; title: string; kind?: string; type?: string; body: string; domain: string[]; confidence: number; sources: number; entities: string[]; refs: string[]; parse_error?: string; from_commit?: string; commit_hash?: string; commit_date?: string }

// normalizeFactResponse maps the new HAL FactView shape to the Fact interface.
// The new API returns refs as [{raw, kind, _links}] and uses as_of.commit
// instead of commit_hash; this normalizer keeps component code unchanged.
function normalizeFactResponse(data: any): Fact {
  let refs: string[] = [];
  if (Array.isArray(data.refs)) {
    refs = data.refs.map((r: any) => (typeof r === 'string' ? r : r.raw));
  }
  return {
    path: data.path,
    title: data.title,
    kind: data.kind,
    type: data.type,
    body: data.body,
    domain: data.domain || [],
    confidence: data.confidence,
    sources: data.sources,
    entities: data.entities || [],
    refs,
    parse_error: data.parse_error,
    from_commit: data.from_commit,
    commit_hash: data.commit_hash ?? data.as_of?.commit,
    commit_date: data.commit_date ?? data.as_of?.date,
  };
}
export interface SearchResult { path: string; title: string; body: string; score: number; kind?: string; type?: string; domain?: string[]; entities?: string[] }
export interface HistoryEntry { commit: string; date: string; message: string }
export interface FileCounts { added?: number; modified?: number; deleted?: number }
export interface HistoryEntryWithTags { commit: string; date: string; message: string; operation?: string; files?: FileCounts }
export interface HistoryResponse { entries: HistoryEntryWithTags[]; next?: string; prev?: string }
export interface RecentFactEntry { path: string; title: string; kind?: string; type?: string; committed_at: number; operation?: string; score?: number }
export interface RecentResponse { facts: RecentFactEntry[]; total: number }
export interface CommitFile { path: string; action: string; title?: string }
export interface CommitDetail { commit: string; date: string; message: string; operation?: string; files: CommitFile[] }
export interface Stats { total: number; domains: Record<string, number>; entities: Record<string, number>; avg_confidence: number }
export interface Status { head: string; branch: string; index_commit: string; embeddings_enabled: boolean; ontology_root: string }
export interface ActivityStats { last_commit: string; total: number; changes_7d: number; changes_30d: number; changes_90d: number }

export interface OriginResponse {
  name: string;
  url: string;
  branch: string;
  interval: number;
  last_sync_at: string | null;
  last_status: string | null;
  last_error: string | null;
  push_interval: number;
  last_push_at: string | null;
  last_push_status: string | null;
  last_push_error: string | null;
  auth_method: string;
}

export interface OriginSetResponse {
  status: string;
  branch: string;
  head: string;
}

export interface RefVersion { commit: string; committed_at?: number; deleted?: boolean; kind?: string; type?: string }
export interface RefGroup {
  path: string;
  title: string;
  kind?: string;            // kind of the latest version
  type?: string;            // type of the latest version (UI uses this for chip color)
  versions: RefVersion[];   // newest-first
  deleted?: boolean;        // true if the latest version is deleted (target retracted)
}

import type { FilterChip, AsOf } from './state';

// parseSearchQuery splits a query string into structured components.
// Tokens of the form domain:X or entity:X are extracted as filters;
// quoted strings (e.g. entity:"Composer 2") are extracted as filter values;
// bare quoted strings are extracted as similarity text;
// the remaining words are treated as free text for embeddings.
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

const SHORT_SHA = /^[0-9a-f]{7}$/i;

function parseAnchorToken(prefix: 'at' | 'vs', value: string, lookupHead?: () => string): AsOf | undefined {
  const v = value.trim();
  if (!v) return undefined;
  if (prefix === 'at') {
    // at:HEAD
    if (v === 'HEAD') return { mode: 'live' };
    // at:<7-char-sha>
    if (SHORT_SHA.test(v)) return { mode: 'scrubbed', commit: v.toLowerCase() };
    return undefined;
  }
  // vs:<from>..<to>
  const m = v.match(/^([0-9a-fA-F]{7}|HEAD)\.\.([0-9a-fA-F]{7}|HEAD)$/);
  if (m) {
    const from = m[1] === 'HEAD' ? (lookupHead?.() ?? '') : m[1].toLowerCase();
    const to   = m[2] === 'HEAD' ? (lookupHead?.() ?? '') : m[2].toLowerCase();
    if (from && to) return { mode: 'diff', from, to };
  }
  return undefined;
}

export function parseFilterQuery(raw: string, lookupHead?: () => string): { chips: FilterChip[]; text: string; asOf?: AsOf; warnings: string[] } {
  const chips: FilterChip[] = [];
  let asOf: AsOf | undefined;
  const warnings: string[] = [];

  // Extract at:VALUE and vs:VALUE first — anchor tokens are side-channel, not chips.
  let remaining = raw.replace(/(at|vs):"([^"]+)"/g, (_m, prefix, value) => {
    const result = parseAnchorToken(prefix as 'at' | 'vs', value, lookupHead);
    if (result) asOf = result;
    else warnings.push(`invalid ${prefix}: token "${value}"`);
    return '';
  });
  remaining = remaining.replace(/(at|vs):(\S+)/g, (_m, prefix, value) => {
    const result = parseAnchorToken(prefix as 'at' | 'vs', value, lookupHead);
    if (result) asOf = result;
    else warnings.push(`invalid ${prefix}: token "${value}"`);
    return '';
  });

  // Extract prefix:"quoted value" patterns first
  remaining = remaining.replace(/(domain|entity|type|kind|ep|path):"([^"]+)"/g, (_m, prefix, value) => {
    chips.push({ category: prefix as FilterChip['category'], value });
    return '';
  });
  // Extract prefix:value patterns (no quotes, no spaces)
  remaining = remaining.replace(/(domain|entity|type|kind|ep|path):(\S+)/g, (_m, prefix, value) => {
    chips.push({ category: prefix as FilterChip['category'], value });
    return '';
  });
  return { chips, text: remaining.trim(), asOf, warnings };
}

export interface SessionCreateResponse {
  session_id: string;
}

export interface TestResult {
  branches: string[];
  agent_branches: string[];
  default_branch: string;
  matched_agent?: string;
  history: "disjoint" | "shared";
  remote_fact_count: number;
  local_fact_count: number;
}

export interface PreviewResult {
  local_only: number;
  remote_only: number;
  shared_path: number;
  dead_refs_found: number;
}

export interface ApplyResult {
  total_facts: number;
  from_local: number;
  from_remote: number;
  overwrites: number;
  refs_resolved_from_history: number;
  dangling_refs_dropped: number;
}

export type SSEEvent =
  | { phase: "connecting" }
  | { phase: "cloning"; progress?: string }
  | { phase: "analyzing" }
  | { phase: "comparing" }
  | { phase: "replaying"; current: number; total: number }
  | { phase: "merging" }
  | { phase: "swapping" }
  | { phase: "configuring" }
  | { phase: "rebuilding"; sub_phase?: string; current?: number; total?: number }
  | { phase: "done"; result: any }
  | { phase: "error"; message: string };

function sessionBase(repo: string, sessionId: string) {
  return `${repoBase(repo)}/origin-sessions/${sessionId}`;
}

function parseSSELines(text: string): SSEEvent[] {
  const events: SSEEvent[] = [];
  for (const line of text.split('\n')) {
    const trimmed = line.trim();
    if (trimmed.startsWith('data: ')) {
      try { events.push(JSON.parse(trimmed.slice(6))); } catch {}
    }
  }
  return events;
}

async function readSSEStream(res: Response, onEvent?: (e: SSEEvent) => void): Promise<void> {
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error || res.statusText);
  }
  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const events = parseSSELines(buf);
    buf = buf.includes('\n') ? buf.slice(buf.lastIndexOf('\n') + 1) : '';
    for (const ev of events) onEvent?.(ev);
  }
}

export function createSession(repo: string, opts: { url: string; auth_method?: string; token?: string; user?: string; password?: string }): Promise<SessionCreateResponse> {
  return fetch(`${repoBase(repo)}/origin-sessions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(opts),
  }).then(r => { if (!r.ok) return r.json().then(e => { throw new Error(e.error || r.statusText); }); return r.json(); });
}

export function streamTest(repo: string, sessionId: string, onEvent: (e: SSEEvent) => void): () => void {
  const es = new EventSource(`${sessionBase(repo, sessionId)}/test`);
  es.onmessage = (e) => { try { onEvent(JSON.parse(e.data)); } catch {} };
  es.onerror = () => { es.close(); };
  return () => es.close();
}

export function streamPreview(repo: string, sessionId: string, onEvent: (e: SSEEvent) => void): () => void {
  const es = new EventSource(`${sessionBase(repo, sessionId)}/preview`);
  es.onmessage = (e) => { try { onEvent(JSON.parse(e.data)); } catch {} };
  es.onerror = () => { es.close(); };
  return () => es.close();
}

export async function streamApply(repo: string, sessionId: string, strategy: string, branch?: string, onEvent?: (e: SSEEvent) => void): Promise<void> {
  const body: Record<string, string> = { conflict_strategy: strategy };
  if (branch) body.branch = branch;
  const res = await fetch(`${sessionBase(repo, sessionId)}/apply`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  await readSSEStream(res, onEvent);
}

export async function streamCommit(repo: string, sessionId: string, onEvent: (e: SSEEvent) => void): Promise<void> {
  const res = await fetch(`${sessionBase(repo, sessionId)}/commit`, { method: 'POST' });
  await readSSEStream(res, onEvent);
}

export function deleteSession(repo: string, sessionId: string): Promise<void> {
  return fetch(`${sessionBase(repo, sessionId)}`, { method: 'DELETE' }).then(r => { if (!r.ok) throw new Error(r.statusText); });
}

// stripOntologyRoot removes the ontology root prefix from a full path.
// e.g. ontologyRoot="kb", path="kb/ai/ml" -> "ai/ml"
//      ontologyRoot="kb", path="kb"        -> ""  (root)
function stripOntologyRoot(ontologyRoot: string, path: string): string {
  if (path === ontologyRoot) return '';
  const prefix = ontologyRoot + '/';
  if (path.startsWith(prefix)) return path.slice(prefix.length);
  return path;
}

// getAgentBranch fetches the branches list for a repo and returns the agent
// branch name. It picks the first branch whose name starts with "machine/"
// (the knomit machine-branch convention), falling back to the first branch.
async function getAgentBranch(repo: string): Promise<string> {
  const data = await fetchJSON<any>(`${repoBase(repo)}/branches`);
  const branches: Array<{ name: string }> =
    (data._embedded?.branches as Array<{ name: string }>) || [];
  const agent = branches.find(b => b.name.startsWith('agent/'));
  const main = branches.find(b => b.name === 'main');
  return (agent || main || branches[0])?.name || 'main';
}

export const api = {
  getAgentBranch,

  repos: (): Promise<RepoInfo[]> =>
    fetchJSON<any>('/api/v1/repos').then(data => {
      // New endpoint returns HAL: {count, _links, _embedded: {repos: [{name, _links}]}}
      if (data && data._embedded && Array.isArray(data._embedded.repos)) {
        return data._embedded.repos as RepoInfo[];
      }
      // Fallback: flat array (legacy)
      return Array.isArray(data) ? data : [];
    }),

  browse: (repo: string, branch: string, path: string, ontologyRoot: string): Promise<BrowseResponse> => {
    const relative = stripOntologyRoot(ontologyRoot, path);
    const url = relative
      ? `${branchBase(repo, branch)}/topics/${relative}`
      : `${branchBase(repo, branch)}/topics`;
    return fetchJSON<any>(url).then(data => {
      // HAL: {_embedded: {topics: [{name, is_dir, type?, title?, _links}]}}
      const items: DirChild[] = ((data._embedded?.topics as any[]) || []).map((e: any) => ({
        name: e.name,
        is_dir: e.is_dir,
        type: e.type,
        title: e.title,
        fullPath: e.is_dir ? undefined : (relative ? `${ontologyRoot}/${relative}/${e.name}` : `${ontologyRoot}/${e.name}`),
      }));
      return { path, children: items };
    });
  },

  fact: (repo: string, branch: string, path: string, commit?: string, opts?: { fallback?: 'before' }): Promise<Fact> => {
    const base = commit
      ? `${branchBase(repo, branch)}/commits/${commit}/facts/${path}`
      : `${branchBase(repo, branch)}/facts/${path}`;
    // ?fallback=before is only meaningful for commit-anchored reads (a HEAD
    // read either finds the fact or it doesn't — there's no prior version
    // to fall back to). Skip the parameter for HEAD reads.
    const query = (commit && opts?.fallback === 'before') ? '?fallback=before' : '';
    return fetchJSON<any>(base + query).then(normalizeFactResponse);
  },

  search: (repo: string, branch: string, q: string, path = '', minConfidence = 0,
    opts?: { types?: string[]; kinds?: string[]; excludeKinds?: string[]; eps?: string[]; domains?: string[]; entities?: string[] }
  ): Promise<{ results: SearchResult[] }> => {
    const { text, domains, entities } = parseSearchQuery(q);
    const allDomains = [...domains, ...(opts?.domains || [])];
    const allEntities = [...entities, ...(opts?.entities || [])];
    const p = new URLSearchParams({ limit: '50' });
    if (text) p.set('q', text);
    if (allDomains.length) p.set('domain', allDomains.join(','));
    if (allEntities.length) p.set('entities', allEntities.join(','));
    if (path) p.set('path', path);
    if (minConfidence) p.set('min_confidence', String(minConfidence));
    if (opts?.types?.length) p.set('type', opts.types.join(','));
    if (opts?.kinds?.length) p.set('kind', opts.kinds.join(','));
    if (opts?.excludeKinds?.length) p.set('exclude_kind', opts.excludeKinds.join(','));
    if (opts?.eps?.length) p.set('ep', opts.eps.join(','));
    return fetchJSON<any>(`${branchBase(repo, branch)}/search?${p}`).then(data => ({
      // HAL CollectionView: {_embedded: {results: [...]}}
      results: data._embedded?.results || data.results || [],
    }));
  },

  factCommits: (repo: string, branch: string, path: string, after?: string, from?: string, before?: string): Promise<HistoryResponse> => {
    const p = new URLSearchParams({ limit: '50' });
    if (after) p.set('after', after);
    if (from) p.set('from', from);
    if (before) p.set('before', before);
    return fetchJSON<any>(`${branchBase(repo, branch)}/facts/${path}/commits?${p}`).then(data => {
      // HAL CollectionView: {count, _links: {next?, prev?}, _embedded: {commits: [...]}}
      const entries: HistoryEntryWithTags[] = data._embedded?.commits || data.entries || [];
      // Extract next/prev cursor from _links href query params
      const nextLink: string | undefined = data._links?.next?.href;
      const prevLink: string | undefined = data._links?.prev?.href;
      const extractAfter = (href: string | undefined): string | undefined => {
        if (!href) return undefined;
        try { return new URL(href, 'http://x').searchParams.get('after') ?? undefined; } catch { return undefined; }
      };
      return {
        entries,
        next: extractAfter(nextLink),
        prev: extractAfter(prevLink),
      };
    });
  },

  commitDetail: (repo: string, branch: string, hash: string): Promise<CommitDetail> =>
    fetchJSON<CommitDetail>(`${branchBase(repo, branch)}/commits/${hash}`),

  updateFact: (repo: string, branch: string, path: string, content: string): Promise<Fact> =>
    fetchJSON<any>(`${branchBase(repo, branch)}/facts/${path}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content }),
    }).then(normalizeFactResponse),

  stats: (repo: string, branch: string, path: string): Promise<Stats> =>
    fetchJSON<Stats>(`${branchBase(repo, branch)}/stats?path=${encodeURIComponent(path)}`),

  activity: (repo: string, branch: string, path: string): Promise<ActivityStats> =>
    fetchJSON<ActivityStats>(`${branchBase(repo, branch)}/activity?path=${encodeURIComponent(path)}`),

  status: (repo: string, branch: string): Promise<Status> =>
    fetchJSON<any>(`${branchBase(repo, branch)}`).then(data => ({
      head: data.head,
      branch: branch,
      index_commit: data.index_commit,
      embeddings_enabled: data.embeddings_enabled,
      // ontology_root not in new response — caller preserves existing state value
      ontology_root: data.ontology_root || '',
    })),

  synthesize: (repo: string, branch: string, recipe = ''): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetchJSON(`${branchBase(repo, branch)}/synthesis-runs`, { method: 'POST', body: recipe }),

  rebuild: (repo: string, branch: string): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetchJSON(`${branchBase(repo, branch)}/index-rebuilds`, { method: 'POST' }),

  recent: (repo: string, branch: string, path: string, query = '', limit = 50, offset = 0,
    opts?: { typeFilter?: string; excludeType?: string; kinds?: string[]; excludeKinds?: string[]; domains?: string[]; entities?: string[]; eps?: string[] }
  ): Promise<RecentResponse> => {
    const p = new URLSearchParams({ sort: 'recent', path, limit: String(limit), offset: String(offset) });
    if (query) p.set('q', query);
    if (opts?.typeFilter) p.set('type', opts.typeFilter);
    if (opts?.excludeType) p.set('exclude_type', opts.excludeType);
    if (opts?.kinds?.length) p.set('kind', opts.kinds.join(','));
    if (opts?.excludeKinds?.length) p.set('exclude_kind', opts.excludeKinds.join(','));
    if (opts?.domains?.length) p.set('domain', opts.domains.join(','));
    if (opts?.entities?.length) p.set('entities', opts.entities.join(','));
    if (opts?.eps?.length) p.set('ep', opts.eps.join(','));
    return fetchJSON<any>(`${branchBase(repo, branch)}/facts?${p}`).then(data => ({
      // HAL CollectionView: count = total, _embedded.facts = items
      facts: data._embedded?.facts || data.facts || [],
      total: data.count ?? data.total ?? 0,
    }));
  },

  getOrigin: (repo: string): Promise<OriginResponse | null> =>
    fetch(`${repoBase(repo)}/origin`).then(r => r.status === 204 ? null : r.json()),

  setOrigin: (repo: string, opts: { url?: string; auth_method?: string; token?: string; user?: string; password?: string }): Promise<OriginSetResponse> =>
    fetch(`${repoBase(repo)}/origin`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(opts),
    }).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); }),

  retractFact: (repo: string, branch: string, path: string): Promise<void> =>
    fetch(`${branchBase(repo, branch)}/facts/${path}`, { method: 'DELETE' })
      .then(r => { if (!r.ok) return r.json().then(e => { throw new Error(e.title || e.detail || r.statusText); }); }),

  completions: (repo: string, branch: string, category: string, prefix = ''): Promise<{ values: string[] }> =>
    fetchJSON(`${branchBase(repo, branch)}/completions?category=${encodeURIComponent(category)}&prefix=${encodeURIComponent(prefix)}`),

  factDiff: async (
    repo: string, branch: string, path: string,
    from: string, to: string,
    signal?: AbortSignal,
  ): Promise<{ from: Fact | null; to: Fact | null }> => {
    const fetchSide = async (commit: string): Promise<Fact | null> => {
      const url = `${branchBase(repo, branch)}/commits/${commit}/facts/${path}`;
      const res = await fetch(url, { signal });
      if (res.status === 404) return null;
      if (!res.ok) throw new Error(res.statusText);
      return normalizeFactResponse(await res.json());
    };
    const [fromFact, toFact] = await Promise.all([fetchSide(from), fetchSide(to)]);
    return { from: fromFact, to: toFact };
  },

  explain: (repo: string, branch: string, path: string, commit?: string): Promise<{
    incoming: RefGroup[];
    outgoing: RefGroup[];
  }> => {
    // When commit is set, use the commit-anchored sub-resource endpoints so
    // refs reflect the state of the source/target at that commit (the
    // commit-anchored handler dispatches /incoming and /outgoing to the
    // *AtCommit store primitives). Without this, navigating to a specific
    // version of a fact in the Explain view would show no refs.
    const factURL = commit
      ? `${branchBase(repo, branch)}/commits/${commit}/facts/${path}`
      : `${branchBase(repo, branch)}/facts/${path}`;
    type RawRef = { path: string; title: string; kind?: string; type?: string; commit?: string; committed_at?: number; deleted?: boolean };
    const parseRefs = (data: any): RawRef[] => {
      // HAL CollectionView: {_embedded: {refs: [...]}}
      // Each ref carries a `commit` field pinning it to a specific version:
      // source_commit for /incoming, target_commit for /outgoing. The
      // `deleted` flag (outgoing-only) marks tombstoned targets — those
      // entries omit _links.self.
      if (data && data._embedded && Array.isArray(data._embedded.refs)) {
        return data._embedded.refs;
      }
      // Fallback: flat array
      return Array.isArray(data) ? data : [];
    };
    // groupRefs collapses ref-events that share a `path` into a single
    // RefGroup with versions[] ordered newest-first by committed_at.
    // Same source path with different source_commits = different versions
    // of the source asserting the same target — multi-edges are intentional
    // (see internal/store/edge_props.go:11). Grouping de-dupes them in the UI.
    const groupRefs = (refs: RawRef[]): RefGroup[] => {
      const order: string[] = [];
      type Pending = { path: string; entries: { ref: RawRef; ord: number }[] };
      const groups = new Map<string, Pending>();
      refs.forEach((r, idx) => {
        const key = r.path;
        let g = groups.get(key);
        if (!g) {
          g = { path: r.path, entries: [] };
          groups.set(key, g);
          order.push(key);
        }
        g.entries.push({ ref: r, ord: idx });
      });
      return order.map(key => {
        const g = groups.get(key)!;
        // Sort entries newest-first by committed_at; fall back to backend
        // insertion order when committed_at is missing on either side.
        const sorted = [...g.entries].sort((a, b) => {
          const at = a.ref.committed_at;
          const bt = b.ref.committed_at;
          if (at != null && bt != null && at !== bt) return bt - at;
          return a.ord - b.ord;
        });
        const versions: RefVersion[] = sorted.map(e => ({
          commit: e.ref.commit ?? '',
          committed_at: e.ref.committed_at,
          deleted: e.ref.deleted,
          kind: e.ref.kind,
          type: e.ref.type,
        }));
        const latestRef = sorted[0]?.ref;
        return {
          path: g.path,
          title: latestRef?.title ?? '',
          kind: latestRef?.kind,
          type: latestRef?.type,
          versions,
          deleted: latestRef?.deleted ?? false,
        };
      });
    };
    return Promise.all([
      fetch(`${factURL}/incoming`).then(r => r.ok ? r.json() : r.json().then((e: { error: string }) => { throw new Error(e.error || r.statusText); })),
      fetch(`${factURL}/outgoing`).then(r => r.ok ? r.json() : r.json().then((e: { error: string }) => { throw new Error(e.error || r.statusText); })),
    ]).then(([inc, out]) => ({
      incoming: groupRefs(parseRefs(inc)),
      outgoing: groupRefs(parseRefs(out)),
    }));
  },
};
