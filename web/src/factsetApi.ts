// The FactSet view's whole contact with the server, behind one small interface
// (FactSetApi) so the view, its stream and their tests never touch fetch.
//
// WIRE SHAPE. A page is materialize.FactSetPage (internal/corpus/materialize/
// factset.go), the same JSON knomit_materialize's factset mode returns: an
// envelope repeated on every page plus whole sections. A node is a
// materialize Node (MatNode here) plus its place in the FactSet.
//
// ROUTES. A repo list: /repos/{repo}/branches/{branch}/search/factset; a lens
// list (of any size, a one-member lens included): /lenses/{lens}/search/factset.
// The first call carries the list's own search (listSearchParams /
// lensListSearchParams), `sort`, and the FactSet's options; a later call carries
// ONLY the cursor (and `unit` to seek). Export and close take only the cursor.

import { branchBase, lensBase } from './api';
import { readDocument } from './materializeApi';
import type { MatExport, MatNode, MatOptions, MatTruncation } from './materializeApi';

/** Page size asked for on the first call (the server's maximum, 1..5). */
export const FS_PAGE_LIMIT = 5;

/** Where a fact appearing as a heading only is written. A forward link names
 *  the unit and number; a back link names the unit that FIRST wrote it and no
 *  number (ruling 2026-11-02: links show the target's title, not a number). */
export interface FactSetLink {
  unit?: number;
  number?: string;
  anchor_id: string;
  direction: 'forward' | 'back';
  cited_blob: string;
  /** The version cited here is not the version written (any change). */
  different_version?: boolean;
}

export interface FactSetNode extends MatNode {
  blob?: string;
  unit: number;
  /** Lens pages only, on unit roots: the member the root comes from. */
  repo_name?: string;
  /** "5", "5.1", "5.1.2"; '' for a frame's ancestor. */
  number: string;
  /** Depth in the unit's walk tree (not the BFS `depth`). */
  tree_depth: number;
  parent?: number;
  written: boolean;
  link?: FactSetLink;
  /** A unit root with no version at the anchor: a heading alone. */
  unavailable?: boolean;
  unit_truncated?: MatTruncation;
}

export interface FactSetPage {
  mode: string;
  title: string;
  search_line: string;
  anchor: string;
  /** Lens pages only: each searched member's id12 -> its anchor. */
  anchors?: Record<string, string>;
  unpinned: boolean;
  cross_kb: boolean;
  depth: number;
  max_nodes: number;
  facts_limit: number;
  units: number;
  facts: FactSetNode[];
  unit_from: number;
  unit_to: number;
  /** Set on every page of a non-empty FactSet; null when it is empty. */
  cursor: string | null;
  has_more: boolean;
}

/** The FactSet's route: a repo branch or a lens. */
export type FsTarget =
  | { kind: 'repo'; repo: string; branch: string }
  | { kind: 'lens'; lens: string };

/** A FactSet's search, frozen when it was opened: the route, the list's order,
 *  and the list's own parameters (a query string without sort/offset/limit). */
export interface FsSearch {
  target: FsTarget;
  sort: 'relevance' | 'recent';
  params: string;
}

export type FsErrorKind = 'busy' | 'expired' | 'invalid' | 'failed';

/** A failed call: kind from the status (503 busy, 404 expired, 400/422
 *  invalid, anything else or no response failed), Retry-After in seconds. */
export class FsError extends Error {
  status?: number;
  kind: FsErrorKind;
  retryAfter?: number;
  detail: string;
  constructor(detail: string, status?: number, retryAfter?: number) {
    super(detail);
    this.name = 'FsError';
    this.status = status;
    this.detail = detail;
    this.retryAfter = retryAfter;
    this.kind = status === 503 ? 'busy'
      : status === 404 ? 'expired'
      : status === 400 || status === 422 ? 'invalid'
      : 'failed';
  }
}

export interface FactSetApi {
  /** The first call: a new FactSet of search at options. */
  first(search: FsSearch, options: MatOptions, signal?: AbortSignal): Promise<FactSetPage>;
  /** The page after cursor. */
  next(target: FsTarget, cursor: string, signal?: AbortSignal): Promise<FactSetPage>;
  /** The page starting at unit (any live cursor of the FactSet). */
  seek(target: FsTarget, cursor: string, unit: number, signal?: AbortSignal): Promise<FactSetPage>;
  /** The WHOLE FactSet as markdown; resolves only with the complete document. */
  exportMarkdown(target: FsTarget, cursor: string, signal?: AbortSignal, onProgress?: (bytes: number) => void): Promise<MatExport>;
  /** Closes the FactSet (best effort: a gone one is not an error). */
  close(target: FsTarget, cursor: string): Promise<void>;
}

export function factsetBase(t: FsTarget): string {
  return `${t.kind === 'repo' ? branchBase(t.repo, t.branch) : lensBase(t.lens)}/search/factset`;
}

/** The first call's query: the list's parameters, the order, and the options.
 *  `facts` is never sent (the default, every result up to 500, applies), and
 *  cross_kb never on a lens (a lens refuses it). */
export function firstParams(search: FsSearch, o: MatOptions): URLSearchParams {
  const p = new URLSearchParams(search.params);
  p.set('sort', search.sort);
  p.set('depth', String(o.depth));
  p.set('max_nodes', String(o.references + 1));
  if (o.unpin) p.set('unpin', 'true');
  if (o.crossKb && search.target.kind === 'repo') p.set('cross_kb', 'true');
  p.set('limit', String(FS_PAGE_LIMIT));
  return p;
}

async function failure(r: Response): Promise<FsError> {
  let detail = r.statusText;
  try {
    const b = (await r.json()) as { detail?: string; title?: string };
    detail = b.detail || b.title || detail;
  } catch { /* not JSON */ }
  const ra = Number(r.headers.get('Retry-After'));
  return new FsError(detail || `HTTP ${r.status}`, r.status, Number.isFinite(ra) && ra > 0 ? ra : undefined);
}

async function call(url: string, init?: RequestInit): Promise<Response> {
  let r: Response;
  try {
    r = await fetch(url, init);
  } catch (e) {
    if (init?.signal?.aborted) throw e;
    throw new FsError(e instanceof Error ? e.message : String(e));
  }
  if (!r.ok) throw await failure(r);
  return r;
}

async function page(url: string, signal?: AbortSignal): Promise<FactSetPage> {
  const r = await call(url, { signal });
  return (await r.json()) as FactSetPage;
}

export const factsetApi: FactSetApi = {
  first(search, options, signal) {
    return page(`${factsetBase(search.target)}?${firstParams(search, options)}`, signal);
  },
  next(target, cursor, signal) {
    return page(`${factsetBase(target)}?${new URLSearchParams({ cursor })}`, signal);
  },
  seek(target, cursor, unit, signal) {
    return page(`${factsetBase(target)}?${new URLSearchParams({ cursor, unit: String(unit) })}`, signal);
  },
  async exportMarkdown(target, cursor, signal, onProgress) {
    const r = await call(`${factsetBase(target)}/export?${new URLSearchParams({ cursor })}`, { signal });
    return readDocument(r, onProgress);
  },
  async close(target, cursor) {
    try {
      await fetch(`${factsetBase(target)}?${new URLSearchParams({ cursor })}`, { method: 'DELETE' });
    } catch { /* best effort: an idle FactSet expires on its own */ }
  },
};
