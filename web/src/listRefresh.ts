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
 * refetchLoaded re-reads the first n rows of a list, the rows the reader has
 * already scrolled through, in ceil(n / API_MAX_LIMIT) calls (n is at most
 * 500 on a relevance list, so one call). It stops at the first short page:
 * the list now ends there. `short` reports that, so the caller can stop
 * paging. The work is bounded by what the reader loaded, never more: the
 * reader paid for those rows one page of 50 at a time.
 */
export async function refetchLoaded<T>(
  n: number,
  fetchPage: (offset: number, limit: number) => Promise<ListPageResult<T>>,
): Promise<{ rows: T[]; total: number; short: boolean }> {
  const rows: T[] = [];
  let total = 0;
  while (rows.length < n) {
    const limit = Math.min(API_MAX_LIMIT, n - rows.length);
    const page = await fetchPage(rows.length, limit);
    rows.push(...page.rows);
    total = page.total;
    if (page.rows.length < limit) return { rows, total, short: true };
  }
  return { rows, total, short: false };
}

/**
 * A scroll anchor: the row the reader is looking at, by path, and where it sat
 * in the scroller (pixels from the scroller's top edge). `fallback` lists the
 * rows that followed it, then the ones before it (nearest first), so a row that
 * vanished (deleted, or no longer matching) anchors on its nearest survivor.
 */
export interface ScrollAnchor { path: string; offset: number; fallback: string[] }

function rowsOf(container: HTMLElement): HTMLElement[] {
  return Array.from(container.querySelectorAll<HTMLElement>('[data-path]')).filter(el => el.dataset.path);
}

/**
 * captureAnchor picks the selected row when any of it is on screen, else the
 * first row on screen, and records its offset. Returns null for an empty
 * list.
 */
export function captureAnchor(container: HTMLElement, selectedPath: string | undefined): ScrollAnchor | null {
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
 * candidate row is in the list.
 */
export function restoreAnchor(container: HTMLElement, anchor: ScrollAnchor): number | null {
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
