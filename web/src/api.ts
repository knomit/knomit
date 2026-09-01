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

// `id` is the repo's 12-hex stable identity — the root commit of its KB store,
// the same value a kb://<id>/<path> ref carries. Optional because an older
// server omits it, and because a repo whose store has not opened cannot resolve
// one. NOTE this is the KB-store namespace: a src:// ref's id is the SOURCE
// repo's root commit and will never match anything here.
// RepoInfo is one row of the repo listing. `uid` is the registry key lens
// membership is written with; `id` is the 12-char root-commit identity `kb://`
// paths address. They are different questions — never substitute one.
//
// `state` says whether the row has a live store: 'active', or the reason it has
// none — 'missing' (the database file is gone), 'unopenable', or 'conflict'
// (another repo already holds this knowledge base). The listing carries such a
// repo because it IS registered; every one of its own endpoints answers 409.
// Optional because an older server omits it entirely.
export interface RepoInfo {
  name: string;
  uid: string;
  id?: string;
  state?: string;
  /** Human-readable amplification of a non-'active' state. */
  detail?: string;
}

// repoAvailable reports whether a repo can be read at all.
//
// Written as "not a known-bad state" rather than "state === 'active'" so an
// older server (no `state` at all) and a newer one (a reason this build has
// never heard of) both stay usable: the first is genuinely active, and refusing
// to open the second would hide a repo over a string we failed to recognise.
// Callers that RENDER the state show it verbatim; only navigation gates on this.
export function repoAvailable(r: Pick<RepoInfo, 'state'>): boolean {
  return !r.state || r.state === 'active';
}

// LensMembership is the part of a Lens that says which repos it binds.
export type LensMembership = Pick<Lens, 'write' | 'reads'>;

// brokenLensMember names the first member repo of a lens that has no live
// store, or null when every member is readable.
//
// A lens binds ALL of its members or none. NewBindingOfLens
// (internal/repos/binding.go) fails the whole lens the moment one member has no
// live instance — "a lens must never silently shrink its read set" — so a lens
// with one dead mount is not a lens that lost a mount: every read endpoint
// under it answers 503. GET /lenses/{lens} sits OUTSIDE the lens middleware and
// still answers 200 for such a lens, which is why the resolve-and-rescue path
// never fires on its own: the fetch succeeded. This is the check that stands in
// for the gate the route does not have.
//
// It returns a NAME, never a uid: the uid is a ksuid the reader has never been
// shown.
export function brokenLensMember(l: LensMembership, repos: RepoInfo[]): string | null {
  // Only POSITIVE evidence counts: a member the listing carries, in a state it
  // says is broken. A member the listing does not mention at all is NOT
  // evidence — the listing may simply be older than the lens, or not loaded
  // yet — and treating absence as breakage would grey out every lens for the
  // first frame after mount. Same reasoning as repoAvailable, which is written
  // as "not a known-bad state" rather than "state === active".
  const byUID = new Map(repos.map(r => [r.uid, r]));
  for (const m of [l.write, ...l.reads]) {
    const r = byUID.get(m.uid);
    if (r && !repoAvailable(r)) return r.name;
  }
  return null;
}

// lensAvailable reports whether a lens can be entered at all. Mirrors
// repoAvailable, and is the gate every navigation into a lens must pass.
export function lensAvailable(l: LensMembership, repos: RepoInfo[]): boolean {
  return brokenLensMember(l, repos) === null;
}

// RepoDetails is the single-repo GET shape. description is the verbatim
// README.md root manifest read at HEAD; license is the verbatim LICENSE. Both
// are absent when the repo has no readable copy.
//
// license_oversize is a THIRD state for the licence, distinct from both "no
// license field" (no LICENSE exists) and a present license: a LICENSE exists
// on the branch but exceeds the server's read cap
// (repos.MaxRepoDescriptionBytes), so its content is withheld. The Manage
// pane must render this differently from "no LICENSE" — offering Add/Edit
// over a file the server never actually read is what let a save silently
// destroy an oversize LICENSE (see WriteLicense's ErrLicenseTooLargeToReplace
// guard, which now refuses that write server-side too). Only ever true when
// license is absent; never sent as false.
export interface RepoDetails { name: string; agent_branch?: string; description?: string; license?: string; license_oversize?: boolean }

// getRepo fetches GET /api/v1/repos/{repo} — name, agent branch, and the
// README.md description when available.
async function getRepo(repo: string): Promise<RepoDetails> {
  return fetchJSON<RepoDetails>(repoBase(repo));
}

