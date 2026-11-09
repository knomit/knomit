// Fact links in a chat answer (design §3): which inline code names a fact,
// and the remark plugin that marks the code the server resolved, so the
// answer's components draw it as a fact link, a repo chip, or "+N".
//
// Candidates are found in INLINE CODE only: prose and fenced blocks are never
// read. A candidate the server does not resolve (POST /facts/resolve omits it)
// stays code, untouched.

import { createContext, useEffect, useRef, useState } from 'react';
import { factLinksMore } from './chatCopy';
import { resolveFactHandles } from './chatApi';
import { boundAfter, callTarget } from './chatState';
import type { ChatTurn } from './chatState';

/** One answer of POST /api/v1/facts/resolve. */
export interface ResolvedFact {
  handle: string;
  id: string;
  title: string;
  type: string;
  repo: string;
  branch: string;
  path: string;
  excerpt: string;
  /** Absent when the server could not parse the fact (and on a commit item). */
  confidence?: number;
  /** What Browse's meta line shows besides (absent from an older server). */
  kind?: string;
  origin?: string;
  sources?: number;
  /** Set (the full hash) when the handle is a COMMIT the server found in the
   *  turn's one repo; the fact fields are then empty. */
  commit?: string;
}

/** The repos the page knows: a kb://<store> chip names one by id or name. */
export interface RepoRef { name: string; id?: string; mode?: string }

const SHORT_ID = /^[0-9a-f]{8}$/;
const COMMIT_HASH = /^[0-9a-f]{7,40}$/;
const KB_URI = /^kb:\/\/[^\s/]+\/\S+$/;
const PATH = /^[\w.-]+(?:\/[\w.-]+)+$/;

/** Whether inline code `code` may name a fact or a commit: a 7-40 hex hash (an
 *  8-hex one may be a fact's short id), a fact path or handle (slashes, ending
 *  in `.md` or in an 8-hex id), or a kb:// URI with a path. Anything else is
 *  code to the reader and is never asked about. */
export function isFactCandidate(code: string): boolean {
  const s = code.trim();
  if (COMMIT_HASH.test(s)) return true;
  if (s.includes('..')) return false;
  if (KB_URI.test(s)) return true;
  if (!PATH.test(s)) return false;
  const last = s.slice(s.lastIndexOf('/') + 1);
  return last.endsWith('.md') || SHORT_ID.test(last);
}

/** The text of an inline code span as markdown reads it: line endings become
 *  spaces, and one space is stripped from each end when both ends have one. */
function codeValue(raw: string): string {
  const v = raw.replace(/\r?\n/g, ' ');
  return v.length > 2 && v.startsWith(' ') && v.endsWith(' ') && v.trim() !== '' ? v.slice(1, -1) : v;
}

