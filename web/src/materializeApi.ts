// The materialize view's whole contact with the server, behind one small
// interface so the view, its stream hook and its tests never touch fetch.
//
// WIRE SHAPE. A page mirrors the MCP knomit_materialize human-audience page
// (internal/edge/mcp/materialize.go: materializeEnv + materializePage, with the
// per-fact materialize.Node from internal/corpus/materialize/walk.go): same
// field names, same envelope. The REST cursor route returns that page as JSON.
// `root_title` is the one optional addition (materialize.Document carries it;
// the MCP envelope does not), so the view falls back to the viewed fact's title.

import { apiUrl } from './api';

/** Why a fact was left out: a ref that was not written, with the reason. */
export interface MatOmission { title?: string; id?: string; path: string; reason: string }
/** A non-fact ref: an external URL or a source-code ref. */
export interface MatSource { label: string; url?: string }
/** A ref to a fact written in the same document. different_version: the
 *  citing fact cited a version whose BODY differs from the one written. */
export interface MatLink { n: number; title: string; id: string; anchor_id: string; different_version?: boolean }

/** The roles a node plays in reading order (walk.go Role*). */
export type MatRole = 'root' | 'ancestor' | 'focal' | 'ref' | (string & {});

/** The mark a node carries when reached through a cross-KB ref. */
export const MARK_LATEST_OTHER_KB = 'latest_other_kb';

export interface MatNode {
  n: number;
  path: string;
  repo: string;
  commit: string;
  depth: number;
  role: MatRole;
  title: string;
  type: string;
  kind: string;
  confidence: number;
  date?: string;
  body?: string;
  builds_on?: MatLink[];
  not_included?: MatOmission[];
  sources?: MatSource[];
  marks?: string[];
  anchor_id: string;
  /** The heading written before this node (a frame's three groups). */
  section?: string;
}

export interface MatTruncation { depth: number; nodes: number }

export interface MatPage {
  root: string;
  root_title?: string;
  anchor: string;
  order: string;
  audience?: string;
  unpinned: boolean;
  cross_kb: boolean;
  depth: number;
  max_nodes: number;
  /** Exact: the walk is planned up front. */
  total: number;
  truncated: MatTruncation;
  facts: MatNode[];
  n_from: number;
  n_to: number;
  /** null on the last page. */
  cursor: string | null;
  has_more: boolean;
}

/** The options a first call takes. A cursor call takes none of them. */
export interface MatOptions {
  depth: number;
  maxFacts: number;
  crossKb: boolean;
  unpin: boolean;
}

export const DEFAULT_OPTIONS: MatOptions = { depth: 4, maxFacts: 200, crossKb: false, unpin: false };
export const MAX_DEPTH = 10;
export const MAX_FACTS = 500;

/** Which fact, on which branch, as of which commit. */
export interface MatTarget {
  repo: string;
  branch: string;
  /** Repo-relative fact path. */
  path: string;
  /** The viewed commit; '' or undefined for the branch tip. */
  commit?: string;
}

/** A failed call. status is the HTTP status when there was one. */
export class MatError extends Error {
  status?: number;
  constructor(message: string, status?: number) {
    super(message);
    this.name = 'MatError';
    this.status = status;
  }
  /** A cursor the server no longer holds: retrying the same cursor cannot
   *  succeed, only a new first call can. */
  get expired(): boolean { return this.status === 404 || this.status === 410; }
}

export interface MatExport {
  /** The WHOLE document. */
  text: string;
  /** Content-Disposition's filename, when the server sent one. */
  serverName?: string;
}

export interface MaterializeApi {
  /** One page: the first call (cursor null) with options, or a cursor call. */
  page(target: MatTarget, options: MatOptions, cursor: string | null, signal?: AbortSignal): Promise<MatPage>;
  /** The whole materialization as markdown, at the options' depth and count.
   *  Resolves only with the complete document; rejects otherwise.
   *  onProgress is told the bytes received so far. */
  exportMarkdown(target: MatTarget, options: MatOptions, signal?: AbortSignal, onProgress?: (bytes: number) => void): Promise<MatExport>;
}

function factUrl(t: MatTarget): string {
  const enc = (p: string) => p.split('/').map(encodeURIComponent).join('/');
  const branch = t.branch.replaceAll('/', ':');
  const base = `/api/v1/repos/${t.repo}/branches/${branch}`;
  return apiUrl(t.commit ? `${base}/commits/${t.commit}/facts/${enc(t.path)}` : `${base}/facts/${enc(t.path)}`);
}

function optionParams(o: MatOptions): URLSearchParams {
  const p = new URLSearchParams({ depth: String(o.depth), max_nodes: String(o.maxFacts) });
  if (o.unpin) p.set('unpin', 'true');
  if (o.crossKb) p.set('cross_kb', 'true');
  return p;
}

async function failure(r: Response, url: string): Promise<MatError> {
  let detail = r.statusText;
  try {
    const b = (await r.json()) as { detail?: string; title?: string };
    detail = b.detail || b.title || detail;
  } catch { /* not JSON */ }
  return new MatError(`${url} → ${r.status} ${detail}`.trim(), r.status);
}

/** Content-Disposition's filename, if any. */
export function dispositionName(h: string | null): string | undefined {
  if (!h) return undefined;
  const star = /filename\*=(?:UTF-8'')?([^;]+)/i.exec(h);
  if (star) { try { return decodeURIComponent(star[1].trim().replace(/^"|"$/g, '')); } catch { /* fall through */ } }
  const plain = /filename="?([^";]+)"?/i.exec(h);
  return plain?.[1].trim();
}

/** The real API. The cursor route is `<fact URL>/materialize/pages`, and the
 *  agent document the export reads is `<fact URL>/materialize`. */
export const materializeApi: MaterializeApi = {
  async page(target, options, cursor, signal) {
    const p = cursor ? new URLSearchParams({ cursor }) : optionParams(options);
    const url = `${factUrl(target)}/materialize/pages?${p}`;
    const r = await fetch(url, { signal });
    if (!r.ok) throw await failure(r, url);
    return (await r.json()) as MatPage;
  },

  async exportMarkdown(target, options, signal, onProgress) {
    const url = `${factUrl(target)}/materialize?${optionParams(options)}`;
    const r = await fetch(url, { signal });
    if (!r.ok) throw await failure(r, url);
    const serverName = dispositionName(r.headers.get('Content-Disposition'));
    let text: string;
    if (r.body && typeof r.body.getReader === 'function') {
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let got = 0;
      text = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        got += value.byteLength;
        text += dec.decode(value, { stream: true });
        onProgress?.(got);
      }
      text += dec.decode();
      const want = Number(r.headers.get('Content-Length'));
      if (want > 0 && got !== want) throw new MatError(`the document ended early (${got} of ${want} bytes)`);
    } else {
      text = await r.text();
    }
    return { text, serverName };
  },
};
