// Keeping the reader's place in a results list when the head moves (user
// ruling 2026-11-03, option b). An agent's write moves the branch head; the
// list must reload QUIETLY: no reset to page 1, no jump to row 0, the open
// fact, the selection and the scroll position kept, new or changed rows shown
// where they now rank.
//
// Paging is stateless (offset/limit re-run the search), so the refresh re-asks
// for exactly the rows the reader has loaded, [0, n), in as few calls as the
// API's page ceiling allows, and the scroll is re-anchored on a row by PATH.

/** The API's page ceiling: limit is 1..500 on every list route (params.go
 *  maxLimit). The refresh asks for the loaded rows in pages of this size. */
export const API_MAX_LIMIT = 500;

/** One page of a list: its rows and the server's total for the query. */
export interface ListPageResult<T> { rows: T[]; total: number }

/**
 * refetchRange re-reads rows [from, to) of a list in ceil((to-from) /
 * API_MAX_LIMIT) calls, checking `stale` between calls: a stale refresh
 * (the list's scope changed) stops and returns null. It stops at the first
 * short page, where the list now ends, and reports that as `short`.
 */
export async function refetchRange<T>(
  from: number, to: number,
  fetchPage: (offset: number, limit: number) => Promise<ListPageResult<T>>,
  stale: () => boolean = () => false,
): Promise<{ rows: T[]; total: number; short: boolean } | null> {
  const rows: T[] = [];
  let total = 0;
  while (from + rows.length < to) {
    if (stale()) return null;
    const limit = Math.min(API_MAX_LIMIT, to - from - rows.length);
    const page = await fetchPage(from + rows.length, limit);
    rows.push(...page.rows);
    total = page.total;
    if (page.rows.length < limit) return stale() ? null : { rows, total, short: true };
  }
  return stale() ? null : { rows, total, short: false };
}

/** refetchLoaded is refetchRange over the first n rows, the rows the reader
 *  has loaded: the work is bounded by what the reader paid for, 50 at a time. */
export function refetchLoaded<T>(
  n: number,
  fetchPage: (offset: number, limit: number) => Promise<ListPageResult<T>>,
  stale?: () => boolean,
) {
  return refetchRange(0, n, fetchPage, stale);
}

/**
 * The refresh state of one list. `gen` is the list's SCOPE generation: a full
 * load bumps it, which makes any refresh in flight stale. `loaded` is set once
 * the current scope's page 1 has landed: only then is there a place to keep.
 * At most one refresh is in flight (`inFlight`); a new head while one is in
 * flight only marks it `dirty`, and the refresh re-runs once when it
 * completes (trailing edge), so every refresh lands and the last head wins.
 * `blockedLoadMore` records a loadMore refused during a refresh, to retry.
 */
export interface RefreshState { gen: number; loaded: boolean; inFlight: boolean; dirty: boolean; blockedLoadMore: boolean }

export function newRefreshState(): RefreshState {
  return { gen: 0, loaded: false, inFlight: false, dirty: false, blockedLoadMore: false };
}

/** A full load of the list: a new scope generation, nothing loaded yet, any
 *  refresh in flight abandoned. */
export function beginFullLoad(st: RefreshState): number {
  st.gen += 1;
  st.loaded = false;
  st.inFlight = false;
  st.dirty = false;
  return st.gen;
}

/**
 * runRefresh re-reads the loaded rows of a list whose head moved, coalescing
 * heads that arrive while it runs. `paths` is the list's current paths;
 * `anchor` the path of the row the reader's place hangs on (the selected row
 * if on screen, else the top visible row), read at completion time. Each pass
 * reads [0, n) (n = the rows the previous pass landed) and, when the window
 * filled, reads on by exactly as many rows as were inserted above the anchor,
 * so the rows below it are still loaded and the scroller can put it back
 * where it sat. The extension is bounded by those inserts. `apply` receives each pass's rows;
 * `onError` runs on a failed pass (the list keeps its rows).
 */
