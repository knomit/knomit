const BASE = '/api/v1';

export interface DirChild { name: string; is_dir: boolean }
export interface BrowseResponse { path: string; children: DirChild[] }
export interface Fact { path: string; title: string; body: string; domain: string[]; confidence: number; sources: number; entities: string[]; refs: string[] }
export interface SearchResult { path: string; title: string; body: string; score: number; domain?: string[]; entities?: string[] }
export interface HistoryEntry { commit: string; date: string; message: string }
export interface Stats { total: number; domains: Record<string, number>; entities: Record<string, number>; avg_confidence: number }
export interface Status { head: string; branch: string; index_commit: string; embeddings_enabled: boolean }

// parseSearchQuery splits a query string into structured components.
// Tokens of the form domain:X or entity:X are extracted as filters;
// the remaining words are treated as free text for FTS/embeddings.
export function parseSearchQuery(raw: string): { text: string; domains: string[]; entities: string[] } {
  const domains: string[] = [];
  const entities: string[] = [];
  const textTokens: string[] = [];
  for (const token of raw.trim().split(/\s+/)) {
    if (!token) continue;
    if (token.startsWith('domain:')) { const v = token.slice(7); if (v) domains.push(v); }
    else if (token.startsWith('entity:')) { const v = token.slice(7); if (v) entities.push(v); }
    else textTokens.push(token);
  }
  return { text: textTokens.join(' '), domains, entities };
}

export const api = {
  browse: (path: string): Promise<BrowseResponse> => fetch(`${BASE}/browse?path=${encodeURIComponent(path)}`).then(r => r.json()),
  fact: (path: string): Promise<Fact> => fetch(`${BASE}/fact?path=${encodeURIComponent(path)}`).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); }),
  search: (q: string, minConfidence = 0): Promise<{ results: SearchResult[] }> => {
    const { text, domains, entities } = parseSearchQuery(q);
    const p = new URLSearchParams({ limit: '50' });
    if (text) p.set('q', text);
    if (domains.length) p.set('domain', domains.join(','));
    if (entities.length) p.set('entities', entities.join(','));
    if (minConfidence) p.set('min_confidence', String(minConfidence));
    return fetch(`${BASE}/search?${p}`).then(r => r.json());
  },
  history: (path: string): Promise<{ entries: HistoryEntry[] }> => fetch(`${BASE}/history?path=${encodeURIComponent(path)}`).then(r => r.json()),
  stats: (path: string): Promise<Stats> => fetch(`${BASE}/stats?path=${encodeURIComponent(path)}`).then(r => r.json()),
  status: (): Promise<Status> => fetch(`${BASE}/status`).then(r => r.json()),
  sync: (): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetch(`${BASE}/sync`, { method: 'POST' }).then(r => r.json()),
  synthesize: (recipe = ''): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetch(`${BASE}/synthesize`, { method: 'POST', body: recipe }).then(r => r.json()),
};
