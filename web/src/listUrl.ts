// The Library list's search and the browse context it was taken in, in the
// page's query string, so a reload restores the same list (and, with
// factset=1, re-creates the same FactSet from it).
//
// Query string only, written with replaceState: the app has no router and a
// relative asset base (gotcha a11a0b03). The keys are prefixed `ctx_` and
// `l_` so they never collide with the single-fact view's `materialize` /
// mat_* keys or the FactSet's `factset` / fs_* keys. The URL never carries a
// FactSet id or cursor (Q-UI6).
//
// The context is named by STABLE ids, never display names: a repo by its
// registry uid and a lens by its registry uid, both of which survive a rename
// (gotcha ae7a7d41). A lens is a virtual repo with the same UX, so it takes the
// same shape; the branch rides only a repo context, because the lens lists
// read the union and take no branch.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { RepoInfo, Status } from './api';
import { factsetView } from './factsetOptions';
import type { Action, AppState, BrowseContext, FilterChip, LibrarySort } from './state';

const K_REPO = 'ctx_repo';
const K_LENS = 'ctx_lens';
const K_BRANCH = 'ctx_branch';
const K_TEXT = 'l_q';
const K_FILTER = 'l_f';
const K_RETURN = 'l_rp';
const K_SORT = 'l_sort';
const K_SOURCES = 'l_src';
const K_SINCE_FORK = 'l_since_fork';
const KEYS = [K_REPO, K_LENS, K_BRANCH, K_TEXT, K_FILTER, K_RETURN, K_SORT, K_SOURCES, K_SINCE_FORK];

const CATEGORIES: FilterChip['category'][] = ['domain', 'entity', 'type', 'kind', 'origin', 'ep', 'path', 'motif'];
const SORTS: LibrarySort[] = ['path', 'recent', 'relevance'];

/** The browse context, by stable id: exactly one of repo or lens. */
export type UrlContext =
  | { kind: 'repo'; uid: string; branch: string }
  | { kind: 'lens'; uid: string };

/** The list state the URL carries: what the list sends, in the app's names. */
export interface UrlList {
  freeText: string;
  filters: FilterChip[];
  sort: LibrarySort;
  lensSources: string[] | null;
  sinceFork: boolean;
}

export const EMPTY_LIST: UrlList = { freeText: '', filters: [], sort: 'path', lensSources: null, sinceFork: false };

export interface ListUrlState { ctx: UrlContext | null; list: UrlList }

export function parseListUrl(search: string): ListUrlState {
  const p = new URLSearchParams(search);
  const repo = p.get(K_REPO) ?? '';
  const lens = p.get(K_LENS) ?? '';
  // Both, or neither, names no one context: nothing to restore.
  const ctx: UrlContext | null = repo && !lens ? { kind: 'repo', uid: repo, branch: p.get(K_BRANCH) ?? '' }
    : lens && !repo ? { kind: 'lens', uid: lens }
    : null;
  const filters: FilterChip[] = [];
  for (const raw of p.getAll(K_FILTER)) {
    const i = raw.indexOf(':');
    if (i <= 0) continue;
    const category = raw.slice(0, i) as FilterChip['category'];
    const value = raw.slice(i + 1);
    if (!CATEGORIES.includes(category) || !value) continue;
    filters.push({ category, value });
  }
  // The pivot's displaced folder rides its one motif chip (state.ts PIVOT_MOTIF).
  const returnPath = p.get(K_RETURN);
  const motif = filters.find(f => f.category === 'motif');
  if (returnPath && motif) motif.returnPath = returnPath;
  const sortRaw = p.get(K_SORT) as LibrarySort | null;
  return {
    ctx,
    list: {
      freeText: p.get(K_TEXT) ?? '',
      filters,
      sort: sortRaw && SORTS.includes(sortRaw) ? sortRaw : 'path',
      // Absent: every mount (null). Present but empty (`l_src=`): none.
      lensSources: p.has(K_SOURCES) ? p.getAll(K_SOURCES).filter(Boolean) : null,
      sinceFork: p.get(K_SINCE_FORK) === '1',
    },
  };
}

