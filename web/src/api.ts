const BASE = '';

export interface DirChild { name: string; is_dir: boolean }
export interface BrowseResponse { path: string; children: DirChild[] }
export interface Fact { path: string; title: string; body: string; domain: string[]; confidence: number; sources: number; entities: string[]; refs: string[] }
export interface SearchResult { path: string; title: string; body: string; score: number; domain?: string[]; entities?: string[] }
export interface HistoryEntry { commit: string; date: string; message: string }
export interface Stats { total: number; domains: Record<string, number>; avg_confidence: number }
export interface Status { head: string; branch: string; index_commit: string; embeddings_enabled: boolean }

export const api = {
  browse: (path: string): Promise<BrowseResponse> => fetch(`${BASE}/api/browse?path=${encodeURIComponent(path)}`).then(r => r.json()),
  fact: (path: string): Promise<Fact> => fetch(`${BASE}/api/fact?path=${encodeURIComponent(path)}`).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); }),
  search: (q: string, minConfidence = 0): Promise<{ results: SearchResult[] }> => fetch(`${BASE}/api/search?q=${encodeURIComponent(q)}&min_confidence=${minConfidence}&limit=50`).then(r => r.json()),
  history: (path: string): Promise<{ entries: HistoryEntry[] }> => fetch(`${BASE}/api/history?path=${encodeURIComponent(path)}`).then(r => r.json()),
  stats: (path: string): Promise<Stats> => fetch(`${BASE}/api/stats?path=${encodeURIComponent(path)}`).then(r => r.json()),
  status: (): Promise<Status> => fetch(`${BASE}/api/status`).then(r => r.json()),
  sync: (): Promise<{ status: string; commit?: string; message?: string; error?: string }> => fetch(`${BASE}/api/sync`, { method: 'POST' }).then(r => r.json()),
};