/* Description caps, in BYTES (not characters) — mirrors of the server-side
 * limits, kept here beside the calls they bound. A repo's README.md is a
 * manifest that runs to pages; a lens description is a note about a read union,
 * and its cap is more than an order of magnitude smaller. An editor shared by
 * both must say which one it is holding, or the difference only surfaces as a
 * 422.
 *   repos.MaxRepoDescriptionBytes — internal/repos/manifest.go
 *   repos.MaxLensDescriptionBytes — internal/repos/lens.go */
export const MAX_REPO_DESCRIPTION_BYTES = 64 * 1024;
export const MAX_LENS_DESCRIPTION_BYTES = 4096;

// updateRepo PATCHes /api/v1/repos/{repo}. The editable fields are
// description and license, each committed to the repo's README.md or LICENSE
// root manifest on the agent branch — so editing either here writes a real
// commit into the repo's history. The two are independent: a field omitted
// from the body is left alone, so sending only `license` does not touch the
// README. Returns the re-read repo view (same shape as getRepo).
async function updateRepo(repo: string, body: { description?: string; license?: string }): Promise<RepoDetails> {
  return fetchJSON<RepoDetails>(repoBase(repo), {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// renameRepo POSTs /api/v1/repos/{repo}/rename. A custom action, not a PATCH:
// the rename invalidates the URL the request was addressed by, so the response
// is the repo re-read under its NEW name. Callers must stop using the old name
// the moment this resolves.
async function renameRepo(repo: string, name: string): Promise<RepoDetails> {
  return fetchJSON<RepoDetails>(`${repoBase(repo)}/rename`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  });
}

// renameLens POSTs /api/v1/lenses/{lens}/rename — the lens counterpart of
// renameRepo above, same reasoning: a custom action rather than a PATCH
// because the rename invalidates the URL the request was addressed by, and
// the response is the lens re-read under its NEW name.
async function renameLens(lens: string, name: string): Promise<Lens> {
  return fetchJSON<Lens>(`${lensBase(lens)}/rename`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name }),
  });
}

// LensMember identifies one member repo of a lens. `uid` is the registry key —
// the ONLY spelling requests may send, and the one thing that survives a rename.
// `name` is derived by the server and read-only: it is here so the UI can render
// a lens without a second fetch, and it is never what gets sent back.
export interface LensMember { uid: string; name: string }
// LensRead is one read-mount of a lens: a member, optionally pinned to a
// branch and/or a source label (the server fills defaults when omitted).
export interface LensRead extends LensMember { branch?: string; source?: string }
// LensMemberRef is what a REQUEST carries for a member: the uid alone. Spelled
// as its own type so a response object (which also carries `name`) can be passed
// where one is expected, but never the other way round.
export interface LensMemberRef { uid: string }
export interface LensReadRef extends LensMemberRef { branch?: string; source?: string }
// Lens is the composed view: writes land in `write`, reads union `reads`.
// `reads` is server-ordered by member name and always includes the write repo.
// created_at/updated_at are unix seconds, present on server responses.
export interface Lens {
  name: string; write: LensMember; reads: LensRead[];
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
  types: Record<string, number>;
  highlights: Highlight[];
  default_axis: Exclude<RankAxis, 'recent'>;
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
  _links?: { target?: { href: string } };
}

export interface Fact { path: string; title: string; kind?: string; type?: string; origin?: string; body: string; domain: string[]; confidence: number; sources: number; entities: string[]; motifs?: string[]; refs: FactRef[]; ref_warnings?: string[]; parse_error?: string; from_commit?: string; commit_hash?: string; commit_date?: string }

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
        : ({ raw: r.raw, kind: r.kind, path: r.path, _links: r._links } as FactRef));
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
    // Omitted on a fact carrying none, which is most of them — so this is
    // `undefined` far more often than it is a list, and every reader of it
    // has to treat absence as the ordinary case rather than as missing data.
    motifs: data.motifs,
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
export interface RecentFactEntry { path: string; title: string; kind?: string; type?: string; committed_at: number; operation?: string; score?: number;
  /** The spellings this fact carries. Free on every collection row, and what
   *  lets a widened pivot mark the rows a looser tier let in — a row carrying
   *  none of the cluster's members is here on a technicality, not as a carrier. */
  motifs?: string[] }
export interface RecentResponse { facts: RecentFactEntry[]; total: number }
export interface CommitFile { path: string; action: string; title?: string }
export interface CommitAuthor { name: string; email: string }
export interface CommitDetail { commit: string; date: string; message: string; operation?: string; author?: CommitAuthor; files: CommitFile[] }
// RankAxis is the highlights ordering. All three are server-side rankings;
// 'recent' is requestable but never returned as default_axis.
export type RankAxis = 'impact' | 'confidence' | 'recent';

// Highlight is one row of the overview's highlights list. `impact` is the
// count of facts this one was derived from, and is GLOBAL: it does not change
// when the view is scoped to a folder. There is no commit: highlights list
// live facts and open live, like a Library row.
export interface Highlight {
  path: string;
  title: string;
  type: string;
  confidence: number;
  impact: number;
  committed_at: number;
}

