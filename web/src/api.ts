const BASE = '/api/v1/knomit';

export interface DirChild { name: string; is_dir: boolean }
export interface BrowseResponse { path: string; children: DirChild[] }
export interface Fact { path: string; title: string; body: string; domain: string[]; confidence: number; sources: number; entities: string[]; refs: string[] }
export interface SearchResult { path: string; title: string; body: string; score: number; domain?: string[]; entities?: string[] }
export interface HistoryEntry { commit: string; date: string; message: string }
export interface HistoryEntryWithTags { commit: string; date: string; message: string; tags: string[] }
export interface HistoryResponse { entries: HistoryEntryWithTags[]; next?: string }
export interface CommitFile { path: string; action: string }
export interface CommitDetail { commit: string; date: string; message: string; tags: string[]; files: CommitFile[] }
export interface Stats { total: number; domains: Record<string, number>; entities: Record<string, number>; avg_confidence: number }
export interface Status { head: string; branch: string; index_commit: string; embeddings_enabled: boolean; ontology_root: string }

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
  browse: (path: string): Promise<BrowseResponse> => fetch(`${BASE}/browse?path=${encodeURIComponent(path)}`).then(r => r.json()),
  fact: (path: string, commit?: string): Promise<Fact> => {
    const p = new URLSearchParams({ path });
    if (commit) p.set('commit', commit);
    return fetch(`${BASE}/fact?${p}`).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); });
  },
  search: (q: string, minConfidence = 0): Promise<{ results: SearchResult[] }> => {
    const { text, domains, entities } = parseSearchQuery(q);
    const p = new URLSearchParams({ limit: '50' });
    if (text) p.set('q', text);
    if (domains.length) p.set('domain', domains.join(','));
    if (entities.length) p.set('entities', entities.join(','));
    if (minConfidence) p.set('min_confidence', String(minConfidence));
    return fetch(`${BASE}/search?${p}`).then(r => r.json());
  },
  history: (path: string, after?: string): Promise<HistoryResponse> => {
    const p = new URLSearchParams({ path, limit: '50' });
    if (after) p.set('after', after);
    return fetch(`${BASE}/history?${p}`).then(r => r.json());
  },
  commitDetail: (hash: string): Promise<CommitDetail> =>
    fetch(`${BASE}/commit?hash=${encodeURIComponent(hash)}`).then(r => r.json()),
  stats: (path: string): Promise<Stats> => fetch(`${BASE}/stats?path=${encodeURIComponent(path)}`).then(r => r.json()),
  status: (): Promise<Status> => fetch(`${BASE}/status`).then(r => r.json()),
  sync: (): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetch(`${BASE}/sync`, { method: 'POST' }).then(r => r.json()),
  synthesize: (recipe = ''): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetch(`${BASE}/synthesize`, { method: 'POST', body: recipe }).then(r => r.json()),
  getOrigin: (): Promise<OriginResponse | null> =>
    fetch(`${BASE}/origin`).then(r => r.status === 204 ? null : r.json()),
  setOrigin: (opts: { url: string; auth_method?: string; token?: string; user?: string; password?: string }): Promise<OriginSetResponse> =>
    fetch(`${BASE}/origin`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(opts),
    }).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); }),
};
