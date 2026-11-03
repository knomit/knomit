import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';
import { latestStream, streamsMatching } from './testEventSource';

// A new head while the reader is deep in a list KEEPS THE READER'S PLACE (user
// ruling 2026-11-03, option b): the loaded rows are re-read quietly, the
// selected row stays selected by path, the scroll stays anchored on it, and
// nothing navigates. Recent and relevance, repo and lens.
vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({ path: 'kb', children: [] }),
    recent: vi.fn(),
    search: vi.fn(),
    listLensFacts: vi.fn(),
    lensSearch: vi.fn(),
  },
}));

const lens: Lens = {
  name: 'eng',
  write: { uid: 'uid-core', name: 'core' },
  reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }],
};
const repoBase: AppState = { ...init, repo: 'core', branch: 'agent/main', headCommit: 'h1', librarySort: 'recent' };
const lensBase: AppState = { ...repoBase, context: { kind: 'lens', name: 'eng' }, lens, lensSources: null };

const ROW = 20; // px per row in the fake layout below
const NEW = 'kb/r/new.md';
const pathOf = (i: number) => `kb/r/f${String(i).padStart(3, '0')}.md`;

// The server's list: 160 rows; after a head an agent's write may have added a
// row at the top, or deleted one.
let list: string[] = [];
function row(p: string) {
  return { path: p, title: p, type: 'observation', committed_at: 1, source: { repo: 'core', id: 'aaaaaaaaaaaa', branch: 'agent/main' } };
}
function page(offset = 0, limit = 50) {
  const rows = list.slice(offset, offset + limit).map(row);
  return { rows, total: list.length };
}

let observers: IntersectionObserverCallback[] = [];
const origIO = window.IntersectionObserver;
const origRect = Element.prototype.getBoundingClientRect;
const origClientHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientHeight');
const origClientRects = Element.prototype.getClientRects;
const origRO = globalThis.ResizeObserver;
// hidden: the Library is display:none (the FactSet view holds Focus): no
// layout box, so getClientRects() is empty.
let hidden = false;
let resizeCallbacks: ResizeObserverCallback[] = [];