/** The query string (with its '?', or '') carrying s, keeping every key that
 *  is not ours. A null ctx writes none of our keys: a list without the context
 *  it was taken in is never restored. Only values off the defaults are written. */
export function writeListUrl(search: string, s: ListUrlState): string {
  const p = new URLSearchParams(search);
  for (const k of KEYS) p.delete(k);
  if (s.ctx) {
    if (s.ctx.kind === 'repo') {
      p.set(K_REPO, s.ctx.uid);
      if (s.ctx.branch) p.set(K_BRANCH, s.ctx.branch);
    } else {
      p.set(K_LENS, s.ctx.uid);
    }
    const l = s.list;
    if (l.freeText) p.set(K_TEXT, l.freeText);
    for (const f of l.filters) p.append(K_FILTER, `${f.category}:${f.value}`);
    const rp = l.filters.find(f => f.category === 'motif')?.returnPath;
    if (rp) p.set(K_RETURN, rp);
    if (l.sort !== 'path') p.set(K_SORT, l.sort);
    if (s.ctx.kind === 'lens' && l.lensSources !== null) {
      if (l.lensSources.length === 0) p.set(K_SOURCES, '');
      else for (const m of l.lensSources) p.append(K_SOURCES, m);
    }
    if (s.ctx.kind === 'repo' && l.sinceFork) p.set(K_SINCE_FORK, '1');
  }
  const q = p.toString();
  return q ? `?${q}` : '';
}

/** Replaces the page URL's query string with q (no history entry), when it
 *  differs. */
export function replaceQuery(q: string): void {
  if (typeof window === 'undefined' || q === window.location.search) return;
  window.history.replaceState(window.history.state, '', window.location.pathname + q + window.location.hash);
}

/** The UrlContext of the context on screen, or null while it cannot be named
 *  by a stable id (repo list not loaded, branch unknown, lens unresolved or
 *  from a server that sends no lens uid). */
export function contextOf(
  context: BrowseContext, branch: string, repoUid: (name: string) => string | undefined, lensUid: string | undefined,
): UrlContext | null {
  if (context.kind === 'repo') {
    const uid = context.repo ? repoUid(context.repo) : undefined;
    return uid && branch ? { kind: 'repo', uid, branch } : null;
  }
  return lensUid ? { kind: 'lens', uid: lensUid } : null;
}

/** Where a reload's restore stands. 'pending': the boot has not picked a
 *  context yet. 'resolving': the URL's context was found and entered; waiting
 *  for its branch (and, for a lens, its doc). 'done': restored or abandoned;
 *  the URL is now written from the state. */
type Phase = 'pending' | 'resolving' | 'done';

export interface ListUrlBoot {
  /** The context the URL named at page load, or null. */
  ctx: UrlContext | null;
  /** The boot found and entered the URL's context, named `name` here. */
  adopt: (name: string) => void;
  /** The URL's context is gone: today's boot stands, and the list and FactSet
   *  keys that depended on it are dropped. */
  abandon: () => void;
}

/**
 * useListUrl owns the list's URL state for App. On mount it reads the URL as
 * the page load found it (the FactSet store too). The boot asks `ctx` first
 * (the URL beats localStorage) and reports `adopt` or `abandon`. Once the
 * adopted context has its branch (the URL's, entered the way enterBranch does)
 * and, for a lens, its resolved doc, the list is restored in one RESTORE_LIST
 * and the FactSet flag is armed for the Library to re-create. From then on
 * every change of context, branch or list is written back with replaceState.
 *
 * A context or branch that no longer exists falls back to today's behaviour
 * (localStorage, the read branch) and drops the dependent keys: a list taken in
 * one context is never applied to another.
 */