export interface Stats {
  total: number;
  domains: Record<string, number>;
  entities: Record<string, number>;
  avg_confidence: number;
  types: Record<string, number>;
  highlights: Highlight[];
  default_axis: Exclude<RankAxis, 'recent'>;
}
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

  // The recognised chip categories — the same set in every context. `repo:` is
  // NOT among them: mount scope is state.lensSources, not a filter chip.
  // Kept in lockstep with FilterChip['category'] in state.ts — the union and
  // this alternation are the same set written twice, and a category present in
  // one but not the other is a chip you can hold but never type (or vice versa).
  const cats = 'domain|entity|type|kind|origin|ep|path|motif';
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
/**
 * An Error carrying the HTTP status that produced it, so a caller can tell a
 * 404 ("no such thing there") from a transport failure or a 500. The status is
 * ADDED to an ordinary Error rather than carried by a subclass so `String(err)`
 * stays byte-identical for every existing consumer that renders it.
 */
export interface HttpError extends Error { status?: number }

function httpError(status: number, message: string): HttpError {
  const e = new Error(message) as HttpError;
  e.status = status;
  return e;
}

/** True when err is an HTTP 404 — a missing resource, not a failed request. */
export function isNotFound(err: unknown): boolean {
  return (err as HttpError | null)?.status === 404;
}

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

// RepoCreateStatus is the state of one detached repo-create job — the body of
// the 202 that POST /repos answers with, and of every poll of
// /repo-creates/{id}. One shape for both, so the 202 IS the first poll result.
//
// step/message/pct are a LATEST-VALUE snapshot, not a stream: a poll landing
// between two steps reports the earlier one, and no intermediate step is
// guaranteed to be seen by anyone. Anything that needs every step must derive
// it from the known pipeline, not from what it happened to observe.
export interface RepoCreateStatus {
  create_id: string;
  name: string;
  mode: string;
  state: 'running' | 'done' | 'failed';
  step?: string;
  message?: string;
  pct?: number;
  /** Present only when state is 'done'. */
  repo?: { name: string };
  /** Present only when state is 'failed'. */
  error?: string;
  /** Present only when state is 'failed': the create's own deadline expired. */
  timed_out?: boolean;
}

export interface CreateRepoBody {
  name: string;
  /**
   * `preset`/`custom` are local-only. The two REMOTE modes are the two halves
   * of one question about the chosen branch — does it already carry
   * `.knomit/ontology.yaml`? — which api.probeInitialized answers:
   *
   *   `clone`      joins a branch that HAS one. Its ontology governs, and the
   *                backend REFUSES an ontology_preset/ontology_yaml here
   *                rather than silently dropping it.
   *   `initialize` turns a branch that has NONE into a knowledge base: it
   *                carries the chosen ontology, which knomit commits to its
   *                own agent branch and pushes. The consensus branch is never
   *                written.
   *
   * There is no mode for an empty remote. knomit never creates a branch on a
   * remote other than its own agent branch, so a remote with no branches is a
   * blocked state the wizard reports rather than a case it handles.
   */
  mode: 'preset' | 'custom' | 'clone' | 'initialize';
  ontology_preset?: string;
  ontology_yaml?: string;
  origin?: { url: string; branch?: string; auth_method?: string; auth_token?: string };
}

/**
 * InitializedResult is the response of POST /api/v1/repos:probe-initialized —
 * "does THIS BRANCH of this remote already hold a knomit knowledge base?"
 *
 * Separate from ProbeResult because the answer is per-branch (a repo can carry
 * the ontology on main and not on develop) and the branch is not known when the
 * origin probe runs.
 */
export interface InitializedResult {
  /**
   * THREE STATES, and the third must never be collapsed into either other.
   *
   *   'yes'      the branch is a knowledge base → mode 'clone'
   *   'no'       it is not → mode 'initialize'
   *   undefined  THE CHECK DID NOT COMPLETE, and nothing was established
   *
   * `undefined` is the absent field, not the empty string — the backend omits
   * it precisely so a client that forgets this case reads undefined rather
   * than something that looks like an answer.
   *
   * Guessing either way is unrecoverable, because a repo's ontology is fixed at
   * create time and never editable afterwards: guess 'yes' and the ontology the
   * user chose is discarded; guess 'no' and one is written over a knowledge base
   * that already had its own. Block and offer a retry instead.
   */
  initialized?: 'yes' | 'no';
  /**
   * The branch actually inspected, which is NOT always the one asked about.
   *
   * A create reads whatever it adopts: this machine's agent branch when the
   * remote already carries one, otherwise the branch named. The probe mirrors
   * that rule, so this names the branch the answer is really about — the branch
   * step shows it, because "main already holds a knowledge base" is false when
   * the ontology is on agent/<host> and main has none.
   */
  branch?: string;
  /** Which knowledge base it is — the id of the ontology found. Only when `initialized` is 'yes'. */
  ontology_id?: string;
  /** Why the answer is absent. Only present for the unestablished case. */
  detail?: string;
}

