// A deterministic stand-in for the materialize endpoints, for tests and for
// developing the view without a server. It serves the SAME page shape the
// backend does (materializeApi.ts), cut into pages of `pageSize` facts, and
// records every call so a test can say exactly what was requested.

import { MARK_LATEST_OTHER_KB, MatError, PAGE_LIMIT } from './materializeApi';
import type { MaterializeApi, MatNode, MatOptions, MatPage, MatTarget } from './materializeApi';

export interface FixtureOptions {
  /** Facts in the whole walk (before the count cap). */
  total?: number;
  pageSize?: number;
  /** Cursor calls (1-based, page 2 is the first) that fail once, then succeed. */
  failPageOnce?: number[];
  /** Cursor calls that always fail with this HTTP status. */
  failPageStatus?: Record<number, number>;
  /** Make every call wait for this promise before answering. */
  gate?: () => Promise<void>;
  /** Frames: the first `ancestors` facts are ancestors, then the focal frame. */
  ancestors?: number;
  rootTitle?: string;
  /** Explicit outline: ordinal -> the ordinals it cites (its children first). Without it each
   *  fact cites the one before it. */
  tree?: Record<number, number[]>;
  /** Breadth-first inclusion, depth-first order: a cap ABOVE this count lets a
   *  new fact in at position 2, shifting every fact after it (as a real walk
   *  does when the cap rises). */
  shiftAbove?: number;
  /** Cited facts left unexpanded by the depth limit (the envelope's truncated.depth). */
  depthBeyond?: number;
  /** Page-2 cursors are valid for this options key only (see api.calls). */
  exportText?: string;
  exportFails?: boolean;
  /** The Content-Disposition filename the export answers with, if any. */
  serverName?: string;
}

export interface FixtureCall {
  kind: 'page' | 'export';
  cursor: string | null;
  options: MatOptions;
}

export function fixtureId(n: number): string {
  return (0x1000 * 0x1000 + n * 0x9e37).toString(16).padStart(8, '0').slice(-8);
}

export function fixtureNode(n: number, o: FixtureOptions = {}): MatNode {
  const ancestors = o.ancestors ?? 0;
  const isAncestor = n <= ancestors;
  const isFocal = ancestors > 0 && n === ancestors + 1;
  const depth = ancestors > 0 ? (isAncestor ? 0 : isFocal ? 0 : 1 + ((n - ancestors - 2) % 4)) : (n === 1 ? 0 : 1 + ((n - 2) % 4));
  const id = fixtureId(n);
  const link = (c: number) => ({ n: c, title: `Fact number ${c}`, id: fixtureId(c), anchor_id: `f-${fixtureId(c)}`, different_version: c % 5 === 0 ? false : undefined });
  const builds: MatNode['builds_on'] = o.tree
    ? (o.tree[n] ?? []).map(link)
    : n > 1
    ? [{ n: n - 1, title: `Fact number ${n - 1}`, id: fixtureId(n - 1), anchor_id: `f-${fixtureId(n - 1)}`, different_version: n % 5 === 0 }]
    : undefined;
  return {
    n,
    path: `kb/decisions/fixture/${id}.md`,
    repo: n % 7 === 0 ? 'abcdef012345' : '3ec012f5b4d2',
    commit: `e25e${String(n).padStart(2, '0')}364aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`.slice(0, 40),
    depth,
    role: isAncestor ? 'ancestor' : isFocal ? 'focal' : n === 1 ? 'root' : 'ref',
    title: n === 1 && o.rootTitle ? o.rootTitle : `Fact number ${n}: what it decided and why it matters to the one citing it`,
    type: n % 3 === 0 ? 'policy' : 'observation',
    kind: n % 3 === 0 ? 'pragmatic' : 'epistemic',
    confidence: 0.9,
    date: '2026-10-29',
    body: `Body of fact ${n}. It states one claim in full, with \`inline code\` and a [link](https://example.com/${n}).\n\nA second paragraph for fact ${n}.`,
    builds_on: builds,
    sources: n % 4 === 0 ? [{ label: `file${n}.go (code @040bc5f)` }] : undefined,
    marks: n % 7 === 0 ? [MARK_LATEST_OTHER_KB] : undefined,
    anchor_id: `f-${id}`,
    section: ancestors > 0 ? (isAncestor ? 'Ancestors' : isFocal ? 'This frame' : n === ancestors + 2 ? 'What it stands on' : undefined) : undefined,
  };
}