beforeEach(async () => {
  vi.clearAllMocks();
  list = Array.from({ length: 160 }, (_, i) => pathOf(i));
  observers = [];
  hidden = false;
  resizeCallbacks = [];
  Element.prototype.getClientRects = function () {
    return (hidden ? [] : [{}]) as unknown as DOMRectList;
  };
  globalThis.ResizeObserver = class {
    constructor(cb: ResizeObserverCallback) { resizeCallbacks.push(cb); }
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver;
  window.IntersectionObserver = class {
    constructor(cb: IntersectionObserverCallback) { observers.push(cb); }
    observe() {}
    disconnect() {}
    unobserve() {}
    takeRecords() { return []; }
    root = null; rootMargin = ''; thresholds = [];
  } as unknown as typeof IntersectionObserver;
  // A fake layout: the scroller's top edge is y=0 and it is 400px tall; row i
  // of the list sits at i*ROW - scrollTop.
  Object.defineProperty(HTMLElement.prototype, 'clientHeight', { configurable: true, get() { return 400; } });
  Element.prototype.getBoundingClientRect = function (this: Element) {
    const el = this as HTMLElement;
    const path = el.dataset?.path;
    if (path && el.parentElement) {
      const scroller = el.parentElement;
      const rows = Array.from(scroller.querySelectorAll<HTMLElement>('[data-path]'));
      const top = rows.indexOf(el) * ROW - scroller.scrollTop;
      return { top, bottom: top + ROW, left: 0, right: 100, width: 100, height: ROW, x: 0, y: top, toJSON() {} } as DOMRect;
    }
    return { top: 0, bottom: 400, left: 0, right: 100, width: 100, height: 400, x: 0, y: 0, toJSON() {} } as DOMRect;
  };
  const { api } = await import('./api');
  (api.recent as ReturnType<typeof vi.fn>).mockImplementation(async (_r: string, _b: string, _p: string, _q: string, limit?: number, offset = 0) => {
    const p = page(offset, limit); return { facts: p.rows, total: p.total };
  });
  (api.search as ReturnType<typeof vi.fn>).mockImplementation(async (_r: string, _b: string, _q: string, _p: string, _c: number, _o: unknown, pg?: { limit?: number; offset?: number }) => {
    const p = page(pg?.offset, pg?.limit); return { results: p.rows, total: p.total };
  });
  (api.listLensFacts as ReturnType<typeof vi.fn>).mockImplementation(async (_l: string, o: { limit?: number; offset?: number }) => {
    const p = page(o.offset, o.limit); return { facts: p.rows, total: p.total };
  });
  (api.lensSearch as ReturnType<typeof vi.fn>).mockImplementation(async (_l: string, _q: string, _r: unknown, _o: unknown, pg?: { limit?: number; offset?: number }) => {
    const p = page(pg?.offset, pg?.limit); return { results: p.rows, total: p.total };
  });
});
afterEach(() => {
  window.IntersectionObserver = origIO;
  Element.prototype.getBoundingClientRect = origRect;
  Element.prototype.getClientRects = origClientRects;
  globalThis.ResizeObserver = origRO;
  if (origClientHeight) Object.defineProperty(HTMLElement.prototype, 'clientHeight', origClientHeight);
  else delete (HTMLElement.prototype as { clientHeight?: number }).clientHeight;
});

function intersect() {
  act(() => { observers[observers.length - 1]([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver); });
}

const CASES: { name: string; state: AppState; testId: string }[] = [
  { name: 'repo Recent', state: repoBase, testId: 'chrono-item' },
  { name: 'repo relevance', state: { ...repoBase, freeText: 'agentic' }, testId: 'dir-entry' },
  { name: 'lens Recent', state: lensBase, testId: 'lens-item' },
  { name: 'lens relevance', state: { ...lensBase, freeText: 'agentic' }, testId: 'lens-item' },
];

/** Loads 150 rows, selects row 120 and scrolls it to 100px below the top. */
async function deepInList(state: AppState, testId: string) {
  const dispatch = vi.fn();
  const navigate = vi.fn();
  const view = render(<Library state={state} dispatch={dispatch} navigate={navigate} />);
  await waitFor(() => expect(screen.getAllByTestId(testId).length).toBe(50));
  intersect();
  await waitFor(() => expect(screen.getAllByTestId(testId).length).toBe(100));
  intersect();
  await waitFor(() => expect(screen.getAllByTestId(testId).length).toBe(150));
  fireEvent.click(screen.getAllByTestId(testId)[120]);
  // App's side of the click: the fact opens (state.factPath).
  expect(navigate).toHaveBeenCalledWith({ view: 'library', factPath: pathOf(120) });
  const open = { ...state, factPath: pathOf(120) };
  view.rerender(<Library state={open} dispatch={dispatch} navigate={navigate} />);
  await waitFor(() => expect(selectedPath()).toBe(pathOf(120)));
  const scroller = screen.getByTestId('recent-sentinel').parentElement!;
  scroller.scrollTop = 120 * ROW - 100;
  dispatch.mockClear();
  navigate.mockClear();
  return { view, dispatch, navigate, scroller, open };
}
const selectedPath = () => document.querySelector<HTMLElement>('[data-selected="true"]')?.dataset.path;
const paths = (testId: string) => screen.getAllByTestId(testId).map(e => e.dataset.path);

// Sabotage: treat a new head like a new scope (sameScopeNewHead always false),
// and the list resets to page 1 with row 0 opened; skip restoreAnchor, and the
// scroll stays at the old scrollTop while the row moved down; keep the old
// selected index, and the selection lands on the row above.
describe('Library — a new head keeps the reader\'s place', () => {
  for (const c of CASES) {
    it(`${c.name}: row 120 stays selected and anchored when a write adds a row above it`, async () => {
      const { view, dispatch, navigate, scroller, open } = await deepInList(c.state, c.testId);
      list = [NEW, ...list];
      view.rerender(<Library state={{ ...open, headCommit: 'h2' }} dispatch={dispatch} navigate={navigate} />);
      await waitFor(() => expect(paths(c.testId)[0]).toBe(NEW));
      // The window read on by the one row inserted above the anchor, so what
      // lay below it is still loaded.
      await waitFor(() => expect(paths(c.testId)).toHaveLength(151));
      expect(paths(c.testId).at(-1)).toBe(pathOf(149));
      expect(selectedPath()).toBe(pathOf(120));
      expect(paths(c.testId).indexOf(pathOf(120))).toBe(121);
      // The row moved down one row; the scroll moved with it, so it sits where
      // it sat (100px below the top).
      expect(scroller.scrollTop).toBe(121 * ROW - 100);
      expect(dispatch.mock.calls.filter(a => a[0].type === 'AMEND_NAV')).toEqual([]);
      expect(navigate).not.toHaveBeenCalled();
    });
  }

  it('the refresh re-reads exactly the loaded rows, in one call under the API ceiling', async () => {
    const { api } = await import('./api');
    const { view, dispatch, navigate, open } = await deepInList(CASES[1].state, CASES[1].testId);
    const search = api.search as ReturnType<typeof vi.fn>;
    search.mockClear();
    view.rerender(<Library state={{ ...open, headCommit: 'h2' }} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(search).toHaveBeenCalled());
    expect(search.mock.calls.map(cl => cl[6])).toEqual([{ limit: 150, offset: 0 }]);
  });

  // Fallback: the open fact was deleted (or no longer matches). Nothing
  // navigates (the right panel keeps it open and says what it says), no row is
  // selected, and the scroll anchors on its nearest survivor: the row that
  // followed it takes its place on screen.
  for (const c of [CASES[0], CASES[3]]) {
    it(`${c.name}: a deleted selected row leaves no selection and its successor in its place`, async () => {
      const { view, dispatch, navigate, scroller, open } = await deepInList(c.state, c.testId);
      list = list.filter(p => p !== pathOf(120));
      view.rerender(<Library state={{ ...open, headCommit: 'h3' }} dispatch={dispatch} navigate={navigate} />);
      await waitFor(() => expect(paths(c.testId)).not.toContain(pathOf(120)));
      expect(paths(c.testId)).toHaveLength(150);
      expect(selectedPath()).toBeUndefined();
      expect(paths(c.testId).indexOf(pathOf(121))).toBe(120);
      expect(scroller.scrollTop).toBe(120 * ROW - 100);
      expect(dispatch.mock.calls.filter(a => a[0].type === 'AMEND_NAV')).toEqual([]);
      expect(navigate).not.toHaveBeenCalled();
    });
  }

  it('a scope change is still a new list from page 1', async () => {
    const { view, dispatch, navigate, open } = await deepInList(CASES[0].state, CASES[0].testId);
    view.rerender(<Library state={{ ...open, headCommit: 'h2', freeText: 'other' }} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(50));
  });
});

/** A promise the test resolves by hand. */
function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>(r => { resolve = r; });
  return { promise, resolve };
}

describe('Library — keeping the place under a busy agent (#739 review)', () => {
  // H1: a head while the CURRENT scope's page 1 is in flight is a fresh load,
  // not a refresh of whatever rows are still held (here, the folder the reader
  // searched from).
  // Sabotage: drop the rs.loaded check (keep place whenever rows are held) and
  // the head refreshes the folder's 7 rows ({limit: 7}), page 1 never lands
  // and the search spinner stays on.
  it('a new head during page 1 reloads page 1: it lands, the spinner stops, row 0 opens', async () => {
    const { api } = await import('./api');
    (api.browse as ReturnType<typeof vi.fn>).mockResolvedValue({
      path: 'kb', children: Array.from({ length: 7 }, (_, i) => ({ name: `d${i}.md`, is_dir: false, fullPath: `kb/d${i}.md`, title: `D${i}` })),
    });
    const search = api.search as ReturnType<typeof vi.fn>;
    const firstPage = deferred<{ results: unknown[]; total: number }>();
    const real = search.getMockImplementation()! as (...a: unknown[]) => unknown;
    search.mockImplementationOnce(() => firstPage.promise);
    const dispatch = vi.fn();
    const pathState: AppState = { ...repoBase, librarySort: 'path' };
    const view = render(<Library state={pathState} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(7));
    const searching = { ...pathState, freeText: 'agentic' };
    view.rerender(<Library state={searching} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
    search.mockImplementation(real);
    view.rerender(<Library state={{ ...searching, headCommit: 'h2' }} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(search).toHaveBeenCalledTimes(2));
    firstPage.resolve({ results: [], total: 0 }); // the superseded page 1: dropped
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(50));
    expect(search.mock.calls.map(c => c[6])).toEqual([{ limit: 50, offset: 0 }, { limit: 50, offset: 0 }]);
    const searchingFlags = dispatch.mock.calls.map(c => c[0]).filter(a => a.type === 'SET_SEARCHING');
    expect(searchingFlags.at(-1)).toEqual({ type: 'SET_SEARCHING', value: false });
    expect(dispatch.mock.calls.map(c => c[0]).filter(a => a.type === 'AMEND_NAV').at(-1)).toEqual({ type: 'AMEND_NAV', factPath: pathOf(0) });
  });

  // M1: heads arriving faster than a refresh completes coalesce: at most one
  // refresh is in flight, the in-flight one still lands, and one more pass
  // runs after it, reading the state after the LAST head.
  // Sabotage: restart on every head (no dirty flag: a new pass per head) and
  // three passes overlap; drop the trailing re-run and the last head's rows
  // never show.
  it('heads faster than a refresh: one in flight, every pass lands, the last head wins', async () => {
    const { api } = await import('./api');
    const c = CASES[1];
    const { view, dispatch, navigate, open } = await deepInList(c.state, c.testId);
    const search = api.search as ReturnType<typeof vi.fn>;
    const real = search.getMockImplementation()! as (...a: unknown[]) => unknown;
    let inFlight = 0, maxInFlight = 0;
    const gates: Array<() => void> = [];
    search.mockClear();
    search.mockImplementation((...args: unknown[]) => {
      inFlight++; maxInFlight = Math.max(maxInFlight, inFlight);
      return new Promise(resolve => gates.push(() => { inFlight--; resolve(real(...args)); }));
    });
    list = ['kb/r/a.md', ...list];
    view.rerender(<Library state={{ ...open, headCommit: 'h2' }} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
    list = ['kb/r/b.md', ...list];
    view.rerender(<Library state={{ ...open, headCommit: 'h3' }} dispatch={dispatch} navigate={navigate} />);
    list = ['kb/r/c.md', ...list];
    view.rerender(<Library state={{ ...open, headCommit: 'h4' }} dispatch={dispatch} navigate={navigate} />);
    await act(async () => { await Promise.resolve(); });
    expect(search).toHaveBeenCalledTimes(1);
    // Release the gated calls one at a time until nothing is pending: pass 1
    // reads [0,150) then reads on by the 3 rows inserted above the anchor;
    // the one trailing pass (for h3 and h4 together) reads [0,153).
    for (let i = 0; i < 10 && (gates.length > 0 || i === 0); i++) {
      await waitFor(() => expect(gates.length).toBeGreaterThan(0));
      await act(async () => { gates.shift()!(); });
      await act(async () => { await Promise.resolve(); });
      if (search.mock.calls.length >= 3 && gates.length === 0) break;
    }
    await waitFor(() => expect(paths(c.testId).slice(0, 3)).toEqual(['kb/r/c.md', 'kb/r/b.md', 'kb/r/a.md']));
    expect(maxInFlight).toBe(1);
    expect(search.mock.calls.map(cl => cl[6])).toEqual([{ limit: 150, offset: 0 }, { limit: 3, offset: 150 }, { limit: 153, offset: 0 }]);
    expect(selectedPath()).toBe(pathOf(120));
    expect(paths(c.testId).indexOf(pathOf(120))).toBe(123);
  });

  // M2: a page the sentinel asked for while a refresh ran is asked for again
  // once the refresh lands (the sentinel is still in view, and the observer
  // only reports changes).
  // Sabotage: drop the retry in the layout effect and offset 150 is never read.
  it('a loadMore refused during a refresh is retried when it lands', async () => {
    const { api } = await import('./api');
    const c = CASES[1];
    const { view, dispatch, navigate, open } = await deepInList(c.state, c.testId);
    const search = api.search as ReturnType<typeof vi.fn>;
    const real = search.getMockImplementation()! as (...a: unknown[]) => unknown;
    const gate = deferred<void>();
    search.mockClear();
    search.mockImplementationOnce(async (...args: unknown[]) => { await gate.promise; return real(...args); });
    view.rerender(<Library state={{ ...open, headCommit: 'h2' }} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
    intersect(); // refused: the refresh is replacing the rows
    expect(search).toHaveBeenCalledTimes(1);
    await act(async () => { gate.resolve(); });
    await waitFor(() => expect(search.mock.calls.map(cl => cl[6])).toEqual([{ limit: 150, offset: 0 }, { limit: 50, offset: 150 }]));
    await waitFor(() => expect(paths(c.testId)).toHaveLength(160));
  });

  // L3: inserts above the selected row push it past the loaded rows. The
  // refresh reads on by exactly the number of rows inserted above it (40),
  // so the selected row is still loaded, selected and anchored.
  // Sabotage: no extension, and the selected row falls out of the list (no
  // selection, the reader's row gone).
  it('a selected row pushed past the loaded rows is read on to, bounded by the inserts above it', async () => {
    const { api } = await import('./api');
    const c = CASES[1];
    const { view, dispatch, navigate, open, scroller } = await deepInList(c.state, c.testId);
    const search = api.search as ReturnType<typeof vi.fn>;
    search.mockClear();
    list = [...Array.from({ length: 40 }, (_, i) => `kb/r/n${String(i).padStart(2, '0')}.md`), ...list];
    view.rerender(<Library state={{ ...open, headCommit: 'h2' }} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(paths(c.testId)).toHaveLength(190));
    expect(search.mock.calls.map(cl => cl[6])).toEqual([{ limit: 150, offset: 0 }, { limit: 40, offset: 150 }]);
    expect(selectedPath()).toBe(pathOf(120));
    expect(paths(c.testId).indexOf(pathOf(120))).toBe(160);
    expect(scroller.scrollTop).toBe(160 * ROW - 100);
  });

  // M3: a refresh while the list is hidden (FactSet Focus: display:none) reads
  // no rects. It must not anchor on row 0 or write scrollTop 0 into the
  // scroll memory; it re-anchors on the last place seen once the list shows.
  // Sabotage: capture the anchor from the hidden list (no isRendered check)
  // and the place is lost: the anchor resolves to nothing usable and the
  // scroll is not moved with the inserted row.
  it('a refresh while the list is hidden keeps the place for when it shows', async () => {
    const c = CASES[0];
    const { view, dispatch, navigate, open, scroller } = await deepInList(c.state, c.testId);
    fireEvent.scroll(scroller);
    await act(async () => { await new Promise(r => requestAnimationFrame(() => r(null))); });
    hidden = true;
    list = [NEW, ...list];
    view.rerender(<Library state={{ ...open, headCommit: 'h2' }} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(paths(c.testId)[0]).toBe(NEW));
    expect(scroller.scrollTop).toBe(120 * ROW - 100);
    expect(selectedPath()).toBe(pathOf(120));
    hidden = false;
    act(() => { for (const cb of resizeCallbacks) cb([], {} as ResizeObserver); });
    expect(scroller.scrollTop).toBe(121 * ROW - 100);
  });
});

// L2 (#739 review): path browse still re-reads its folder on a new head, and
// native scroll anchoring cannot keep the place (the scroll memory's restore
// puts the old scrollTop back over rows that moved). It re-anchors like the
// paged lists: entries inserted above the reader's top row move the scroll
// with them.
// Sabotage: drop the capture in the path effect and the scroll stays put
// while the rows move down by three.
describe('Library — path browse keeps the place on a new head', () => {
  it('three entries inserted above the top visible row move the scroll by three rows', async () => {
    const { api } = await import('./api');
    let names = Array.from({ length: 200 }, (_, i) => `f${String(i).padStart(3, '0')}.md`);
    (api.browse as ReturnType<typeof vi.fn>).mockImplementation(async () => ({
      path: 'kb', children: names.map(n => ({ name: n, is_dir: false, fullPath: `kb/${n}`, title: n })),
    }));
    const state: AppState = { ...repoBase, librarySort: 'path' };
    const view = render(<Library state={state} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(200));
    const scroller = screen.getAllByTestId('dir-entry')[0].parentElement!;
    scroller.scrollTop = 120 * ROW - 100;
    fireEvent.scroll(scroller);
    names = ['a0.md', 'a1.md', 'a2.md', ...names];
    view.rerender(<Library state={{ ...state, headCommit: 'h2' }} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(203));
    expect(scroller.scrollTop).toBe(123 * ROW - 100);
  });
});

// THE ~125px DRIFT (#739 review round 3). The window re-reads n rows, so each
// row inserted above the reader pushes one row out at the bottom. With the
// reader's row within a viewport of the window's end, the scroller then
// cannot scroll far enough to put it back: it clamps, and the row comes back
// lower by the inserted rows (3 x 47px in the browser; 3 x ROW here). The
// refresh reads on by exactly the rows inserted above the anchor, so the
// content below it is kept and the restore is exact.
// Sabotage: read on only when the anchor fell out of the window (the old L3
// rule) and the scroll clamps at the bottom.
describe('Library — a reader at the bottom of the loaded rows', () => {
  it('rows inserted above keep the anchor exact, though the scroller clamps', async () => {
    const c = CASES[0];
    const { view, dispatch, navigate, open, scroller } = await deepInList(c.state, c.testId);
    // A real scroller cannot scroll past its content: emulate the clamp.
    let top = 0;
    Object.defineProperty(scroller, 'scrollTop', {
      configurable: true,
      get() { return top; },
      set(v: number) {
        const rows = scroller.querySelectorAll('[data-path]').length;
        top = Math.max(0, Math.min(v, rows * ROW - 400));
      },
    });
    // The selected row 145 at 300px: 4 rows below it, so the list ends at the
    // bottom of the viewport (scrollTop = max).
    fireEvent.click(screen.getAllByTestId(c.testId)[145]);
    const at145 = { ...open, factPath: pathOf(145) };
    view.rerender(<Library state={at145} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(selectedPath()).toBe(pathOf(145)));
    scroller.scrollTop = 145 * ROW - 300;
    expect(scroller.scrollTop).toBe(150 * ROW - 400);
    list = ['kb/r/x0.md', 'kb/r/x1.md', 'kb/r/x2.md', ...list];
    view.rerender(<Library state={{ ...at145, headCommit: 'h2' }} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(paths(c.testId)[0]).toBe('kb/r/x0.md'));
    await waitFor(() => expect(paths(c.testId)).toHaveLength(153));
    expect(paths(c.testId).indexOf(pathOf(145))).toBe(148);
    expect(scroller.scrollTop).toBe(148 * ROW - 300);
  });
});

// R1 (#739 re-review): an EMPTY landed list has no place to keep. A new head
// must reload it from page 1, or the fact the agent just wrote never shows:
// a refresh of 0 rows reads nothing and applies nothing.
// Sabotage: drop the "rows > 0" guard and the list stays empty.
describe('Library — an empty list shows the first fact written', () => {
  for (const c of [CASES[0], CASES[2]]) {
    it(`${c.name}: empty, then a write and a new head: the fact appears and data-count is 1`, async () => {
      list = [];
      const dispatch = vi.fn();
      const view = render(<Library state={c.state} dispatch={dispatch} navigate={vi.fn()} />);
      await waitFor(() => expect(screen.getByTestId('library-header').getAttribute('data-count')).toBe('0'));
      await act(async () => { await Promise.resolve(); });
      expect(screen.queryAllByTestId(c.testId)).toHaveLength(0);
      list = [NEW];
      view.rerender(<Library state={{ ...c.state, headCommit: 'h2' }} dispatch={dispatch} navigate={vi.fn()} />);
      await waitFor(() => expect(paths(c.testId)).toEqual([NEW]));
      expect(screen.getByTestId('library-header').getAttribute('data-count')).toBe('1');
    });
  }
});

// R2 (#739 re-review): while a relevance search's page 1 loads (here from an
// empty folder), the list does not claim "No facts match the search.".
// Sabotage: drop the !relLoading check on the relevance empty state.
describe('Library — no false empty state while a search loads', () => {
  it('page 1 in flight from an empty folder: no "No facts match"', async () => {
    const { api } = await import('./api');
    (api.browse as ReturnType<typeof vi.fn>).mockResolvedValue({ path: 'kb', children: [] });
    const search = api.search as ReturnType<typeof vi.fn>;
    const gate = deferred<{ results: unknown[]; total: number }>();
    search.mockImplementationOnce(() => gate.promise);
    const pathState: AppState = { ...repoBase, librarySort: 'path' };
    const view = render(<Library state={pathState} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getByText('No items in this path.')).toBeTruthy());
    view.rerender(<Library state={{ ...pathState, freeText: 'agentic' }} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
    expect(screen.queryByText('No facts match the search.')).toBeNull();
    await act(async () => { gate.resolve({ results: [], total: 0 }); });
    await waitFor(() => expect(screen.getByText('No facts match the search.')).toBeTruthy());
  });
});

// A lens list moves with ANY member's content (user ruling 2026-11-03): a
// `content` notice on GET /lenses/{lens}/events (here for a READ member: the
// write repo's head does not move) runs the same quiet keep-place refresh.
// Repo lists hold no lens stream and ignore it.
// Sabotage: leave lensRev out of the lens list's head, and the notice
// refreshes nothing (the new row never shows).
describe('Library — a read member\'s content notice refreshes the lens list', () => {
  for (const c of [CASES[2], CASES[3]]) {
    it(`${c.name}: a notice with no write-repo head change refreshes and keeps row 120`, async () => {
      const { api } = await import('./api');
      const { scroller } = await deepInList(c.state, c.testId);
      const stream = latestStream('/api/v1/lenses/eng/events');
      expect(stream).toBeDefined();
      const call = (c.state.freeText ? api.lensSearch : api.listLensFacts) as ReturnType<typeof vi.fn>;
      call.mockClear();
      list = [NEW, ...list];
      act(() => { stream!.emit('content', { repo: 'docs', id: 'bbbbbbbbbbbb', branch: 'main', head: 'abc' }); });
      await waitFor(() => expect(paths(c.testId)[0]).toBe(NEW));
      expect(call).toHaveBeenCalled();
      expect(selectedPath()).toBe(pathOf(120));
      expect(paths(c.testId).indexOf(pathOf(120))).toBe(121);
      expect(scroller.scrollTop).toBe(121 * ROW - 100);
    });
  }

  it('repo lists open no lens stream, and a stray notice changes nothing', async () => {
    const { api } = await import('./api');
    const { open } = await deepInList(CASES[0].state, CASES[0].testId);
    void open;
    expect(streamsMatching('/lenses/').filter(es => !es.closedByClient)).toHaveLength(0);
    const recent = api.recent as ReturnType<typeof vi.fn>;
    recent.mockClear();
    list = [NEW, ...list];
    await act(async () => { await Promise.resolve(); });
    expect(recent).not.toHaveBeenCalled();
    expect(paths(CASES[0].testId)[0]).toBe(pathOf(0));
  });
});
