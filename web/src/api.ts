// API_BASE is the origin the REST/SSE API is served from. Empty in the cloud
// build (UI and API are same-origin, so URLs stay relative). The desktop build
// serves the UI in-process via Wails from a different origin and sets
// window.__KNOMIT_API_BASE__ (via /config.js) to the looknomitck API URL, making
// all calls cross-origin to the TCP listener. One bundle, runtime-configured.
// apiBase reads the configured base at call time (not import time) so it is
// robust to script/module evaluation order and easy to test.
function apiBase(): string {
  return (typeof window !== 'undefined' &&
    (window as Window & { __KNOMIT_API_BASE__?: string }).__KNOMIT_API_BASE__) || '';
}

// apiUrl prefixes an absolute API path with the runtime API base.
export function apiUrl(path: string): string {
  return apiBase() + path;
}

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
      detail = errorText(await r.json(), detail);
    } catch {
      // Non-JSON body; keep the statusText.
    }
    throw new Error(`${url} → ${r.status} ${detail}`);
  }
  return r.json() as Promise<T>;
}

export interface VersionInfo {
  version: string;
  commit: string;
  full: string;
  readOnly: boolean;
}

// fetchVersion GETs /api/v1/version — the build version of the running server.
export async function fetchVersion(): Promise<VersionInfo> {
  const data = await fetchJSON<VersionInfo & { read_only?: boolean }>(apiUrl('/api/v1/version'));
  return { version: data.version, commit: data.commit, full: data.full, readOnly: !!data.read_only };
}

function repoBase(repo: string): string {
  return apiUrl(`/api/v1/repos/${repo}`);
}

function branchBase(repo: string, branch: string): string {
  return `${repoBase(repo)}/branches/${encodeBranch(branch)}`;
}

export interface RepoInfo { name: string }

// RepoDetails is the single-repo GET shape. description is the verbatim kb.md
// root manifest read at HEAD; absent when the repo has no readable kb.md.
export interface RepoDetails { name: string; agent_branch?: string; description?: string }

// getRepo fetches GET /api/v1/repos/{repo} — name, agent branch, and the kb.md
// description when available.
async function getRepo(repo: string): Promise<RepoDetails> {
  return fetchJSON<RepoDetails>(repoBase(repo));
}

/* Description caps, in BYTES (not characters) — mirrors of the server-side
 * limits, kept here beside the calls they bound. A repo's kb.md is a manifest
 * that runs to pages; a lens description is a note about a read union, and its
 * cap is more than an order of magnitude smaller. An editor shared by both must
 * say which one it is holding, or the difference only surfaces as a 422.
 *   repos.MaxRepoDescriptionBytes — internal/repos/manifest.go
 *   repos.MaxLensDescriptionBytes — internal/repos/lens.go */
export const MAX_REPO_DESCRIPTION_BYTES = 64 * 1024;
export const MAX_LENS_DESCRIPTION_BYTES = 4096;