export function useListUrl(state: AppState, dispatch: Dispatch<Action>, repos: RepoInfo[], onStatus: (s: Status) => void): ListUrlBoot {
  const [boot] = useState<ListUrlState>(() => {
    factsetView.load();
    return parseListUrl(typeof window !== 'undefined' ? window.location.search : '');
  });
  const [phase, setPhase] = useState<Phase>(boot.ctx ? 'pending' : 'done');
  const adopted = useRef('');
  const branchAsked = useRef(false);
  // The phase and context as of the last commit, for async callbacks.
  const live = useRef({ phase, context: state.context });
  useLayoutEffect(() => { live.current = { phase, context: state.context }; }, [phase, state.context]);

  const abandon = useCallback(() => {
    factsetView.dropPending();
    setPhase('done');
  }, []);
  const adopt = useCallback((name: string) => {
    adopted.current = name;
    setPhase('resolving');
  }, []);
  const restored = useCallback(() => setPhase('done'), []);

  // No context in the URL: nothing to re-create a FactSet from.
  useEffect(() => { if (!boot.ctx) factsetView.dropPending(); }, [boot.ctx]);

  useEffect(() => {
    if (phase !== 'resolving' || !boot.ctx) return;
    const ctx = boot.ctx;
    const name = adopted.current;
    // No context set yet (the initial { repo: '' }) is not "a different
    // context": it is the boot still on its way there. Wait, never abandon.
    if (state.context.kind === 'repo' && !state.context.repo) return;
    if (ctx.kind === 'repo') {
      if (state.context.kind !== 'repo' || state.context.repo !== name) { abandon(); return; }
      if (!state.branch) return; // the status bootstrap is still running
      if (ctx.branch && state.branch !== ctx.branch) {
        if (branchAsked.current) return;
        branchAsked.current = true;
        // Lands only if this restore is still the one in progress: a user who
        // picked another repo meanwhile has abandoned it, and SET_STATUS has
        // no repo guard, so a late answer would put this branch on that repo.
        const stillMine = () => {
          const c = live.current;
          return c.phase === 'resolving' && c.context.kind === 'repo' && c.context.repo === name;
        };
        api.status(name, ctx.branch)
          .then(s => { if (!stillMine()) return; if (s.branch === ctx.branch) onStatus(s); else abandon(); })
          .catch(() => { if (stillMine()) abandon(); });
        return;
      }
    } else {
      if (state.context.kind !== 'lens' || state.context.name !== name) { abandon(); return; }
      if (!state.lens || state.lens.uid !== ctx.uid || !state.branch) return;
    }
    // One batch: the next render has the restored list AND phase 'done'.
    dispatch({ type: 'RESTORE_LIST', ...boot.list });
    // eslint-disable-next-line react-hooks/set-state-in-effect -- a one-time restore step: the one extra render is the point
    restored();
  }, [phase, boot, state.context, state.branch, state.lens, dispatch, abandon, restored, onStatus]);

  useEffect(() => {
    if (phase !== 'done') return;
    // The render that first sees 'done' already holds the restored list, so
    // the Library is told it may re-create the FactSet only now. A no-op
    // unless a flag is still waiting (abandon and a URL with no context drop
    // it first).
    if (boot.ctx) factsetView.arm();
    const ctx = contextOf(state.context, state.branch, n => repos.find(r => r.name === n)?.uid, state.lens?.uid);
    replaceQuery(writeListUrl(window.location.search, {
      ctx,
      list: { freeText: state.freeText, filters: state.filters, sort: state.librarySort, lensSources: state.lensSources, sinceFork: state.sinceFork },
    }));
  }, [phase, boot.ctx, state.context, state.branch, state.lens, repos, state.freeText, state.filters, state.librarySort, state.lensSources, state.sinceFork]);

  return { ctx: boot.ctx, adopt, abandon };
}