// ProbeResult is the response of POST /api/v1/repos:probe-origin — the wizard's
// probe of a candidate remote before committing to clone/seed it. `branches` is
// always a JSON array from the server, never null. `detail` carries a
// human-readable reason when `reachable` is false (or auth is required).
export interface ProbeResult {
  reachable: boolean;
  empty: boolean;
  auth_required: boolean;
  upstream_branch: string;
  branches: string[];
  detail?: string;
  /**
   * May knomit PUSH here? Reading and writing are authorized separately —
   * a ref listing speaks git-upload-pack, a push speaks git-receive-pack — so
   * a remote can answer a read probe and still refuse the first commit.
   *
   * '' / absent means NOT ESTABLISHED, which is a third state and must never
   * be rendered as either answer. 'denied' is advisory, never a gate.
   */
  write_access?: '' | 'ok' | 'denied';
  write_detail?: string;
}

// OntologyDiagnostic is one parse/validation error from POST
// /api/v1/ontologies:validate, line/column 1-based into the submitted YAML.
export interface OntologyDiagnostic { line: number; column: number; message: string }

// OntologyValidation is the response of POST /api/v1/ontologies:validate.
// Discriminated on `ok` rather than an all-optional bag: on success all four
// remaining keys are always present, including `rule_count: 0` — the backend
// dropped `omitempty` from these fields specifically so a real zero survives
// the wire. Typing them optional would let `if (result.rule_count)` silently
// mistreat that zero as absent, the same class of bug the wire fix closed.
export type OntologyValidation =
  | { ok: true; id: string; name: string; topics: string[]; rule_count: number }
  | { ok: false; diagnostics: OntologyDiagnostic[] };

// OntologyPreset is one row of GET /api/v1/ontologies/presets.
export interface OntologyPreset {
  name: string; id: string; title: string; description: string; topics: string[];
}

// OntologyField is one row of GET /api/v1/ontologies/schema — the struct/field
// pairs a custom ontology's rules may reference, with their doc string.
export interface OntologyField { struct: string; field: string; doc: string }

export interface ArchivedRepo {
  /** The repo's registry uid — the key restore and purge take. */
  id: string;
  name: string;
  origin: string;
  archivedAt: string;
  /** On-disk size of the archived database. An archived repo's file is named
   *  for its uid and never moves, so this is the only place the disk a purge
   *  would reclaim is visible. Optional: an older server omits it. */
  sizeBytes?: number;
}

// createRepoPollMs is how often createRepo asks how a create is going.
//
// It is a RESPONSIVENESS choice, not a correctness one: the create finishes
// when it finishes regardless of whether anyone is asking. Short enough that a
// clone's progress reads as live, long enough not to hammer the server for the
// minutes a large clone can take.
const createRepoPollMs = 400;