// updateRepo PATCHes /api/v1/repos/{repo}. The only editable field is
// description, which the server commits to the repo's kb.md root manifest on
// the agent branch — so editing it here writes a real commit into the repo's
// history. Returns the re-read repo view (same shape as getRepo).
async function updateRepo(repo: string, body: { description?: string }): Promise<RepoDetails> {
  return fetchJSON<RepoDetails>(repoBase(repo), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// LensRead is one read-mount of a lens: a source repo, optionally pinned to a
// branch and/or a source label (the server fills defaults when omitted).
export interface LensRead { repo: string; branch?: string; source?: string }
// Lens is the composed view: writes land in `write`, reads union `reads`.
// created_at/updated_at are unix seconds, present on server responses.
export interface Lens {
  name: string; write: string; reads: LensRead[];
  description?: string;
  created_at?: number; updated_at?: number;
}
// LensSource identifies which mount a lens union row came from: the source
// repo, its 12-char id, and the branch the row was read at (RFC §6.2).
export interface LensSource { repo: string; id: string; branch: string }
// LensFactEntry is one row of a lens union facts collection — a RecentFactEntry
// (path canonical: bare for the write repo, kb://<id12>/… for a read mount) plus
// its source mount.
export interface LensFactEntry extends RecentFactEntry { source: LensSource }

// LensRepoStats is one per-mount row of the lens union stats: the mount's
// identity plus its own aggregate stats and commit activity.
export interface LensRepoStats {
  id: string; name: string; source: string; branch: string; is_write: boolean;
  total: number; avg_confidence: number;
  domains: Record<string, number>; entities: Record<string, number>;
  last_commit: string; changes_7d: number; changes_30d: number; changes_90d: number;
}
// LensStats is the flat union stats envelope: exact-sum roll-up (weighted
// avg_confidence, max last_commit) plus one LensRepoStats row per mount.
export interface LensStats {
  total: number; repo_count: number; last_commit: string; avg_confidence: number;
  domains: Record<string, number>; entities: Record<string, number>;
  repos: LensRepoStats[];
}

export interface DirChild { name: string; is_dir: boolean; type?: string; title?: string; fullPath?: string }
export interface BrowseResponse { path: string; children: DirChild[] }

// LensDirChild is one child of a unified lens tree level: a DirChild plus —
// on fact leaves only — the canonical qualified wire `path` (bare for the
// write mount, kb://<id12>/… for a read mount; what openFact needs) and the
// owning mount's `source` tag. Directories are merged across mounts and carry
// neither.
export interface LensDirChild extends DirChild { path?: string; source?: { repo: string; id: string } }
export interface LensBrowseResponse { path: string; children: LensDirChild[] }
// FactRef mirrors internal/web/ref_view.go's RefView. `kind` is decided
// server-side by fact.ClassifyRef; for a fact in this repo, "fact" vs "broken"
// additionally reflects existence AT THE VIEWED COMMIT, which only the server
// can know. Never re-derive any of this from `raw` in the client.
export interface FactRef {
  raw: string;
  kind: 'fact' | 'broken' | 'foreign' | 'source_code' | 'url';
  // Repo-relative fact path, sent for kind 'fact' and 'broken' only. This is
  // what a hop addresses: a canonical kb://<own-id>/<path> ref and its bare
  // equivalent name the same fact and arrive with the same `path`.
  path?: string;
  // Compact LABEL for kinds whose raw form is unreadable at a glance —
  // 'source_code' and 'foreign', where a 12-hex repo id (and for src://, two
  // 40-hex hashes) dominate the string. Computed server-side from the same
  // ClassifyRef parse that produced `kind`, for the same reason `kind` is:
  // taking a ref apart is parsing, and the client must not hold a second
  // parser. Absent means "render raw" — the correct answer for every other
  // kind. Whatever shows `display` MUST keep `raw` reachable (a title
  // attribute at minimum): the stored citation is what a reader copies.
  display?: string;
  _links?: { target?: { href: string } };
}

export interface Fact { path: string; title: string; kind?: string; type?: string; origin?: string; body: string; domain: string[]; confidence: number; sources: number; entities: string[]; refs: FactRef[]; ref_warnings?: string[]; parse_error?: string; from_commit?: string; commit_hash?: string; commit_date?: string }

// normalizeFactResponse maps the HAL FactView shape to the Fact interface.
//
// refs arrive as [{raw, kind, _links}] and are kept whole. An earlier version
// flattened them to bare strings, which threw away the server's classification
// and left FactBody re-deriving a worse one from a regex — it could not know
// whether a target existed, could not tell a foreign repo from a typo, and
// marked any schemeless string clickable.
function normalizeFactResponse(data: any): Fact {
  let refs: FactRef[] = [];
  if (Array.isArray(data.refs)) {
    refs = data.refs.map((r: any) =>
      typeof r === 'string'
        // Older server: a bare string carries no kind, so classify on the ONE
        // thing a string can support — does it have a scheme. Anything
        // schemeless is a repo-relative fact path; anything with a scheme is
        // some URI, and 'url' renders inert unless it is http(s). Never guess
        // 'fact' for a scheme'd ref: that made a src:// citation clickable and
        // handed an unhoppable string to onRefClick.
        ? ({ raw: r, kind: /^[a-z][a-z0-9+.-]*:/i.test(r) ? 'url' : 'fact', path: r } as FactRef)
        : ({ raw: r.raw, kind: r.kind, path: r.path, display: r.display, _links: r._links } as FactRef));
  }
  return {
    path: data.path,
    ref_warnings: data.ref_warnings,
    title: data.title,
    kind: data.kind,
    type: data.type,
    origin: data.origin,
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
export interface CommitAuthor { name: string; email: string }
export interface CommitDetail { commit: string; date: string; message: string; operation?: string; author?: CommitAuthor; files: CommitFile[] }
export interface Stats { total: number; domains: Record<string, number>; entities: Record<string, number>; avg_confidence: number }
export interface Status { head: string; branch: string; index_commit: string; embeddings_enabled: boolean; ontology_root: string; index_state?: string; index_done?: number; index_total?: number; index_percent?: number }
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
import { track } from './telemetry';

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
    if (SHORT_SHA.test(v)) return { mode: 'history', commit: v.toLowerCase() };
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

// parseFilterQuery is context-aware via opts.allowRepo. `repo:` is a lens-only
// facet: it is recognised as a chip category ONLY when allowRepo is set (lens
// context). In a repo context (the default) `repo:foo` stays free text — the
// repo-context parse output is byte-for-byte what it was before this facet
// existed, so no new chip category can leak onto a repo surface.
export function parseFilterQuery(raw: string, lookupHead?: () => string, opts?: { allowRepo?: boolean }): { chips: FilterChip[]; text: string; asOf?: AsOf; warnings: string[] } {
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

  // The recognised chip categories. `repo` is appended only in lens context.
  const cats = opts?.allowRepo
    ? 'domain|entity|type|kind|origin|ep|path|repo'
    : 'domain|entity|type|kind|origin|ep|path';
  const quotedRe = new RegExp(`(${cats}):"([^"]+)"`, 'g');
  const bareRe = new RegExp(`(${cats}):(\\S+)`, 'g');

  // Extract prefix:"quoted value" patterns first
  remaining = remaining.replace(quotedRe, (_m, prefix, value) => {
    chips.push({ category: prefix as FilterChip['category'], value });
    return '';
  });
  // Extract prefix:value patterns (no quotes, no spaces)
  remaining = remaining.replace(bareRe, (_m, prefix, value) => {
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
  | { phase: "replaying"; current?: number; total?: number }
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

// errorText pulls the human-readable message out of an error body. The API
// emits RFC 9457 problem+json everywhere, so `detail` is the message and
// `title` is the class; `error` remains as a fallback for any body that
// predates the problem+json unification. Callers pass a statusText fallback
// for non-JSON bodies.
function errorText(body: unknown, fallback: string): string {
  const b = body as { detail?: string; title?: string; error?: string } | null;
  return b?.detail || b?.title || b?.error || fallback;
}

export async function readSSEStream(res: Response, onEvent?: (e: SSEEvent) => void): Promise<void> {
  if (!res.ok) {
    const err = await res.json().catch(() => null);
    throw new Error(errorText(err, res.statusText));
  }
  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    // Only the bytes up to the last newline form complete lines; everything
    // after is a partial line that must be retained for the next chunk. A chunk
    // with no newline at all is entirely partial — keep the whole buffer (the
    // old code wiped it here, silently dropping any event split across reads).
    const nl = buf.lastIndexOf('\n');
    if (nl < 0) continue;
    for (const ev of parseSSELines(buf.slice(0, nl + 1))) onEvent?.(ev);
    buf = buf.slice(nl + 1);
  }
  // Flush a trailing complete line that lacked a final newline.
  for (const ev of parseSSELines(buf)) onEvent?.(ev);
}

export function createSession(repo: string, opts: { url: string; auth_method?: string; token?: string; user?: string; password?: string }): Promise<SessionCreateResponse> {
  return fetch(`${repoBase(repo)}/origin-sessions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(opts),
  }).then(r => { if (!r.ok) return r.json().then(e => { throw new Error(errorText(e, r.statusText)); }); return r.json(); });
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

// getAgentBranch returns THIS machine's agent branch for a repo. The server
// knows it authoritatively (RepoDetails.agent_branch), so prefer that: a repo
// connected to a shared remote can carry several "agent/*" branches (one per
// machine), and the heuristic below would otherwise pick the first one
// alphabetically — a foreign machine's branch with no local facts. Fall back to
// the branch-list heuristic only when the server doesn't report agent_branch.
async function getAgentBranch(repo: string): Promise<string> {
  try {
    const details = await getRepo(repo);
    if (details.agent_branch) return details.agent_branch;
  } catch {
    // fall through to the branch-list heuristic
  }
  const data = await fetchJSON<any>(`${repoBase(repo)}/branches`);
  const branches: Array<{ name: string }> =
    (data._embedded?.branches as Array<{ name: string }>) || [];
  const agent = branches.find(b => b.name.startsWith('agent/'));
  const main = branches.find(b => b.name === 'main');
  return (agent || main || branches[0])?.name || 'main';
}

export interface CreateEvent {
  type: 'progress' | 'done' | 'error';
  step?: string;
  message?: string;
  pct?: number;
  repo?: { name: string };
  title?: string;
  detail?: string;
}

export function parseNDJSONLine(line: string): CreateEvent | null {
  const t = line.trim();
  if (!t) return null;
  try {
    return JSON.parse(t) as CreateEvent;
  } catch {
    return null;
  }
}

export interface CreateRepoBody {
  name: string;
  mode: 'preset' | 'custom' | 'clone';
  ontology_preset?: string;
  ontology_yaml?: string;
  origin?: { url: string; branch?: string; auth_method?: string; auth_token?: string };
}

export interface ArchivedRepo {
  id: string;
  name: string;
  origin: string;
  archivedAt: string;
}

// createRepo POSTs and streams NDJSON progress, invoking onEvent per line.
// Resolves when the stream ends. Throws on a pre-stream non-OK (problem+json).
async function createRepo(body: CreateRepoBody, onEvent: (e: CreateEvent) => void): Promise<void> {
  const r = await fetch(apiUrl('/api/v1/repos'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    let detail = r.statusText;
    try {
      const b = await r.json();
      detail = b?.detail || b?.title || detail;
    } catch { /* ignore */ }
    throw new Error(`create → ${r.status} ${detail}`);
  }
  const reader = r.body!.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let nl: number;
    while ((nl = buf.indexOf('\n')) >= 0) {
      const e = parseNDJSONLine(buf.slice(0, nl));
      buf = buf.slice(nl + 1);
      if (e) onEvent(e);
    }
  }
  const tail = parseNDJSONLine(buf);
  if (tail) onEvent(tail);
}

async function archiveRepo(repo: string): Promise<ArchivedRepo> {
  return fetchJSON<ArchivedRepo>(repoBase(repo), { method: 'DELETE' });
}

async function listArchived(): Promise<ArchivedRepo[]> {
  const data = await fetchJSON<{ _embedded?: { archived?: ArchivedRepo[] } }>(apiUrl('/api/v1/archived'));
  return data._embedded?.archived ?? [];
}

async function restoreRepo(id: string, newName?: string): Promise<{ name: string }> {
  return fetchJSON<{ name: string }>(apiUrl(`/api/v1/archived/${id}/restore`), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(newName ? { new_name: newName } : {}),
  });
}

async function purgeRepo(id: string): Promise<void> {
  const r = await fetch(apiUrl(`/api/v1/archived/${id}`), { method: 'DELETE' });
  if (!r.ok) throw new Error(`purge → ${r.status}`);
}

// listLenses GETs /api/v1/lenses and unwraps the HAL CollectionView
// (_embedded.lenses), mirroring api.repos(). Falls back to [] when the shape
// is missing so the UI never sees undefined.
async function listLenses(): Promise<Lens[]> {
  const data = await fetchJSON<{ _embedded?: { lenses?: Lens[] } }>(apiUrl('/api/v1/lenses'));
  return data._embedded?.lenses ?? [];
}

// getLens GETs /api/v1/lenses/{name} — the single lens view (200/404).
async function getLens(name: string): Promise<Lens> {
  return fetchJSON<Lens>(apiUrl(`/api/v1/lenses/${name}`));
}

// createLens POSTs a new lens. fetchJSON throws on non-2xx surfacing the
// problem+json `detail`, so the UI shows the server's validation message.
async function createLens(body: { name: string; write: string; reads: LensRead[]; description?: string }): Promise<Lens> {
  return fetchJSON<Lens>(apiUrl('/api/v1/lenses'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// lensBase builds the base URL for a single lens's sub-resources.
function lensBase(name: string): string {
  return apiUrl(`/api/v1/lenses/${name}`);
}

// listLensFacts GETs /api/v1/lenses/{lens}/facts — the recency-ordered, deduped
// union of the lens's write repo + read mounts. Flat envelope ({facts,total});
// each row carries a canonical `path` and its `source` mount. `repos` maps to
// repeated `repo=` params (narrows the fan-out); omitted params are dropped.
async function listLensFacts(lens: string, opts: { path?: string; query?: string; limit?: number; offset?: number; repos?: string[] }): Promise<{ facts: LensFactEntry[]; total: number }> {
  const p = new URLSearchParams();
  if (opts.path) p.set('path', opts.path);
  if (opts.query) p.set('query', opts.query);
  if (opts.limit !== undefined) p.set('limit', String(opts.limit));
  if (opts.offset !== undefined) p.set('offset', String(opts.offset));
  for (const repo of opts.repos ?? []) p.append('repo', repo);
  const qs = p.toString();
  return fetchJSON<{ facts: LensFactEntry[]; total: number }>(`${lensBase(lens)}/facts${qs ? `?${qs}` : ''}`);
}

// lensSearch GETs /api/v1/lenses/{lens}/search — the RRF-fused union relevance
// search. The envelope is flat ({results,total}); this returns just the results
// array (each row canonical path + source). `repos` → repeated `repo=` params.
// `opts` forwards the same content filters the repo /search sends — the lens
// search handler accepts the full set (path/type/kind/origin/ep/domain/entities);
// the lens FACTS handler does NOT, which is why the Library routes filter-bearing
// reads through this search path.
async function lensSearch(
  lens: string, q: string, repos?: string[],
  opts?: { path?: string; types?: string[]; kinds?: string[]; origins?: string[]; eps?: string[]; domains?: string[]; entities?: string[] },
): Promise<(SearchResult & { source: LensSource })[]> {
  const p = new URLSearchParams();
  if (q) p.set('q', q);
  if (opts?.path) p.set('path', opts.path);
  if (opts?.types?.length) p.set('type', opts.types.join(','));
  if (opts?.kinds?.length) p.set('kind', opts.kinds.join(','));
  if (opts?.origins?.length) p.set('origin', opts.origins.join(','));
  if (opts?.eps?.length) p.set('ep', opts.eps.join(','));
  if (opts?.domains?.length) p.set('domain', opts.domains.join(','));
  if (opts?.entities?.length) p.set('entities', opts.entities.join(','));
  for (const repo of repos ?? []) p.append('repo', repo);
  const data = await fetchJSON<{ results?: (SearchResult & { source: LensSource })[] }>(`${lensBase(lens)}/search?${p}`);
  return data.results ?? [];
}

// lensCompletions GETs /api/v1/lenses/{lens}/completions — the union of per-mount
// completion values, plus the lens-only category=repo that lists mount names.
async function lensCompletions(lens: string, category: string, prefix = ''): Promise<{ values: string[] }> {
  return fetchJSON<{ values: string[] }>(`${lensBase(lens)}/completions?category=${encodeURIComponent(category)}&prefix=${encodeURIComponent(prefix)}`);
}

// getLensFact GETs /api/v1/lenses/{lens}/facts/{path} — a single fact read
// through a lens. The whole path is encodeURIComponent'd as one segment so a
// kb://<id12>/kb/… qualified address survives to the server, which PathUnescapes
// it (a bare kb/… path round-trips too). Response is the repo FactView body
// (normalized) plus a `source` mount.
async function getLensFact(lens: string, path: string): Promise<Fact & { source: LensSource }> {
  const data = await fetchJSON<any>(`${lensBase(lens)}/facts/${encodeURIComponent(path)}`);
  return { ...normalizeFactResponse(data), source: data.source as LensSource };
}

// getLensStats GETs /api/v1/lenses/{lens}/stats — the union stats/activity
// roll-up of the lens's write repo + read mounts (exact sums, total-weighted
// avg_confidence, max last_commit) with one row per mount. Flat envelope,
// mirroring the other lens reads.
async function getLensStats(lens: string, path: string): Promise<LensStats> {
  return fetchJSON<LensStats>(`${lensBase(lens)}/stats?path=${encodeURIComponent(path)}`);
}

// lensBrowse GETs /api/v1/lenses/{lens}/topics[/{segments}] — ONE level of the
// unified, merged-by-topic ontology tree across the lens's mounts. Strips the
// ontology root from `path` like api.browse does; `repos` maps to repeated
// `repo=` params like listLensFacts (omitted → all mounts). The envelope is
// flat ({path, children}) — no HAL _embedded — and the returned `path` echoes
// the caller's full path, mirroring api.browse.
async function lensBrowse(lens: string, path: string, ontologyRoot: string, repos?: string[]): Promise<LensBrowseResponse> {
  const relative = stripOntologyRoot(ontologyRoot, path);
  const p = new URLSearchParams();
  for (const repo of repos ?? []) p.append('repo', repo);
  const qs = p.toString();
  const url = `${lensBase(lens)}/topics${relative ? `/${relative}` : ''}${qs ? `?${qs}` : ''}`;
  const data = await fetchJSON<{ path: string; children?: LensDirChild[] }>(url);
  return { path, children: data.children ?? [] };
}

// updateLens PATCHes /api/v1/lenses/{name} — omitted fields keep their current
// value, provided fields replace wholesale (reads replace as a set). Returns the
// updated lens view. fetchJSON surfaces the problem+json detail on non-2xx.
async function updateLens(name: string, body: { write?: string; reads?: LensRead[]; description?: string }): Promise<Lens> {
  return fetchJSON<Lens>(lensBase(name), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// deleteLens DELETEs /api/v1/lenses/{name} (204 → void). Raw fetch (not
// fetchJSON) because a 204 carries no JSON body to parse.
async function deleteLens(name: string): Promise<void> {
  const r = await fetch(apiUrl(`/api/v1/lenses/${name}`), { method: 'DELETE' });
  if (!r.ok) {
    let detail = r.statusText;
    try { const b = await r.json(); detail = b?.detail || b?.title || detail; } catch { /* ignore */ }
    throw new Error(`delete lens → ${r.status} ${detail}`);
  }
}

export const api = {
  getAgentBranch,
  getRepo,
  updateRepo,

  repos: (): Promise<RepoInfo[]> =>
    fetchJSON<any>(apiUrl('/api/v1/repos')).then(data => {
      // New endpoint returns HAL: {count, _links, _embedded: {repos: [{name, _links}]}}
      if (data && data._embedded && Array.isArray(data._embedded.repos)) {
        return data._embedded.repos as RepoInfo[];
      }
      // Fallback: flat array (legacy)
      return Array.isArray(data) ? data : [];
    }),

  createRepo,
  archiveRepo,
  listArchived,
  restoreRepo,
  purgeRepo,

  listLenses,
  getLens,
  createLens,
  updateLens,
  deleteLens,
  listLensFacts,
  lensSearch,
  lensCompletions,
  getLensFact,
  getLensStats,
  lensBrowse,

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
    opts?: { types?: string[]; kinds?: string[]; excludeKinds?: string[]; origins?: string[]; eps?: string[]; domains?: string[]; entities?: string[] }
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
    if (opts?.origins?.length) p.set('origin', opts.origins.join(','));
    if (opts?.eps?.length) p.set('ep', opts.eps.join(','));
    return fetchJSON<any>(`${branchBase(repo, branch)}/search?${p}`).then(data => {
      // HAL CollectionView: {_embedded: {results: [...]}}
      const results: SearchResult[] = data._embedded?.results || data.results || [];
      // Anonymous telemetry: counts only — never the query text. No-op unless a
      // host defined window.knomitTelemetry (see telemetry.ts).
      track('search_performed', {
        result_count: results.length,
        query_len: q.length,
        had_results: results.length > 0,
      });
      return { results };
    });
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
      index_state: data.index_state,
      index_done: data.index_done,
      index_total: data.index_total,
      index_percent: data.index_percent,
    })),

  synthesize: (repo: string, branch: string, recipe = ''): Promise<{ op: string; id?: string; status: string; message?: string }> =>
    fetchJSON(`${branchBase(repo, branch)}/synthesis-runs`, { method: 'POST', body: recipe }),

  rebuild: (repo: string, branch: string): Promise<{ id?: string; kind?: string; state?: string }> =>
    fetchJSON(`${branchBase(repo, branch)}/index-rebuilds`, { method: 'POST' }),

  recent: (repo: string, branch: string, path: string, query = '', limit = 50, offset = 0,
    opts?: { typeFilter?: string; excludeType?: string; kinds?: string[]; excludeKinds?: string[]; origins?: string[]; domains?: string[]; entities?: string[]; eps?: string[] }
  ): Promise<RecentResponse> => {
    const p = new URLSearchParams({ sort: 'recent', path, limit: String(limit), offset: String(offset) });
    if (query) p.set('q', query);
    if (opts?.typeFilter) p.set('type', opts.typeFilter);
    if (opts?.excludeType) p.set('exclude_type', opts.excludeType);
    if (opts?.kinds?.length) p.set('kind', opts.kinds.join(','));
    if (opts?.excludeKinds?.length) p.set('exclude_kind', opts.excludeKinds.join(','));
    if (opts?.origins?.length) p.set('origin', opts.origins.join(','));
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
    fetch(`${repoBase(repo)}/origin`).then(r => {
      if (r.status === 204) return null; // no origin configured
      if (!r.ok) throw new Error(`origin → ${r.status} ${r.statusText}`);
      return r.json();
    }),

  setOrigin: (repo: string, opts: { url?: string; branch?: string; auth_method?: string; token?: string; user?: string; password?: string }): Promise<OriginSetResponse> =>
    fetch(`${repoBase(repo)}/origin`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(opts),
    }).then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); }),

  deleteOrigin: (repo: string): Promise<void> =>
    fetch(`${repoBase(repo)}/origin`, { method: 'DELETE' })
      .then(r => { if (!r.ok) throw new Error(`disconnect → ${r.status} ${r.statusText}`); }),

  // setOriginUpstream changes ONLY the consensus ("main") branch of an existing
  // origin (no reconnect, no auth change). The reconcile loop picks it up next tick.
  setOriginUpstream: (repo: string, branch: string): Promise<void> =>
    fetch(`${repoBase(repo)}/origin/upstream`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ branch }),
    }).then(r => { if (!r.ok) throw new Error(`set upstream → ${r.status} ${r.statusText}`); }),

  // listBranchNames returns all branch names for a repo (for the upstream picker).
  listBranchNames: async (repo: string): Promise<string[]> => {
    const data = await fetchJSON<any>(`${repoBase(repo)}/branches`);
    const branches: Array<{ name: string }> =
      (data._embedded?.branches as Array<{ name: string }>) || [];
    return branches.map(b => b.name);
  },

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

  explain: (repo: string, branch: string, path: string, commit?: string, opts?: { fallback?: 'before' }): Promise<{
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
    // ?fallback=before is only appended when explicitly requested via opts.fallback.
    // Commit-anchored edges with fallback resolve the last-valid version's edges
    // when the pinned commit is past retraction (matches fact view). HEAD-anchored
    // reads and commit-anchored reads without explicit fallback take no fallback.
    const edgeQuery = (commit && opts?.fallback === 'before') ? '?fallback=before' : '';
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
      fetch(`${factURL}/incoming${edgeQuery}`).then(r => r.ok ? r.json() : r.json().then((e: unknown) => { throw new Error(errorText(e, r.statusText)); })),
      fetch(`${factURL}/outgoing${edgeQuery}`).then(r => r.ok ? r.json() : r.json().then((e: unknown) => { throw new Error(errorText(e, r.statusText)); })),
    ]).then(([inc, out]) => ({
      incoming: groupRefs(parseRefs(inc)),
      outgoing: groupRefs(parseRefs(out)),
    }));
  },
};