export function runRefresh<T>(
  st: RefreshState,
  paths: string[],
  pathOf: (row: T) => string,
  anchor: () => string | undefined,
  fetchPage: (offset: number, limit: number) => Promise<ListPageResult<T>>,
  apply: (r: { rows: T[]; total: number; short: boolean }) => void,
  onError: () => void,
): void {
  if (st.inFlight) { st.dirty = true; return; }
  st.inFlight = true;
  const gen = st.gen;
  const stale = () => gen !== st.gen;
  const pass = async (old: string[]) => {
    const r = await refetchLoaded(old.length, fetchPage, stale);
    if (!r) return null;
    const at = anchor();
    const got = r.rows.map(pathOf);
    if (at && !r.short && old.includes(at)) {
      // KEEP WHAT LIES BELOW THE ANCHOR. The window re-reads n rows, so every
      // row inserted ABOVE the anchor pushes one row out at the bottom. Read on
      // by exactly that many, or (a) a row near the window's end cannot sit
      // where it sat: the scroller cannot scroll that far and clamps (the
      // ~125px drift seen after a burst), and (b) the anchor itself can fall
      // out of the window. Count the new paths up to the anchor, or, when it
      // is gone, up to the nearest old row above it that is still listed.
      // None (it was deleted or no longer matches, and nothing was inserted
      // above it) reads nothing more.
      const known = new Set(old);
      let k = got.indexOf(at) - 1;
      if (k < -1) {
        k = -1;
        for (let i = old.indexOf(at) - 1; i >= 0 && k < 0; i--) k = got.indexOf(old[i]);
      }
      const inserted = got.slice(0, k + 1).filter(p => !known.has(p)).length;
      if (inserted > 0) {
        const more = await refetchRange(old.length, old.length + inserted, fetchPage, stale);
        if (!more) return null;
        return { rows: [...r.rows, ...more.rows], total: more.total, short: more.short };
      }
    }
    return r;
  };
  const loop = (old: string[]): void => {
    pass(old).then(r => {
      if (stale()) return;
      if (r) apply(r);
      if (st.dirty) {
        st.dirty = false;
        loop(r ? r.rows.map(pathOf) : old);
        return;
      }
      st.inFlight = false;
    }).catch(() => {
      if (stale()) return;
      st.inFlight = false;
      st.dirty = false;
      onError();
    });
  };
  loop(paths);
}

/**
 * A scroll anchor: the row the reader is looking at, by path, and where it sat
 * in the scroller (pixels from the scroller's top edge). `fallback` lists the
 * rows that followed it, then the ones before it (nearest first), so a row that
 * vanished (deleted, or no longer matching) anchors on its nearest survivor.
 */
export interface ScrollAnchor { path: string; offset: number; fallback: string[] }

/** isRendered: the element has a layout box (not display:none, e.g. the
 *  Library while the FactSet view holds Focus). Rects of a hidden list are
 *  all zero, so an anchor read from it would point at row 0. */
export function isRendered(el: Element): boolean {
  return el.getClientRects().length > 0;
}

function rowsOf(container: HTMLElement): HTMLElement[] {
  return Array.from(container.querySelectorAll<HTMLElement>('[data-path]')).filter(el => el.dataset.path);
}

/**
 * captureAnchor picks the selected row when any of it is on screen, else the
 * first row on screen, and records its offset. Returns null for an empty or
 * hidden list.
 */
export function captureAnchor(container: HTMLElement, selectedPath: string | undefined): ScrollAnchor | null {
  if (!isRendered(container)) return null;
  const rows = rowsOf(container);
  if (rows.length === 0) return null;
  const top = container.getBoundingClientRect().top;
  const height = container.clientHeight;
  const visible = (el: HTMLElement) => {
    const r = el.getBoundingClientRect();
    return r.bottom - top > 0 && r.top - top < height;
  };
  let i = selectedPath ? rows.findIndex(el => el.dataset.path === selectedPath && visible(el)) : -1;
  if (i < 0) i = rows.findIndex(visible);
  if (i < 0) i = 0;
  const after = rows.slice(i + 1).map(el => el.dataset.path!);
  const before = rows.slice(0, i).reverse().map(el => el.dataset.path!);
  return {
    path: rows[i].dataset.path!,
    offset: rows[i].getBoundingClientRect().top - top,
    fallback: [...after, ...before],
  };
}

/**
 * restoreAnchor scrolls so the anchor row (or its nearest surviving
 * neighbour) sits where it sat. Returns the scrollTop it set, or null when no
 * candidate row is in the list or the list is hidden (nothing is touched).
 */
export function restoreAnchor(container: HTMLElement, anchor: ScrollAnchor): number | null {
  if (!isRendered(container)) return null;
  const byPath = new Map(rowsOf(container).map(el => [el.dataset.path!, el]));
  for (const p of [anchor.path, ...anchor.fallback]) {
    const el = byPath.get(p);
    if (!el) continue;
    const delta = (el.getBoundingClientRect().top - container.getBoundingClientRect().top) - anchor.offset;
    container.scrollTop += delta;
    return container.scrollTop;
  }
  return null;
}

/** sentinelInView: the infinite-scroll sentinel is inside the scroller's
 *  viewport, so a loadMore refused earlier is still wanted (the observer only
 *  reports changes and will not fire again on its own). */
export function sentinelInView(container: HTMLElement, sentinel: HTMLElement | null): boolean {
  if (!sentinel || !isRendered(container)) return false;
  const c = container.getBoundingClientRect();
  const s = sentinel.getBoundingClientRect();
  return s.top < c.bottom && s.bottom >= c.top;
}