/** The fact candidates in an answer's inline code, once each, in order. */
export function factCandidates(markdown: string): string[] {
  // Fenced blocks are code samples, not citations: drop them first.
  const lines: string[] = [];
  let fence = '';
  for (const line of markdown.split('\n')) {
    const m = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (fence) { if (m && m[1][0] === fence[0] && m[1].length >= fence.length) fence = ''; continue; }
    if (m) { fence = m[1]; continue; }
    lines.push(line);
  }
  const out: string[] = [];
  const seen = new Set<string>();
  const span = /(`+)([\s\S]*?[^`])\1(?!`)/g;
  for (const m of lines.join('\n').matchAll(span)) {
    const c = codeValue(m[2]).trim();
    if (!seen.has(c) && isFactCandidate(c)) { seen.add(c); out.push(c); }
  }
  return out;
}

/** The repo a code span names, for a repo chip: kb://<repo id or name> with no
 *  path, or a bare repo name the page knows. Null for anything else. */
export function repoChipName(code: string, repos: RepoRef[]): string | null {
  const s = code.trim();
  const store = /^kb:\/\/([^\s/]+)\/?$/.exec(s)?.[1];
  if (store) {
    // An id at the server's lengths only (Manager.ByRootID): the full id, or its 12-hex prefix.
    const r = repos.find(x => x.name === store || (!!x.id && (x.id === store || (store.length === 12 && x.id.startsWith(store)))));
    return r ? r.name : null;
  }
  return repos.some(r => r.name === s) ? s : null;
}

// ── the remark plugin ──

interface Node {
  type: string;
  value?: string;
  children?: Node[];
  data?: { hName?: string; hProperties?: Record<string, string> };
}

/** Separators a list of links in a table cell is written with. */
const SEPARATOR = /^[\s,;·/&]*(?:and)?[\s,;·/&]*$/i;

function mark(n: Node, facts: ReadonlyMap<string, ResolvedFact>, repos: RepoRef[]): void {
  if (n.type === 'inlineCode') {
    const v = (n.value ?? '').trim();
    const f = facts.get(v);
    if (f?.commit) n.data = { ...n.data, hProperties: { ...n.data?.hProperties, 'data-commit': v } };
    else if (f) n.data = { ...n.data, hProperties: { ...n.data?.hProperties, 'data-fact': v } };
    else {
      const repo = repoChipName(v, repos);
      if (repo) n.data = { ...n.data, hProperties: { ...n.data?.hProperties, 'data-repo': repo } };
    }
    return;
  }
  n.children?.forEach(c => mark(c, facts, repos));
  if (n.type === 'tableCell') collapseCell(n, facts);
}

const isFact = (n: Node, facts: ReadonlyMap<string, ResolvedFact>) => {
  const f = n.type === 'inlineCode' ? facts.get((n.value ?? '').trim()) : undefined;
  return !!f && !f.commit;
};

/** A table cell citing several facts keeps the first link, then "+N": the
 *  other links, and the separators between them, leave the cell. */
function collapseCell(cell: Node, facts: ReadonlyMap<string, ResolvedFact>): void {
  const kids = cell.children ?? [];
  const first = kids.findIndex(k => isFact(k, facts));
  if (first < 0) return;
  const rest = kids.slice(first + 1);
  const more = rest.filter(k => isFact(k, facts)).length;
  if (more === 0) return;
  // After the first link, drop the other links and the separators around them; keep any other text.
  const kept = rest.filter(k => !isFact(k, facts) && !(k.type === 'text' && SEPARATOR.test(k.value ?? '')));
  const plus: Node = { type: 'factMore', data: { hName: 'span', hProperties: { 'data-fact-more': String(more) } }, children: [{ type: 'text', value: factLinksMore(more) }] };
  cell.children = [...kids.slice(0, first + 1), plus, ...kept];
}

// ── resolution and what the links read ──

/** What one turn's links read: the facts its handles resolved to, the fact
 *  open in the side panel (its link stays highlighted), and what a click does. */
export interface FactLinkEnv {
  facts: ReadonlyMap<string, ResolvedFact>;
  /** factKey of the fact (commitKey of the commit) the side panel shows, or null. */
  openKey: string | null;
  /** Click: open the fact in the side panel. */
  onOpen?: (f: ResolvedFact) => void;
  /** Cmd-click: send the reader to Browse on the fact. */
  onBrowse?: (f: ResolvedFact) => void;
  /** A commit link: open the commit in the side panel. The branch is the
   *  resolver's (the repo's read branch), never a lens: a commit resolves only
   *  when the turn worked in one repo, and it answers with that repo. */
  onCommit?: (repo: string, branch: string, commit: string) => void;
}

const NO_FACTS: ReadonlyMap<string, ResolvedFact> = new Map();
export const FactLinkContext = createContext<FactLinkEnv>({ facts: NO_FACTS, openKey: null });

export const factKey = (repo: string, branch: string, path: string): string => `${repo}\0${branch}\0${path}`;
export const commitKey = (repo: string, branch: string, commit: string): string => `commit\0${repo}\0${branch}\0${commit}`;

/** What has been asked, for the page's life: per preference list and handle,
 *  the fact it resolved to, or null (not a fact, or the call failed). A
 *  remounted chat, or a later turn citing the same fact, asks nothing again. */
const cache = new Map<string, ResolvedFact | null>();
/** The (preference, handle) pairs a call in flight is asking. */
const inFlight = new Set<string>();
const cacheKey = (prefer: string, handle: string): string => `${prefer}\u0001${handle}`;

/** For tests: forget every answer. */
export function resetFactCache(): void { cache.clear(); inFlight.clear(); }

/**
 * useTurnFacts resolves the turns' cited handles, the turn still streaming
 * included. Each effect run sends ONE POST /facts/resolve per distinct
 * preference list (one in the usual chat, which stays bound to one target)
 * with every candidate of every turn not yet answered or being asked. What
 * keeps a stream from asking per delta is that skip (cache and inFlight): a
 * streamed delta that completes no new code span asks nothing, and one that
 * does asks for that handle only. The run is keyed on the candidates, not the
 * text, which only saves re-running the effect.
 * Only a CLOSED code span is a candidate (factCandidates; remark agrees), so a
 * path still streaming is never asked about and never drawn as a link.
 * A failed call is not retried: its handles are remembered as not facts and
 * stay code. An answer that arrives after the chat unmounted is kept in the
 * cache and changes no state. A null answer is not asked about.
 *
 * prefers[i] is turn i's preference list (turnPrefer), and the cache is keyed
 * by it. A streaming turn's list can change: on each call it makes, and when a
 * call that was still running settles (a running call counts; if it then fails
 * it is dropped). Nothing here pins the list while an answer streams. It holds
 * steady in practice only because of the ORDER a model streams in: text after
 * a tool call arrives after that call's result, and any text before the call
 * becomes the call's group lead (ChatPanel's blocksOf), which is not part of
 * the answer. So the answer text a chip is drawn in is read under the turn's
 * settled list, and the finished turn renders as it did while streaming. If a
 * provider ever streamed answer text while a call of the same turn was still
 * running, a change of list would re-key its handles: they would be asked
 * again, and show as code until the new answer came back.
 */
export function useTurnFacts(answers: (string | null)[], prefers: string[][]): ReadonlyMap<string, ResolvedFact>[] {
  const [, setVersion] = useState(0);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const turns = answers.map((a, i) => (a === null ? null : { prefer: prefers[i] ?? [], handles: factCandidates(a) }));
  const sig = JSON.stringify(turns);
  useEffect(() => {
    const ask = new Map<string, { prefer: string[]; handles: string[] }>();
    for (const t of JSON.parse(sig) as ({ prefer: string[]; handles: string[] } | null)[]) {
      if (!t) continue;
      const p = t.prefer.join('\n');
      for (const h of t.handles) {
        const k = cacheKey(p, h);
        if (cache.has(k) || inFlight.has(k)) continue;
        inFlight.add(k);
        const group = ask.get(p) ?? { prefer: t.prefer, handles: [] };
        if (!group.handles.includes(h)) group.handles.push(h);
        ask.set(p, group);
      }
    }
    for (const [p, { prefer, handles }] of ask) {
      const settle = (list: ResolvedFact[]) => {
        const by = new Map(list.map(f => [f.handle, f]));
        for (const h of handles) {
          inFlight.delete(cacheKey(p, h));
          cache.set(cacheKey(p, h), by.get(h) ?? null);
        }
        if (mounted.current) setVersion(v => v + 1);
      };
      resolveFactHandles(handles, prefer).then(settle, () => settle([]));
    }
  }, [sig]);
  return turns.map(t => {
    if (!t) return NO_FACTS;
    const p = t.prefer.join('\n');
    const m = new Map<string, ResolvedFact>();
    for (const h of t.handles) {
      const f = cache.get(cacheKey(p, h));
      if (f) m.set(h, f);
    }
    return m.size ? m : NO_FACTS;
  });
}

/** A string argument of a tool call's arguments, or ''. */
const argStr = (args: unknown, name: string): string => {
  const v = (args as Record<string, unknown> | null | undefined)?.[name];
  return typeof v === 'string' ? v : '';
};

/**
 * turnPrefer is the repos turn ti worked in, for POST /facts/resolve's prefer:
 * what each of its SUCCESSFUL calls acted on (a bind's target; a call the server
 * marks in_base, the server's target, else the binding in force; a call without
 * in_base ran nowhere in particular and adds nothing) and the store of a kb://
 * file it named, latest call first; then the binding in force when the turn
 * began.
 *
 * A call that failed worked in nothing, so it adds nothing (user decision): a
 * failed call naming a second reachable repo must not stop a commit link from
 * resolving, since the server resolves a hash only with exactly one repo preferred.
 * The accepted cost is that such a repo earns no preference either, so a short id
 * that two repos hold, where only a failed call named one of them, is ambiguous and
 * stays code. This rule is the prefer list's alone: callTarget and the activity line
 * still see failed calls.
 * Tool RESULTS are not in the transcript (they are fetched lazily), so a
 * kb:// in a result does not count.
 */
export function turnPrefer(turns: ChatTurn[], ti: number): string[] {
  let bound = '';
  for (let i = 0; i < ti; i++) for (const p of turns[i].parts) if (p.kind === 'group') bound = boundAfter(p.calls, bound);
  const before = bound;
  const own: string[] = [];
  for (const p of turns[ti]?.parts ?? []) {
    if (p.kind !== 'group') continue;
    for (const c of p.calls) {
      if (!c.isError) {
        own.push(callTarget(c, bound));
        const store = /^kb:\/\/([^/\s]+)\//.exec(argStr(c.args, 'file') || argStr(c.args, 'path'))?.[1];
        if (store) own.push(store);
      }
      bound = boundAfter([c], bound); // a failed bind moves nothing
    }
  }
  const out: string[] = [];
  for (const t of [...own.reverse(), before]) if (t && !out.includes(t)) out.push(t);
  return out;
}

/** remarkFactLinks marks the resolved fact handles (data-fact) and the repo
 *  names (data-repo) in inline code, and collapses table cells to "+N". */
export function remarkFactLinks(facts: ReadonlyMap<string, ResolvedFact>, repos: RepoRef[]) {
  return () => (root: Node) => mark(root, facts, repos);
}