// createRepo starts a repo create and follows it to its terminal state,
// invoking onStatus each time the reported status changes.
//
// The POST answers 202 immediately and does NOT hold the work (issue #67):
// the server runs the create on its own bounded context, so nothing here —
// abandoning the poll, reloading the page, losing the network — can cancel a
// create that is already under way. This function is an OBSERVER, and if it
// stops observing the create still lands.
//
// Resolves with the terminal status (state 'done' or 'failed'); a failed
// create resolves rather than throwing, because "the create failed" is an
// outcome the caller renders, not an exception. It throws only when the
// request itself was refused (problem+json) or the job became unreadable.
async function createRepo(
  body: CreateRepoBody,
  onStatus: (s: RepoCreateStatus) => void,
): Promise<RepoCreateStatus> {
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
  let status = await r.json() as RepoCreateStatus;
  onStatus(status);
  while (status.state === 'running') {
    await new Promise(res => setTimeout(res, createRepoPollMs));
    // A poll that fails is NOT a create that failed — the create is on the
    // server and unaffected. Surfacing it as a create failure would report a
    // repo as broken that is very likely fine, so a lost poll just retries.
    try {
      status = await fetchJSON<RepoCreateStatus>(
        apiUrl(`/api/v1/repo-creates/${status.create_id}`));
      onStatus(status);
    } catch { /* transient; poll again */ }
  }
  return status;
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
async function createLens(body: { name: string; write: LensMemberRef; reads: LensReadRef[]; description?: string }): Promise<Lens> {
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

/** How strictly a motif filter matches. `exact` already matches at the CLUSTER
 *  level — pivoting on one spelling includes facts carrying any aliased member —
 *  so the two looser tiers are for deliberate exploration, not for reaching the
 *  rest of a cluster. The server rejects anything looser than these. */
export type MotifMatch = 'exact' | 'stem' | 'token-2';

/** The motif filter, written once for the four list endpoints that take it.
 *
 *  CSV in a single param, like `type` and `domain`: the server reads it with
 *  splitCSV, so two motif chips WIDEN the match. Four hand-written copies would
 *  be four chances to drift, and api.recent's own comment below records what one
 *  such drift already cost — two type chips collapsing to `undefined` and
 *  silently removing all type filtering, on a list that still rendered fine.
 *
 *  `exact` is the server's default and is deliberately NOT sent. A widened list
 *  contains rows that are not carriers of the motif, so the widened state has to
 *  be legible; an always-present `motif_match=exact` would put the ordinary case
 *  and the loosened one in the same shape.
 *
 *  A tier without motifs is meaningless, so both are gated on the CSV existing.
 */
function setMotifParams(p: URLSearchParams, opts?: { motifs?: string[]; motifMatch?: MotifMatch }): void {
  if (!opts?.motifs?.length) return;
  p.set('motifs', opts.motifs.join(','));
  if (opts.motifMatch && opts.motifMatch !== 'exact') p.set('motif_match', opts.motifMatch);
}

/** One cluster in the /motifs collection. `cluster_key` is the STABLE identity
 *  (URLs key on it); `canonical` is the most-used spelling and is what a reader
 *  is shown — keys look like stemmed token strings ("drift-config") and read as
 *  wrong-order nonsense, which is why `canonical` exists. Never show a bare key.
 *
 *  `df` is the VOCABULARY count: live facts carrying any member, counted once
 *  each. It is not `carrier_count` (below) and the two must not be conflated —
 *  df is a share of the vocabulary, carrier_count is a promise about a query. */
export interface MotifEntry {
  cluster_key: string;
  canonical: string;
  members: string[];
  df: number;
  /** `df` ignoring the `path` scope — the branch-wide count. Equal to `df` on an
   *  unscoped read, and always sent by a current server. It is what lets a
   *  path-scoped row say how much of a shape is HERE and how much the pivot will
   *  return: the pivot drops the path, because a motif cuts across the ontology.
   *  Optional so an older server's page still types; fall back to `df`. */
  df_total?: number;
  definition?: string;
  /** `stale` is an INTERIM state, not an error: membership moved since the
   *  sentence was written and it is still served. `missing` is an absence. */
  definition_state?: 'current' | 'stale' | 'missing';
}

/** The corpus-health header on the collection. Counted over AUTHORED facts only
 *  and NOT narrowed by `q` — it describes the vocabulary, not the result list
 *  sitting under it. `recurrence_rate` and `mint_to_link_ratio` are the two that
 *  say whether names are being reused or every fact is minting its own. */
export interface MotifHealth {
  authored_clusters: number;
  authored_recurring: number;
  authored_mints: number;
  authored_links: number;
  authored_epistemic_recurring: number;
  recurrence_rate: number;
  mint_to_link_ratio: number;
}

export interface MotifCarrier {
  path: string;
  title: string;
  type?: string;
  committed_at: number;
}

/** How a spelling joined its cluster. `judge` merges carry a written rationale —
 *  the provenance surface for "why are these the same motif". Their presence is
 *  also load-bearing elsewhere: a cluster with NO judge alias cannot have a
 *  looser `stem` match than `exact`, which is how the widen control knows a rung
 *  would add nothing without asking the server. */
export interface MotifAlias {
  motif: string;
  method: string;
  rationale?: string;
}

export interface MotifCluster extends MotifEntry {
  /** The number of facts the pivot actually returns — the number the UI shows
   *  beside a name, because that is the promise the row makes. */
  carrier_count: number;
  /** Most-recent-first and CAPPED (20 by default): a preview, not the list.
   *  Anything derived from it is approximate; `carrier_count` is the total. */
  carriers: MotifCarrier[];
  aliases: MotifAlias[];
}

/** The server caps `limit` at 200 and 400s anything larger rather than clamping
 *  silently (internal/web/params.go). Clamping here keeps a caller's optimism
 *  from becoming a failed request. */
const MOTIFS_MAX_LIMIT = 200;

const EMPTY_MOTIF_HEALTH: MotifHealth = {
  authored_clusters: 0, authored_recurring: 0, authored_mints: 0,
  authored_links: 0, authored_epistemic_recurring: 0,
  recurrence_rate: 0, mint_to_link_ratio: 0,
};

/** The vocabulary query. `repos` is lens-only (repeated `repo=`, narrowing the
 *  fan-out); the repo endpoint has no mounts to narrow and ignores it. */
export interface MotifsQuery {
  q?: string; path?: string; sort?: 'df' | 'name';
  limit?: number; offset?: number; repos?: string[];
}

export interface MotifsPage { count: number; health: MotifHealth; motifs: MotifEntry[] }

/** The vocabulary reads, written ONCE against a base URL.
 *
 *  A lens's motif vocabulary is the SAME resource as a repo's — same query,
 *  same envelope, same defaults — over a bigger corpus, and the server keeps
 *  it that way deliberately (one shared renderer behind both handlers). Two
 *  clients kept in step would be two chances to drift on the shape the server
 *  went out of its way to make single; the base URL is the only difference,
 *  so it is the only parameter. */
function fetchMotifs(base: string, opts?: MotifsQuery): Promise<MotifsPage> {
  const p = new URLSearchParams();
  if (opts?.q) p.set('q', opts.q);
  // `path` is SCOPE, not a filter over the page — it says which corpus the
  // vocabulary is of, exactly as it does on /stats, and it narrows the health
  // block along with the list. `q` is a way of reading one page and does not.
  if (opts?.path) p.set('path', opts.path);
  if (opts?.sort) p.set('sort', opts.sort);
  if (opts?.limit !== undefined) p.set('limit', String(Math.min(opts.limit, MOTIFS_MAX_LIMIT)));
  if (opts?.offset) p.set('offset', String(opts.offset));
  for (const repo of opts?.repos ?? []) p.append('repo', repo);
  const qs = p.toString();
  return fetchJSON<any>(`${base}/motifs${qs ? `?${qs}` : ''}`).then(data => ({
    count: data.count ?? 0,
    health: data.health ?? EMPTY_MOTIF_HEALTH,
    motifs: data._embedded?.motifs ?? [],
  }));
}

function fetchMotifCluster(base: string, key: string): Promise<MotifCluster> {
  return fetchJSON<any>(`${base}/motifs/${encodeURIComponent(key)}`).then(data => ({
    ...data,
    members: data.members ?? [],
    carriers: data.carriers ?? [],
    aliases: data.aliases ?? [],
  }));
}

// listLensFacts GETs /api/v1/lenses/{lens}/facts — the recency-ordered, deduped
// union of the lens's write repo + read mounts. Flat envelope ({facts,total});
// each row carries a canonical `path` and its `source` mount. `repos` maps to
// repeated `repo=` params (narrows the fan-out); omitted params are dropped.
async function listLensFacts(lens: string, opts: {
  path?: string; query?: string; limit?: number; offset?: number; repos?: string[];
  types?: string[]; kinds?: string[]; origins?: string[]; eps?: string[]; domains?: string[]; entities?: string[];
  motifs?: string[]; motifMatch?: MotifMatch;
}): Promise<{ facts: LensFactEntry[]; total: number }> {
  const p = new URLSearchParams();
  if (opts.path) p.set('path', opts.path);
  if (opts.query) p.set('query', opts.query);
  if (opts.limit !== undefined) p.set('limit', String(opts.limit));
  if (opts.offset !== undefined) p.set('offset', String(opts.offset));
  // The content filters, in the same names /search uses. The handler forwards
  // every selecting filter to each mount, so a filtered union list is paged and
  // counted like an unfiltered one — which is what makes a facet click a browse
  // rather than a search.
  if (opts.types?.length) p.set('type', opts.types.join(','));
  if (opts.kinds?.length) p.set('kind', opts.kinds.join(','));
  if (opts.origins?.length) p.set('origin', opts.origins.join(','));
  if (opts.eps?.length) p.set('ep', opts.eps.join(','));
  if (opts.domains?.length) p.set('domain', opts.domains.join(','));
  if (opts.entities?.length) p.set('entities', opts.entities.join(','));
  setMotifParams(p, opts);
  for (const repo of opts.repos ?? []) p.append('repo', repo);
  const qs = p.toString();
  return fetchJSON<{ facts: LensFactEntry[]; total: number }>(`${lensBase(lens)}/facts${qs ? `?${qs}` : ''}`);
}

// lensSearch GETs /api/v1/lenses/{lens}/search — the RRF-fused union relevance
// search. The envelope is flat ({results,total}); this returns just the results
// array (each row canonical path + source). `repos` → repeated `repo=` params.
// `opts` forwards the same content filters the repo /search sends. The lens
// FACTS handler accepts the identical set, so filter-bearing reads no longer
// have to come through here — this path is now for RANKING (a text query), and
// a bare chip goes to listLensFacts where it can be paged and counted.
async function lensSearch(
  lens: string, q: string, repos?: string[],
  opts?: { path?: string; types?: string[]; kinds?: string[]; origins?: string[]; eps?: string[]; domains?: string[]; entities?: string[];
           motifs?: string[]; motifMatch?: MotifMatch },
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
  setMotifParams(p, opts);
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
async function getLensStats(lens: string, path: string, axis?: RankAxis, repos?: string[]): Promise<LensStats> {
  const p = new URLSearchParams({ path });
  if (axis) p.set('axis', axis);
  // Same repeated `repo=` narrowing the facts and search unions use — the stats
  // handler runs the identical narrowByRepo. Omitted entirely for the "all
  // mounts" selection so the server fans out; an EMPTY selection must never
  // reach here, since no params reads as "all" and the dashboard would answer
  // with every mount the reader just switched off.
  for (const repo of repos ?? []) p.append('repo', repo);
  return fetchJSON<LensStats>(`${lensBase(lens)}/stats?${p}`);
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
async function updateLens(name: string, body: { write?: LensMemberRef; reads?: LensReadRef[]; description?: string }): Promise<Lens> {
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
  renameRepo,

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

  // signal is how the wizard's first step stays interactive: the server bounds
  // the probe by its own network timeout, but that budget is measured in
  // minutes and the user should not have to wait it out to correct a typo.
  probeOrigin: (body: { url: string; branch?: string; auth_method?: string; auth_token?: string },
    signal?: AbortSignal): Promise<ProbeResult> =>
    fetchJSON<ProbeResult>(apiUrl('/api/v1/repos:probe-origin'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    }),

  // The per-BRANCH half of the classification, and the one that decides the
  // create mode. Heavier than probeOrigin — a shallow single-branch clone
  // rather than a ref listing — so it takes a signal for the same reason:
  // the step stays interactive while it runs.
  probeInitialized: (body: { url: string; branch?: string; auth_method?: string; auth_token?: string },
    signal?: AbortSignal): Promise<InitializedResult> =>
    fetchJSON<InitializedResult>(apiUrl('/api/v1/repos:probe-initialized'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    }),

  validateOntology: (yamlText: string): Promise<OntologyValidation> =>
    fetchJSON<OntologyValidation>(apiUrl('/api/v1/ontologies:validate'), {
      method: 'POST',
      headers: { 'Content-Type': 'text/yaml' },
      body: yamlText,
    }),

  ontologyPresets: (): Promise<OntologyPreset[]> =>
    fetchJSON<{ presets: OntologyPreset[] }>(apiUrl('/api/v1/ontologies/presets'))
      .then(d => d.presets),

  // ontologyPresetYAML GETs a single preset's raw YAML body. This endpoint
  // returns text/yaml, not JSON, so it needs a plain fetch with its own
  // non-OK check rather than fetchJSON (which assumes a JSON body).
  ontologyPresetYAML: async (name: string): Promise<string> => {
    const r = await fetch(apiUrl(`/api/v1/ontologies/presets/${encodeURIComponent(name)}`));
    if (!r.ok) throw new Error(`preset ${name} → ${r.status}`);
    return r.text();
  },

  ontologySchema: (): Promise<OntologyField[]> =>
    fetchJSON<{ fields: OntologyField[] }>(apiUrl('/api/v1/ontologies/schema'))
      .then(d => d.fields),

  listLenses,
  getLens,
  createLens,
  updateLens,
  deleteLens,
  renameLens,
  /** The per-repo motif vocabulary. `q` narrows over member spellings AND
   *  definition text — which is what the browser's "Search names and meanings"
   *  placeholder is promising, and what its sibling facet boxes cannot do.
   *  `path` scopes the whole answer to one subtree: a cluster no fact there
   *  carries is absent, `df` counts the carriers under it, and `health` is
   *  counted over the same facts. */
  motifs: (repo: string, branch: string, opts?: MotifsQuery): Promise<MotifsPage> =>
    fetchMotifs(branchBase(repo, branch), opts),

  /** The lens-wide motif vocabulary: every mount's clusters merged into one
   *  list, in the repo endpoint's own envelope. `repos` narrows the fan-out to
   *  the named mounts, like every other lens union read. */
  lensMotifs: (lens: string, opts?: MotifsQuery): Promise<MotifsPage> =>
    fetchMotifs(lensBase(lens), opts),

  /** One cluster. `key` accepts the cluster_key or any member spelling, and is
   *  encoded because neither is guaranteed URL-safe. Carriers and aliases
   *  default to [] so a caller never has to guard the shape — but note that an
   *  empty `carriers` with a non-zero `carrier_count` is a real state (a preview
   *  the server chose not to send), not a contradiction to paper over. */
  motifCluster: (repo: string, branch: string, key: string): Promise<MotifCluster> =>
    fetchMotifCluster(branchBase(repo, branch), key),

  /** One MERGED cluster through a lens. `key` additionally accepts any single
   *  mount's own cluster_key, because a merged cluster's key is the smallest of
   *  its constituents' and a reader can hold either. Carrier paths come back in
   *  the canonical lens form (bare for the write mount, kb://<id12>/… for a
   *  read mount), so they are openable through the lens as they arrive. */
  lensMotifCluster: (lens: string, key: string): Promise<MotifCluster> =>
    fetchMotifCluster(lensBase(lens), key),

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
    opts?: { types?: string[]; kinds?: string[]; excludeKinds?: string[]; origins?: string[]; eps?: string[]; domains?: string[]; entities?: string[];
             motifs?: string[]; motifMatch?: MotifMatch }
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
    setMotifParams(p, opts);
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

  stats: (repo: string, branch: string, path: string, axis?: RankAxis): Promise<Stats> => {
    const p = new URLSearchParams({ path });
    if (axis) p.set('axis', axis);
    return fetchJSON<Stats>(`${branchBase(repo, branch)}/stats?${p}`);
  },

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
    opts?: { types?: string[]; excludeType?: string; kinds?: string[]; excludeKinds?: string[]; origins?: string[]; domains?: string[]; entities?: string[]; eps?: string[];
             motifs?: string[]; motifMatch?: MotifMatch }
  ): Promise<RecentResponse> => {
    const p = new URLSearchParams({ sort: 'recent', path, limit: String(limit), offset: String(offset) });
    if (query) p.set('q', query);
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
    setMotifParams(p, opts);
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

  // NOTE: there is deliberately no `setOrigin` here. PUT /repos/{repo}/origin
  // is driven entirely by the connect wizard's session flow
  // (handlers_origin_session.go), and the dead client wrapper that used to sit
  // here carried an OriginSetResponse with `branch` and `head` fields the
  // handler has never returned — a shape nobody could have relied on without
  // finding out the hard way.

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
    // When commit is set, use the commit-anchored sub-resource endpoints so the
    // refs are THIS version's.
    //
    // Both routes end in the same version-aware primitives — the HEAD route's
    // ExplainFact resolves the path's HEAD-active commit out of branch_facts
    // and then calls IncomingAtCommit/OutgoingAtCommit itself — so the anchor
    // does not decide whether edges carry a target_commit. What it decides is
    // WHICH VERSION OF THIS FACT the edges belong to: HEAD-active, or the one
    // live at the pinned commit. Reading an older version through the HEAD
    // route would therefore show the HEAD version's refs, and a fact retracted
    // at HEAD would show none at all (ErrFactNotLive → 404).
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
    // `dir` decides whether recency may order the entries, because the two
    // directions are not the same question:
    //
    //   outgoing — `commit` is the edge's TARGET_COMMIT: the version of the
    //     target this fact reasoned over. A correctly-indexed source version
    //     has exactly ONE edge per target, so there is nothing to order, and
    //     imposing "newest wins" on the cases that DO carry two would pick the
    //     target's later version — the HEAD-resolution
    //     kb/principles/philosophy/historical-not-current forbids. Backend
    //     order is preserved and untouched.
    //
    //   incoming — `commit` is the edge's SOURCE_COMMIT: which version of
    //     another fact cites this one. Several versions of one source citing
    //     the same target is the intended multi-edge case
    //     (internal/store/edge_props.go:11), and "who cites me" is a question
    //     about the present, so the most recent citing version leads.
    const groupRefs = (refs: RawRef[], dir: 'incoming' | 'outgoing'): RefGroup[] => {
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
        // See the `dir` note above: outgoing keeps backend order, incoming
        // leads with the most recent citing version.
        const sorted = dir === 'outgoing'
          ? g.entries
          : [...g.entries].sort((a, b) => {
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
        // Every group-level field comes from the SAME entry the group is
        // pinned to, so a row describes one edge coherently: this target, at
        // this target_commit, with that version's title and tombstone state.
        // Taking `deleted` from a different (later) entry than the pinned
        // commit would mark a row retracted on the strength of a version the
        // referrer never saw.
        const leadRef = sorted[0]?.ref;
        return {
          path: g.path,
          title: leadRef?.title ?? '',
          kind: leadRef?.kind,
          type: leadRef?.type,
          versions,
          deleted: leadRef?.deleted ?? false,
        };
      });
    };
    return Promise.all([
      fetch(`${factURL}/incoming${edgeQuery}`).then(r => r.ok ? r.json() : r.json().then((e: unknown) => { throw httpError(r.status, errorText(e, r.statusText)); })),
      fetch(`${factURL}/outgoing${edgeQuery}`).then(r => r.ok ? r.json() : r.json().then((e: unknown) => { throw httpError(r.status, errorText(e, r.statusText)); })),
    ]).then(([inc, out]) => ({
      incoming: groupRefs(parseRefs(inc), 'incoming'),
      outgoing: groupRefs(parseRefs(out), 'outgoing'),
    }));
  },
};