export function makeFixtureApi(o: FixtureOptions = {}) {
  const total = o.total ?? 12;
  const pageSize = o.pageSize ?? PAGE_LIMIT;
  const calls: FixtureCall[] = [];
  const failed = new Set<number>();
  const nodesFor = (opts: MatOptions): MatNode[] => {
    const cap = Math.min(total, opts.references + 1);
    const out = Array.from({ length: cap }, (_, i) => fixtureNode(i + 1, o));
    if (o.shiftAbove !== undefined && opts.references + 1 > o.shiftAbove && cap >= 2) {
      // a newly admitted fact (identity 900) lands second; the rest move down
      const inserted = fixtureNode(900, o);
      const order = [out[0], inserted, ...out.slice(1, cap - 1)];
      return order.map((node, i) => ({ ...node, n: i + 1 }));
    }
    return out;
  };
  // A cursor is "<options-json>|<next index>" so a cursor call can find its walk.
  const encode = (opts: MatOptions, next: number) => `${JSON.stringify(opts)}|${next}`;
  const page = (opts: MatOptions, from: number): MatPage => {
    const all = nodesFor(opts);
    const facts = all.slice(from, from + pageSize);
    const next = from + facts.length;
    const more = next < all.length;
    return {
      root: `kb/decisions/fixture/${fixtureId(1)}.md`,
      root_title: o.rootTitle,
      anchor: 'e25e364',
      order: o.ancestors ? 'frame' : 'reading',
      unpinned: opts.unpin,
      cross_kb: opts.crossKb,
      depth: opts.depth,
      max_nodes: opts.references + 1,
      total: all.length,
      truncated: { depth: o.depthBeyond ?? 0, nodes: Math.max(0, total - all.length) },
      facts,
      n_from: facts[0]?.n ?? 0,
      n_to: facts[facts.length - 1]?.n ?? 0,
      cursor: more ? encode(opts, next) : null,
      has_more: more,
    };
  };
  const api: MaterializeApi = {
    async page(_t: MatTarget, options: MatOptions, cursor: string | null, signal?: AbortSignal) {
      calls.push({ kind: 'page', cursor, options });
      if (o.gate) await o.gate();
      if (signal?.aborted) throw new DOMException('aborted', 'AbortError');
      if (cursor === null) return page(options, 0);
      const [json, idx] = [cursor.slice(0, cursor.lastIndexOf('|')), Number(cursor.slice(cursor.lastIndexOf('|') + 1))];
      const pageNo = Math.floor(idx / pageSize) + 1;
      const status = o.failPageStatus?.[pageNo];
      if (status) throw new MatError(`page ${pageNo} failed`, status);
      if (o.failPageOnce?.includes(pageNo) && !failed.has(pageNo)) {
        failed.add(pageNo);
        throw new MatError(`page ${pageNo} failed`, 500);
      }
      return page(JSON.parse(json) as MatOptions, idx);
    },
    async exportMarkdown(_t, options, signal, onProgress) {
      calls.push({ kind: 'export', cursor: null, options });
      if (o.gate) await o.gate();
      if (signal?.aborted) throw new DOMException('aborted', 'AbortError');
      if (o.exportFails) throw new MatError('the document ended early', 500);
      const text = o.exportText ?? `# Materialized\n\n${nodesFor(options).map(n => `### ${n.title}\n`).join('\n')}`;
      onProgress?.(text.length);
      return { text, serverName: o.serverName };
    },
  };
  return { api, calls, pageCalls: () => calls.filter(c => c.kind === 'page') };
}
