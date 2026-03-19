function base(repo: string) { return `/api/v1/${repo}`; }

export interface RepoInfo { name: string; branch: string }

export interface DirChild { name: string; is_dir: boolean }
export interface BrowseResponse { path: string; children: DirChild[] }
export interface Fact { path: string; title: string; type?: string; body: string; domain: string[]; confidence: number; sources: number; entities: string[]; refs: string[]; parse_error?: string; from_commit?: string; commit_hash?: string; commit_date?: string }
export interface SearchResult { path: string; title: string; body: string; score: number; domain?: string[]; entities?: string[] }
export interface HistoryEntry { commit: string; date: string; message: string }
export interface FileCounts { added?: number; modified?: number; deleted?: number }
export interface HistoryEntryWithTags { commit: string; date: string; message: string; operation?: string; files?: FileCounts }
export interface HistoryResponse { entries: HistoryEntryWithTags[]; next?: string }
export interface RecentFactEntry { path: string; title: string; committed_at: number }
export interface RecentResponse { facts: RecentFactEntry[]; total: number }
export interface CommitFile { path: string; action: string }
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

// parseSearchQuery splits a query string into structured components.
// Tokens of the form domain:X or entity:X are extracted as filters;
// quoted strings (e.g. "some phrase") are extracted as similarity text;
// the remaining words are treated as free text for embeddings.
export function parseSearchQuery(raw: string): { text: string; domains: string[]; entities: string[] } {
  const domains: string[] = [];
  const entities: string[] = [];
  const textTokens: string[] = [];

  // Extract quoted strings first, then process remaining tokens
  const quoted: string[] = [];
  const stripped = raw.replace(/"([^"]+)"/g, (_m, g) => { quoted.push(g); return ''; });

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

export const api = {
  repos: (): Promise<RepoInfo[]> => fetch('/api/v1/repos').then(r => r.json()),
  browse: (repo: string, path: string): Promise<BrowseResponse> => fetch(`${base(repo)}/browse?path=${encodeURIComponent(path)}`).then(r => r.json()),
  fact: (repo: string, path: string, commit?: string): Promise<Fact> => {
    const p = new URLSearchParams({ path });
    if (commit) p.set('commit', commit);
    return fetch(`${base(repo)}/fact?${p}`).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); });
  },
  search: (repo: string, q: string, path = '', minConfidence = 0): Promise<{ results: SearchResult[] }> => {
    const { text, domains, entities } = parseSearchQuery(q);
    const p = new URLSearchParams({ limit: '50' });
    if (text) p.set('q', text);
    if (domains.length) p.set('domain', domains.join(','));
    if (entities.length) p.set('entities', entities.join(','));
    if (path) p.set('path', path);
    if (minConfidence) p.set('min_confidence', String(minConfidence));
    return fetch(`${base(repo)}/search?${p}`).then(r => r.json());
  },
  history: (repo: string, path: string, after?: string): Promise<HistoryResponse> => {
    const p = new URLSearchParams({ path, limit: '50' });
    if (after) p.set('after', after);
    return fetch(`${base(repo)}/history?${p}`).then(r => r.json());
  },
  commitDetail: (repo: string, hash: string): Promise<CommitDetail> =>
    fetch(`${base(repo)}/commit?hash=${encodeURIComponent(hash)}`).then(r => r.json()),
  updateFact: (repo: string, path: string, content: string): Promise<Fact> =>
    fetch(`${base(repo)}/fact`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path, content }),
    }).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); }),
  stats: (repo: string, path: string): Promise<Stats> => fetch(`${base(repo)}/stats?path=${encodeURIComponent(path)}`).then(r => r.json()),
  activity: (repo: string, path: string): Promise<ActivityStats> => fetch(`${base(repo)}/activity?path=${encodeURIComponent(path)}`).then(r => r.json()),
  status: (repo: string): Promise<Status> => fetch(`${base(repo)}/status`).then(r => r.json()),
  sync: (repo: string): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetch(`${base(repo)}/sync`, { method: 'POST' }).then(r => r.json()),
  synthesize: (repo: string, recipe = ''): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetch(`${base(repo)}/synthesize`, { method: 'POST', body: recipe }).then(r => r.json()),
  rebuild: (repo: string): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetch(`${base(repo)}/rebuild`, { method: 'POST' }).then(r => r.json()),
  recent: (repo: string, path: string, limit = 50, offset = 0): Promise<RecentResponse> =>
    fetch(`${base(repo)}/recent?path=${encodeURIComponent(path)}&limit=${limit}&offset=${offset}`).then(r => r.json()),
  getOrigin: (repo: string): Promise<OriginResponse | null> =>
    fetch(`${base(repo)}/origin`).then(r => r.status === 204 ? null : r.json()),
  setOrigin: (repo: string, opts: { url?: string; auth_method?: string; token?: string; user?: string; password?: string }): Promise<OriginSetResponse> =>
    fetch(`${base(repo)}/origin`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(opts),
    }).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); }),
};
