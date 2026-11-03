import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';

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

beforeEach(async () => {
  vi.clearAllMocks();
  list = Array.from({ length: 160 }, (_, i) => pathOf(i));
  observers = [];
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
      expect(paths(c.testId)).toHaveLength(150);
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
