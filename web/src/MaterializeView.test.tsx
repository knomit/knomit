import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { BACK_TO_TOP_PX, MaterializeView } from './MaterializeView';
import { useMaterializeStream } from './useMaterializeStream';
import { fixtureId, makeFixtureApi } from './materializeFixture';
import type { FixtureOptions } from './materializeFixture';
import { DEFAULT_OPTIONS, MatError } from './materializeApi';
import { writeMaterializeParams } from './materializeOptions';
import { ChevronLeftIcon } from './icons';
import type { MaterializeApi, MatNode, MatOptions, MatPage } from './materializeApi';
import type { SaveOutcome } from './saveFile';

const TARGET = { repo: 'kb', branch: 'main', path: `kb/decisions/fixture/${fixtureId(1)}.md` };

// ── A controllable IntersectionObserver ──
class MockIO {
  static all: MockIO[] = [];
  live = false;
  cb: IntersectionObserverCallback;
  init?: IntersectionObserverInit;
  constructor(cb: IntersectionObserverCallback, init?: IntersectionObserverInit) { this.cb = cb; this.init = init; MockIO.all.push(this); }
  observe() { this.live = true; }
  unobserve() {}
  disconnect() { this.live = false; }
  takeRecords() { return []; }
}
const realIO = globalThis.IntersectionObserver;
beforeEach(() => {
  MockIO.all = [];
  globalThis.IntersectionObserver = MockIO as unknown as typeof IntersectionObserver;
});
afterEach(() => {
  globalThis.IntersectionObserver = realIO;
  vi.restoreAllMocks();
});

/** The reader scrolls near the bottom: every live observer reports the sentinel. */
function nearBottom() {
  act(() => {
    for (const o of MockIO.all.filter(x => x.live)) {
      o.cb([{ isIntersecting: true } as IntersectionObserverEntry], o as unknown as IntersectionObserver);
    }
  });
}
const liveObservers = () => MockIO.all.filter(o => o.live).length;
const ns = () => screen.queryAllByTestId('mat-section').map(e => Number((e as HTMLElement).dataset.matN));
const settle = () => act(() => new Promise<void>(r => setTimeout(r, 15)));

function Harness({ api, save, initial = DEFAULT_OPTIONS, onOptions, canChoose = () => true }: {
  api: MaterializeApi; save?: (n: string, t: string, ty?: string) => Promise<SaveOutcome>; initial?: MatOptions; onOptions?: (o: MatOptions) => void; canChoose?: () => boolean;
}) {
  const [options, setOptions] = useState(initial);
  return (
    <MaterializeView target={TARGET} title="Fact number 1" options={options} api={api} save={save} canChoose={canChoose}
      onOptionsChange={o => { setOptions(o); onOptions?.(o); }} onBack={() => {}} />
  );
}

function setup(fx: FixtureOptions = {}, extra: { save?: (n: string, t: string, ty?: string) => Promise<SaveOutcome>; initial?: MatOptions; canChoose?: () => boolean } = {}) {
  const f = makeFixtureApi(fx);
  const utils = render(<Harness api={f.api} {...extra} />);
  return { ...f, ...utils };
}

describe('streaming', () => {
  it('renders page 1 at once and fetches the next page only when the reader is near the bottom', async () => {
    const { pageCalls } = setup({ total: 9, pageSize: 3 });
    await waitFor(() => expect(ns()).toEqual([1, 2, 3]));
    await settle();
    expect(pageCalls()).toHaveLength(1);
    expect(pageCalls()[0].cursor).toBeNull();

    nearBottom();
    await waitFor(() => expect(ns()).toEqual([1, 2, 3, 4, 5, 6]));
    expect(pageCalls()).toHaveLength(2);
    expect(pageCalls()[1].cursor).not.toBeNull();
  });

  it('observes the tail with a margin below the pane, not at its edge', async () => {
    setup({ total: 9, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    const io = MockIO.all.find(o => o.live)!;
    expect(io.init?.rootMargin).toMatch(/^0px 0px [1-9]\d*px 0px$/);
  });

  it('appends pages in order, and never shows a section twice even when pages overlap', async () => {
    const mk = (from: number, to: number, cursor: string | null): MatPage => ({
      root: 'r', anchor: 'abc1234', order: 'reading', unpinned: false, cross_kb: false, depth: 4, max_nodes: 200,
      total: 5, truncated: { depth: 0, nodes: 0 }, n_from: from, n_to: to, cursor, has_more: cursor !== null,
      facts: Array.from({ length: to - from + 1 }, (_, i) => ({
        n: from + i, path: `kb/x/${from + i}.md`, repo: 'r', commit: 'c', depth: 1, role: 'ref', title: `T${from + i}`,
        type: 'observation', kind: 'epistemic', confidence: 0.9, body: 'b', anchor_id: `f-${from + i}`,
      }) as MatNode),
    });
    const api: MaterializeApi = {
      page: vi.fn(async (_t, _o, cursor) => (cursor === null ? mk(1, 3, 'c1') : mk(3, 5, null))),
      exportMarkdown: vi.fn(),
    };
    render(<Harness api={api} />);
    await waitFor(() => expect(ns()).toEqual([1, 2, 3]));
    nearBottom();
    await waitFor(() => expect(ns()).toEqual([1, 2, 3, 4, 5]));
  });

  it('shows a skeleton tail while a page loads, then an end marker with totals when the cursor is exhausted', async () => {
    let release!: () => void;
    let hold: Promise<void> = Promise.resolve();
    const { pageCalls } = setup({ total: 6, pageSize: 3, gate: () => hold });
    await waitFor(() => expect(ns()).toHaveLength(3));
    expect(screen.getByTestId('mat-skeleton')).toBeInTheDocument();
    hold = new Promise<void>(r => { release = r; });
    nearBottom();
    expect(screen.getByTestId('mat-skeleton')).toBeInTheDocument();
    release();
    await waitFor(() => expect(screen.getByTestId('mat-end')).toHaveTextContent('End · 6 facts · depth 4 reached'));
    expect(screen.queryByTestId('mat-skeleton')).toBeNull();
    expect(pageCalls()).toHaveLength(2);
  });

  it('makes no request once the cursor is exhausted, however often the reader scrolls', async () => {
    const { pageCalls } = setup({ total: 6, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    nearBottom();
    await waitFor(() => expect(screen.getByTestId('mat-end')).toBeInTheDocument());
    expect(liveObservers()).toBe(0);
    nearBottom();
    nearBottom();
    await settle();
    expect(pageCalls()).toHaveLength(2);
  });

  it('loadMore itself is inert after the last page and while a page is in flight', async () => {
    const f = makeFixtureApi({ total: 6, pageSize: 3 });
    const { result } = renderHook(() => useMaterializeStream(f.api, TARGET, DEFAULT_OPTIONS));
    await waitFor(() => expect(result.current.sections).toHaveLength(3));
    act(() => { result.current.loadMore(); result.current.loadMore(); });
    await waitFor(() => expect(result.current.tail).toBe('done'));
    expect(f.pageCalls()).toHaveLength(2); // the double call fetched ONE page
    act(() => { result.current.loadMore(); });
    await settle();
    expect(f.pageCalls()).toHaveLength(2); // exhausted: nothing more
  });

  it('keeps the rendered sections when a page fails, offers Retry inline, and retries the same cursor', async () => {
    const { pageCalls } = setup({ total: 9, pageSize: 3, failPageOnce: [2] });
    await waitFor(() => expect(ns()).toHaveLength(3));
    nearBottom();
    const err = await screen.findByTestId('mat-tail-error');
    expect(ns()).toEqual([1, 2, 3]);
    expect(err).toHaveTextContent('page 2 failed');
    // a failed tail does not hammer the server on further scrolling
    nearBottom();
    await settle();
    expect(pageCalls()).toHaveLength(2);

    fireEvent.click(within(err).getByTestId('mat-tail-retry'));
    await waitFor(() => expect(ns()).toEqual([1, 2, 3, 4, 5, 6]));
    expect(pageCalls()).toHaveLength(3);
    expect(pageCalls()[2].cursor).toBe(pageCalls()[1].cursor);
    expect(screen.queryByTestId('mat-tail-error')).toBeNull();
  });

  it('offers a restart that keeps the place when the server no longer holds the cursor', async () => {
    setup({ total: 9, pageSize: 3, failPageStatus: { 2: 410 } });
    await waitFor(() => expect(ns()).toHaveLength(3));
    nearBottom();
    expect(await screen.findByTestId('mat-restart')).toBeInTheDocument();
    expect(screen.queryByTestId('mat-tail-retry')).toBeNull();
  });
});

describe('first-page states', () => {
  it('loading, then error with Retry, then the article', async () => {
    let fail = true;
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    const api: MaterializeApi = { ...f.api, page: async (...a) => { if (fail) throw new MatError('boom', 500); return f.api.page(...a); } };
    render(<Harness api={api} />);
    expect(screen.getByTestId('mat-loading')).toBeInTheDocument();
    const err = await screen.findByTestId('mat-error');
    expect(err).toHaveTextContent('boom');
    fail = false;
    fireEvent.click(screen.getByTestId('mat-retry'));
    await waitFor(() => expect(ns()).toEqual([1, 2, 3]));
  });

  it('says so when there is nothing to write', async () => {
    const api: MaterializeApi = {
      page: async () => ({ root: 'r', anchor: 'a', order: 'reading', unpinned: false, cross_kb: false, depth: 4, max_nodes: 200, total: 0,
        truncated: { depth: 0, nodes: 0 }, facts: [], n_from: 0, n_to: 0, cursor: null, has_more: false }),
      exportMarkdown: vi.fn(),
    };
    render(<Harness api={api} />);
    expect(await screen.findByTestId('mat-empty')).toBeInTheDocument();
  });
});

describe('article marks', () => {
  it('sets the heading level by walk depth, groups a frame, and marks cross-KB, changed-since-cited and builds-on', async () => {
    setup({ total: 8, pageSize: 8, ancestors: 1 });
    await waitFor(() => expect(ns()).toHaveLength(8));
    const secs = screen.getAllByTestId('mat-section');
    expect(secs[0].querySelector('h2')).not.toBeNull();            // ancestor, depth 0
    expect(secs[2].querySelector('h3')).not.toBeNull();            // depth 1
    expect(secs[3].querySelector('h4')).not.toBeNull();            // depth 2
    expect(screen.getAllByTestId('mat-group').map(g => g.textContent)).toEqual(['Ancestors', 'This frame', 'What it stands on']);
    // n=7 is another KB's fact: repo badge and "latest · frozen", only there
    expect(within(secs[6]).getByTestId('mat-repo-badge')).toBeInTheDocument();
    expect(within(secs[6]).getByTestId('mat-frozen')).toHaveTextContent('latest · frozen');
    expect(within(secs[5]).queryByTestId('mat-repo-badge')).toBeNull();
    // n=5 cites n=4 at a different version: the mark sits on its builds-on link
    fireEvent.click(within(secs[4]).getByTestId('mat-builds-toggle'));
    expect(within(secs[4]).getByTestId('mat-changed')).toHaveTextContent('changed since cited');
    expect(within(secs[3]).queryByTestId('mat-changed')).toBeNull();
    // the meta line carries path, confidence and the pinned commit
    expect(within(secs[3]).getByTestId('mat-meta')).toHaveTextContent(/kb\/decisions\/fixture\/.*\.md.*confidence 0\.90.*@e25e/);
  });

  it('a builds-on link scrolls to the earlier section', async () => {
    setup({ total: 4, pageSize: 4 });
    await waitFor(() => expect(ns()).toHaveLength(4));
    const box = screen.getByTestId('mat-scroll');
    let top = 0;
    Object.defineProperty(box, 'scrollTop', { get: () => top, set: v => { top = v; }, configurable: true });
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
      const n = (this as HTMLElement).dataset?.matN;
      const y = n ? Number(n) * 100 - top : 0;
      return { top: y, bottom: y + 100, left: 0, right: 0, width: 0, height: 100, x: 0, y, toJSON() {} } as DOMRect;
    });
    fireEvent.click(within(screen.getAllByTestId('mat-section')[3]).getByTestId('mat-builds-toggle'));
    const link = within(screen.getAllByTestId('mat-section')[3]).getByTestId('mat-builds-link');
    fireEvent.click(link);
    await waitFor(() => expect(top).toBeGreaterThan(0));
  });

  it('an unpinned walk shows nothing about a target that is gone', async () => {
    setup({ total: 3, pageSize: 3 }, { initial: { ...DEFAULT_OPTIONS, unpin: true } });
    await waitFor(() => expect(ns()).toHaveLength(3));
    expect(document.body.textContent ?? '').not.toMatch(/gone|omitted|missing/i);
  });
});

describe('changing options mid-read', () => {
  it('RAISING only the count continues in place: nothing is dimmed or replaced, no section repeats', async () => {
    const { pageCalls } = setup({ total: 12, pageSize: 3 }, { initial: { ...DEFAULT_OPTIONS, maxFacts: 6 } });
    await waitFor(() => expect(ns()).toEqual([1, 2, 3]));
    nearBottom();
    await waitFor(() => expect(screen.getByTestId('mat-limit')).toBeInTheDocument());
    expect(ns()).toEqual([1, 2, 3, 4, 5, 6]);
    expect(screen.getByTestId('mat-limit')).toHaveTextContent('Stopped at your limit of 6 facts · 6 more not written');
    const before = screen.getAllByTestId('mat-section')[0];

    expect(screen.getByTestId('mat-raise')).toHaveTextContent('Show up to 12');
    fireEvent.click(screen.getByTestId('mat-raise'));
    await waitFor(() => expect(ns().length).toBeGreaterThan(6));
    expect(screen.getAllByTestId('mat-section')[0]).toBe(before);       // same DOM node: continued, not replaced
    expect(document.querySelector('[data-stale="true"]')).toBeNull();
    expect(ns()).toEqual(Array.from({ length: ns().length }, (_, i) => i + 1)); // contiguous, no repeats
    expect(pageCalls().filter(c => c.cursor === null).at(-1)?.options.maxFacts).toBe(12);
  });

  it('changing the depth restarts from a new first call and dims the old text until page 1 arrives', async () => {
    let release!: () => void;
    let hold: Promise<void> = Promise.resolve();
    const { pageCalls } = setup({ total: 9, pageSize: 3, gate: () => hold });
    await waitFor(() => expect(ns()).toEqual([1, 2, 3]));
    nearBottom();
    await waitFor(() => expect(ns()).toHaveLength(6));
    const firstCalls = pageCalls().length;

    hold = new Promise<void>(r => { release = r; });
    fireEvent.click(screen.getByLabelText('Raise Max depth'));
    // the old text stays, dimmed
    await waitFor(() => expect(document.querySelector('[data-stale="true"]')).not.toBeNull());
    expect(ns()).toHaveLength(6);
    const restarted = pageCalls().slice(firstCalls);
    expect(restarted).toHaveLength(1);
    expect(restarted[0].cursor).toBeNull();
    expect(restarted[0].options.depth).toBe(5);

    release();
    await waitFor(() => expect(document.querySelector('[data-stale="true"]')).toBeNull());
    expect(ns()).toEqual([1, 2, 3]); // the new walk's page 1 replaced the old text
  });

  it.each([
    ['Cross-KB', 'mat-cross-kb', { crossKb: true }],
    ['Unpin', 'mat-unpin', { unpin: true }],
  ])('toggling %s restarts from a new first call', async (_n, id, expected) => {
    const { pageCalls } = setup({ total: 6, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId(id));
    await waitFor(() => expect(pageCalls()).toHaveLength(2));
    expect(pageCalls()[1].cursor).toBeNull();
    expect(pageCalls()[1].options).toMatchObject(expected);
  });

  it('lowering the count restarts too', async () => {
    const { pageCalls } = setup({ total: 6, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByLabelText('Lower Max facts'));
    await waitFor(() => expect(pageCalls()).toHaveLength(2));
    expect(pageCalls()[1].cursor).toBeNull();
    expect(pageCalls()[1].options.maxFacts).toBe(150);
  });

  it('a count raise whose longer walk reorders what was read falls back to a restart that keeps the reader\'s place', async () => {
    // Breadth-first inclusion, depth-first order: raising the cap lets a new fact in
    // at position 2 and shifts everything after it, so "continue in place" cannot hold.
    const { pageCalls } = setup({ total: 12, pageSize: 3, shiftAbove: 6 }, { initial: { ...DEFAULT_OPTIONS, maxFacts: 6 } });
    await waitFor(() => expect(ns()).toEqual([1, 2, 3]));
    nearBottom();
    await waitFor(() => expect(ns()).toEqual([1, 2, 3, 4, 5, 6]));
    const box = screen.getByTestId('mat-scroll');
    let top = 0;
    Object.defineProperty(box, 'scrollTop', { get: () => top, set: v => { top = v; }, configurable: true });
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
      const n = (this as HTMLElement).dataset?.matN;
      const y = n ? (Number(n) - 3) * 100 : 0; // the section at the top of the pane is §3
      return { top: y, bottom: y + 100, left: 0, right: 0, width: 0, height: 100, x: 0, y, toJSON() {} } as DOMRect;
    });
    const bare = (el: Element) => el.querySelector('.k-mat-h')?.textContent?.replace(/^[\d.]+ /, '');
    const titleOfTop = bare(screen.getAllByTestId('mat-section')[2]);

    fireEvent.click(screen.getByTestId('mat-raise'));
    // the fallback dims the old text like any restart ...
    await waitFor(() => expect(pageCalls().filter(c => c.cursor === null)).toHaveLength(3)); // first, raise, fallback
    // ... and the place the reader was at comes back, on the section's NEW position
    await waitFor(() => expect(top).toBe(92)); // moved from §3 to §4: (4-3)*100 - 8
    const secs = screen.getAllByTestId('mat-section');
    expect(bare(secs[3])).toBe(titleOfTop);
    expect(document.querySelector('[data-stale="true"]')).toBeNull();
    expect(screen.queryByTestId('mat-notice')).toBeNull();
  });

  describe('keeping the place', () => {
    function layout() {
      const box = screen.getByTestId('mat-scroll');
      let top = 0;
      Object.defineProperty(box, 'scrollTop', { get: () => top, set: v => { top = v; }, configurable: true });
      vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
        const n = (this as HTMLElement).dataset?.matN;
        // the pane's top edge is 0; section 3 is the one at the top of it
        const y = n ? (Number(n) - 3) * 100 : 0;
        return { top: y, bottom: y + 100, left: 0, right: 0, width: 0, height: 100, x: 0, y, toJSON() {} } as DOMRect;
      });
      return () => top;
    }

    it('scrolls back to the section that was at the top once the new page 1 has it', async () => {
      setup({ total: 12, pageSize: 3 });
      await waitFor(() => expect(ns()).toHaveLength(3));
      const scrollTop = layout();
      fireEvent.click(screen.getByLabelText('Lower Max facts'));          // 200 -> 150: a restart; section 3 survives
      await waitFor(() => expect(scrollTop()).not.toBe(0));
      expect(scrollTop()).toBe(-8);                                        // section 3 is already at the top edge, less the 8px gutter
      expect(screen.queryByTestId('mat-notice')).toBeNull();
    });

    it('lands on the nearest earlier section when that section is gone, and shows NO banner about it', async () => {
      setup({ total: 12, pageSize: 3 });
      await waitFor(() => expect(ns()).toHaveLength(3));
      const scrollTop = layout();
      const input = screen.getByTestId('mat-facts');
      fireEvent.change(input, { target: { value: '2' } });                 // the new walk has only §1 and §2
      fireEvent.blur(input);
      await waitFor(() => expect(ns()).toEqual([1, 2]));
      await waitFor(() => expect(scrollTop()).toBe(-108));                 // section 2, at (2-3)*100 - 8: the nearest earlier one
      expect(screen.queryByTestId('mat-notice')).toBeNull();
      expect(document.body.textContent).not.toMatch(/not in this walk any more|nearest earlier section/);
    });
  });
});

describe('Export .md', () => {
  const saved: SaveOutcome = { kind: 'saved', where: '/home/me/exports/x.md' };

  it('saves the WHOLE document at the chosen options, not what has scrolled in, under the slugged name', async () => {
    const save = vi.fn().mockResolvedValue(saved);
    const { calls } = setup({ total: 9, pageSize: 3, rootTitle: 'Pinned by default: the materialize rulings (2026-10-28) and more words after' }, { save });
    await waitFor(() => expect(ns()).toEqual([1, 2, 3]));                  // only page 1 is on screen
    fireEvent.click(screen.getByTestId('mat-export'));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    const [name, text, type] = save.mock.calls[0];
    expect(name).toBe(`pinned-by-default-the-materialize-rulings-2026-${fixtureId(1)}.md`);
    expect(type).toBe('text/markdown');
    expect(text).toContain('Fact number 9');                               // all 9, though 3 are on screen
    expect(calls.filter(c => c.kind === 'export')[0].options).toEqual(DEFAULT_OPTIONS);
    expect(await screen.findByTestId('mat-export-done')).toHaveTextContent('Saved /home/me/exports/x.md');
  });

  it('saves under the name the server sent in Content-Disposition, not its own', async () => {
    const save = vi.fn().mockResolvedValue(saved);
    setup({ total: 3, pageSize: 3, serverName: 'server-chosen-name-0badf00d.md', rootTitle: 'Some other title' }, { save });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-export'));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save.mock.calls[0][0]).toBe('server-chosen-name-0badf00d.md');
  });

  it('never lets a header name carry a directory (the no-header fallback is the slug test above)', async () => {
    const save = vi.fn().mockResolvedValue(saved);
    setup({ total: 3, pageSize: 3, serverName: '../../etc/evil.md' }, { save });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-export'));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save.mock.calls[0][0]).toBe('evil.md');
  });

  it('writes no file when the export fails, says so, and Retry then saves the whole document', async () => {
    const save = vi.fn().mockResolvedValue(saved);
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    let fails = true;
    const api: MaterializeApi = { ...f.api, exportMarkdown: async (...a) => { if (fails) throw new MatError('stopped at page 3 of 7', 500); return f.api.exportMarkdown(...a); } };
    render(<Harness api={api} save={save} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-export'));
    const failed = await screen.findByTestId('mat-export-failed');
    expect(failed).toHaveTextContent('stopped at page 3 of 7');
    expect(failed).toHaveTextContent('No file was written');
    expect(save).not.toHaveBeenCalled();

    fails = false;
    fireEvent.click(screen.getByTestId('mat-export-retry'));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(screen.queryByTestId('mat-export-failed')).toBeNull();
  });

  it('clicking Export again while it runs cancels it, and nothing is saved', async () => {
    const save = vi.fn().mockResolvedValue(saved);
    let release!: () => void;
    const hold = new Promise<void>(r => { release = r; });
    let gateOn = false;
    const f = makeFixtureApi({ total: 3, pageSize: 3, gate: () => (gateOn ? hold : Promise.resolve()) });
    render(<Harness api={f.api} save={save} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    gateOn = true;
    const btn = screen.getByTestId('mat-export');
    fireEvent.click(btn);
    await waitFor(() => expect(btn).toHaveAttribute('aria-label', 'Cancel export'));
    fireEvent.click(btn);
    release();
    await waitFor(() => expect(btn).toHaveAttribute('aria-label', 'Export'));
    await settle();
    expect(save).not.toHaveBeenCalled();
    expect(screen.queryByTestId('mat-export-failed')).toBeNull();
  });
});

describe('source links', () => {
  function pageWith(url: string | undefined): MaterializeApi {
    return {
      page: async () => ({ root: 'r', anchor: 'abc1234', order: 'reading', unpinned: false, cross_kb: false, depth: 4, max_nodes: 200, total: 1,
        truncated: { depth: 0, nodes: 0 }, n_from: 1, n_to: 1, cursor: null, has_more: false,
        facts: [{ n: 1, path: 'kb/x/1.md', repo: 'r', commit: 'c', depth: 0, role: 'root', title: 'T', type: 'observation', kind: 'epistemic',
          confidence: 0.9, body: 'b', anchor_id: 'f-1', sources: [{ label: 'the source', url }] }],
      }),
      exportMarkdown: vi.fn(),
    };
  }
  it.each([['javascript:alert(1)'], ['data:text/html,<script>1</script>']])('renders %s as plain text, not a link', async url => {
    render(<Harness api={pageWith(url)} />);
    await waitFor(() => expect(ns()).toEqual([1]));
    fireEvent.click(screen.getByTestId('mat-sources-toggle'));
    expect(screen.getByText('the source')).toBeInTheDocument();
    expect(screen.getByTestId('mat-section').querySelector('a')).toBeNull();
  });
  it('renders an https url as a link with that href, opened safely', async () => {
    render(<Harness api={pageWith('https://example.com/a?b=1')} />);
    await waitFor(() => expect(ns()).toEqual([1]));
    fireEvent.click(screen.getByTestId('mat-sources-toggle'));
    const a = screen.getByText('the source').closest('a')!;
    expect(a).toHaveAttribute('href', 'https://example.com/a?b=1');
    expect(a).toHaveAttribute('rel', 'noopener noreferrer');
  });
});

describe('top bar', () => {
  it('shows the controls, the limits and progress, and Back calls back', async () => {
    const onBack = vi.fn();
    const f = makeFixtureApi({ total: 9, pageSize: 3 });
    render(<MaterializeView target={TARGET} title="T" options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={onBack} api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    const bar = screen.getByTestId('mat-bar');
    expect(bar).toHaveTextContent('Max depth');
    expect(bar).toHaveTextContent('Max facts');
    expect(bar.textContent).not.toMatch(/of 10|of 500/);                  // the limits are enforced, not printed
    expect(within(bar).getByTestId('mat-cross-kb')).not.toBeChecked();
    expect(within(bar).getByTestId('mat-unpin')).not.toBeChecked();
    expect(screen.getByTestId('mat-progress')).toHaveTextContent('3 of ≤9 facts · depth 4');
    expect(screen.getByTestId('mat-export')).toHaveAttribute('aria-label', 'Export');
    fireEvent.click(screen.getByTestId('mat-back'));
    expect(onBack).toHaveBeenCalled();
  });

  it('cannot raise the depth past 10 or the count past 500', async () => {
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    render(<MaterializeView target={TARGET} title="T" options={{ ...DEFAULT_OPTIONS, depth: 10, maxFacts: 500 }} onOptionsChange={() => {}} onBack={() => {}} api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    expect(screen.getByLabelText('Raise Max depth')).toBeDisabled();
    expect(screen.getByLabelText('Raise Max facts')).toBeDisabled();
  });
});

describe('polish: labels, numbering, bar', () => {
  it('has no MATERIALIZED label; Export and Focus are icon-only buttons with an aria-label and a tooltip, styled as the fact header\'s icon buttons', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    expect(document.body.textContent).not.toMatch(/MATERIALIZED/i);
    for (const [id, label] of [['mat-export', 'Export'], ['mat-focus', 'Exit focus']]) {  // focus is ON when the view opens
      const b = screen.getByTestId(id);
      expect(b.textContent).toBe('');                    // no text, only the glyph
      expect(b.querySelector('svg')).not.toBeNull();
      expect(b).toHaveAttribute('aria-label', label);
      expect(b).toHaveAttribute('title', label);         // the tooltip
      expect(b).toHaveClass('k-mat-icon');
      expect(b).toHaveClass('k-bare');                   // the app's bare-button focus treatment
    }
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    const rule = /^\.k-mat-icon \{([^}]*)\}/m.exec(css)![1];
    expect(rule).toMatch(/padding: 2px/);                // RightPanel's materialize/retract buttons
    expect(rule).toMatch(/opacity: 0\.75/);
    expect(css).toMatch(/\.k-mat-icon:hover \{ opacity: 1/);
    expect(css).toMatch(/\.k-mat-icon:active/);
  });

  it('while exporting, the Export icon becomes the app\'s spinning arc, the tooltip carries the bytes, and a second click cancels', async () => {
    let release!: () => void;
    const hold = new Promise<void>(r => { release = r; });
    let gateOn = false;
    const f = makeFixtureApi({ total: 3, pageSize: 3, gate: () => (gateOn ? hold : Promise.resolve()) });
    render(<Harness api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    const btn = screen.getByTestId('mat-export');
    expect(screen.queryByTestId('mat-export-spinner')).toBeNull();
    gateOn = true;
    fireEvent.click(btn);
    await waitFor(() => expect(screen.getByTestId('mat-export-spinner')).toHaveClass('icon-spin'));
    expect(btn).toHaveAttribute('title', expect.stringContaining('Cancel export'));
    expect(btn.textContent).toBe('');
    release();
    await waitFor(() => expect(screen.queryByTestId('mat-export-spinner')).toBeNull());
  });

  it('Builds on and Sources are collapsed labels with counts; click expands in place, click again collapses', async () => {
    setup({ total: 4, pageSize: 4 });
    await waitFor(() => expect(ns()).toHaveLength(4));
    const sec = screen.getAllByTestId('mat-section')[3]; // n=4: cites n=3 and has one source
    const built = within(sec).getByTestId('mat-builds-toggle');
    const srcs = within(sec).getByTestId('mat-sources-toggle');
    expect(built).toHaveTextContent('Builds on · 1');
    expect(srcs).toHaveTextContent('Sources · 1');
    expect(built).toHaveAttribute('aria-expanded', 'false');
    expect(srcs).toHaveAttribute('aria-expanded', 'false');
    expect(within(sec).queryByTestId('mat-builds-on')).toBeNull();
    expect(within(sec).queryByTestId('mat-sources')).toBeNull();

    fireEvent.click(built);
    expect(built).toHaveAttribute('aria-expanded', 'true');
    expect(within(sec).getByTestId('mat-builds-on')).toBeInTheDocument();
    expect(within(sec).queryByTestId('mat-sources')).toBeNull(); // the other stays closed
    fireEvent.click(srcs);
    expect(within(sec).getByTestId('mat-sources')).toHaveTextContent('file4.go');
    fireEvent.click(built);
    expect(built).toHaveAttribute('aria-expanded', 'false');
    expect(within(sec).queryByTestId('mat-builds-on')).toBeNull();
    // they are real buttons (keyboard reachable)
    expect(built.tagName).toBe('BUTTON');
  });

  it('the labels read as the old small uppercase labels (no chip), with a disclosure arrow that turns when open', async () => {
    setup({ total: 4, pageSize: 4 });
    await waitFor(() => expect(ns()).toHaveLength(4));
    const sec = screen.getAllByTestId('mat-section')[3];
    const built = within(sec).getByTestId('mat-builds-toggle');
    const arrow = () => built.querySelector('.k-mat-arrow') as HTMLElement;
    expect(arrow().querySelector('svg')).not.toBeNull();
    expect(arrow()).toHaveAttribute('data-open', 'false');
    fireEvent.click(built);
    expect(arrow()).toHaveAttribute('data-open', 'true');
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    const rule = /^\.k-mat-toggle \{([^}]*)\}/m.exec(css)![1];
    expect(rule).toMatch(/text-transform: uppercase/);
    expect(rule).toMatch(/border: none/);
    expect(rule).toMatch(/background: none/);
    expect(rule).not.toMatch(/border-radius/);
    expect(css).toMatch(/\.k-mat-arrow\[data-open='false'\] \{ transform: rotate\(-90deg\)/);
  });

  it('a section with no builds-on or sources shows no label', async () => {
    setup({ total: 1, pageSize: 1 });
    await waitFor(() => expect(ns()).toEqual([1]));
    expect(screen.queryByTestId('mat-builds-toggle')).toBeNull();
    expect(screen.queryByTestId('mat-sources-toggle')).toBeNull();
  });

  it('flags a changed-since-cited link on the closed label', async () => {
    setup({ total: 5, pageSize: 5 });
    await waitFor(() => expect(ns()).toHaveLength(5));
    const fifth = screen.getAllByTestId('mat-section')[4]; // n=5 cites n=4 at a different version
    expect(within(fifth.querySelector('[data-testid="mat-builds-toggle"]') as HTMLElement).getByRole('img')).toHaveAttribute('aria-label', expect.stringContaining('changed'));
    expect(within(screen.getAllByTestId('mat-section')[1]).getByTestId('mat-builds-toggle').querySelector('[role="img"]')).toBeNull();
  });

  it('numbers sections like a document: the root is the unnumbered top, children 1 and 2, grandchildren 1.1, and a repeated fact\'s link names its target by title only', async () => {
    // root(1) cites A(2) and D(4); A cites S(3); D also cites S (reached first under A)
    setup({ total: 4, pageSize: 4, tree: { 1: [2, 4], 2: [3], 4: [3] } });
    await waitFor(() => expect(ns()).toHaveLength(4));
    const secs = screen.getAllByTestId('mat-section');
    const num = (i: number) => (secs[i].querySelector('[data-testid="mat-num"]')?.textContent ?? '').trim();
    expect([0, 1, 2, 3].map(num)).toEqual(['', '1.', '1.1', '2.']);
    // the number is a prefix of the heading text itself, and no longer in the meta line
    expect(secs[1].querySelector('.k-mat-h')?.textContent).toMatch(/^1\. Fact number 2/);
    expect(secs[2].querySelector('.k-mat-h')?.textContent).toMatch(/^1\.1 Fact number 3/);
    expect(secs[0].querySelector('.k-mat-h')?.textContent).toMatch(/^Fact number 1/);
    for (const sec of secs) expect(within(sec).getByTestId('mat-meta').textContent).not.toMatch(/§|^\d/);
    expect(secs[1].querySelector('[data-testid="mat-meta"] [data-testid="mat-num"]')).toBeNull();
    fireEvent.click(within(secs[3]).getByTestId('mat-builds-toggle'));
    const link = within(secs[3]).getByTestId('mat-builds-link');
    // the link names its target by title only: no section number, as prefix or suffix
    expect(link.textContent).toMatch(/^↪ Fact number 3 [0-9a-f]{8}$/);
    expect(link.textContent).not.toMatch(/§|1\.1/);
  });

  it('a frame: ancestors are an unnumbered preamble, the frame the unnumbered top, numbers start at its children', async () => {
    // 1 ancestor, 2 the frame (cites 3 and 5), 3 cites 4
    setup({ total: 5, pageSize: 5, ancestors: 1, tree: { 2: [3, 5], 3: [4] } });
    await waitFor(() => expect(ns()).toHaveLength(5));
    const secs = screen.getAllByTestId('mat-section');
    const nums = secs.map(sec => (sec.querySelector('[data-testid="mat-num"]')?.textContent ?? '').trim());
    expect(nums).toEqual(['', '', '1.', '1.1', '2.']);
    expect(secs[0].querySelector('.k-mat-h')?.textContent).toMatch(/^Fact number 1/); // an ancestor: no number
    expect(secs[2].querySelector('.k-mat-h')?.textContent).toMatch(/^1\. Fact number 3/);
  });
});

describe('full width', () => {
  it('puts no max-width (or centring margin) on the article or the pane it scrolls in', () => {
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    for (const sel of ['.k-mat-article', '.k-mat-scroll', '.k-mat']) {
      const m = new RegExp(`^${sel.replace('.', '\\.')} \\{([^}]*)\\}`, 'm').exec(css);
      expect(m, sel).not.toBeNull();
      expect(m![1]).not.toMatch(/max-width|margin:\s*0 auto/);
    }
  });
});

describe('focus mode', () => {
  const on = () => document.documentElement.dataset.matFocus === '1';
  afterEach(() => { delete document.documentElement.dataset.matFocus; });

  it('opens IN focus (the shell\'s regions are hidden by data-mat-focus on <html>); the toggle leaves and re-enters it', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    const t = screen.getByTestId('mat-focus');
    expect(on()).toBe(true);
    expect(t).toHaveAttribute('aria-pressed', 'true');
    expect(t).toHaveAttribute('aria-label', 'Exit focus');
    expect(t).toHaveAttribute('title', 'Exit focus');
    fireEvent.click(t);
    expect(on()).toBe(false);
    expect(t).toHaveAttribute('aria-pressed', 'false');
    expect(t).toHaveAttribute('aria-label', 'Focus');
    fireEvent.click(t);
    expect(on()).toBe(true);
  });

  it('with focus OFF nothing is hidden: no attribute on <html>, and no inline style or class is put on the shell', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-focus'));
    expect(on()).toBe(false);
    expect(document.documentElement.getAttribute('data-mat-focus')).toBeNull();
    expect(document.documentElement.getAttribute('style')).toBeNull();
    expect(document.body.getAttribute('style')).toBeNull();
    // the hiding is only ever the stylesheet's rule keyed on that attribute
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    for (const line of css.split('\n').filter(l => /\.k-rail|library|browse-toolbar|facet-tabs/.test(l) && !l.startsWith('/*') && !l.startsWith(' '))) {
      expect(line.startsWith('html[data-mat-focus]'), line).toBe(true);
    }
  });

  it('Esc OUTSIDE a field exits Focus; with focus already off the shell gets Esc back', async () => {
    const shellEsc = vi.fn();
    const shell = (e: KeyboardEvent) => { if (e.key === 'Escape') shellEsc(); };
    window.addEventListener('keydown', shell); // registered BEFORE the view's, as the app's is
    try {
      setup({ total: 3, pageSize: 3 });
      await waitFor(() => expect(ns()).toHaveLength(3));
      expect(on()).toBe(true);
      (document.activeElement as HTMLElement | null)?.blur();
      fireEvent.keyDown(window, { key: 'Escape' });
      expect(on()).toBe(false);
      expect(shellEsc).not.toHaveBeenCalled();          // the fact is never closed by it
      expect(screen.getByTestId('materialize-view')).toBeInTheDocument();
      fireEvent.keyDown(window, { key: 'Escape' });      // focus is off: the shell has it again
      expect(shellEsc).toHaveBeenCalledTimes(1);
    } finally {
      window.removeEventListener('keydown', shell);
    }
  });

  it('Esc INSIDE a text/number field (Max depth, Max facts) or any input only leaves the field and keeps Focus', async () => {
    const shellEsc = vi.fn();
    const shell = (e: KeyboardEvent) => { if (e.key === 'Escape') shellEsc(); };
    window.addEventListener('keydown', shell);
    try {
      setup({ total: 3, pageSize: 3 });
      await waitFor(() => expect(ns()).toHaveLength(3));
      for (const id of ['mat-depth', 'mat-facts', 'mat-unpin']) {
        const input = screen.getByTestId(id) as HTMLInputElement;
        input.focus();
        expect(document.activeElement).toBe(input);
        fireEvent.keyDown(input, { key: 'Escape' });
        expect(document.activeElement).not.toBe(input);   // left the field
        expect(on()).toBe(true);                          // Focus kept
      }
      expect(shellEsc).not.toHaveBeenCalled();             // and the shell never saw it
      // the next Esc, now outside any field, exits
      fireEvent.keyDown(window, { key: 'Escape' });
      expect(on()).toBe(false);
    } finally {
      window.removeEventListener('keydown', shell);
    }
  });

  it('Back leaves the view and restores everything, from Focus ON and from Focus OFF', async () => {
    const onBack = vi.fn();
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    const { unmount } = render(<MaterializeView target={TARGET} title="T" options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={onBack} api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    expect(on()).toBe(true);
    fireEvent.click(screen.getByTestId('mat-back'));      // from Focus on
    expect(on()).toBe(false);
    expect(onBack).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByTestId('mat-focus'));      // back in
    fireEvent.click(screen.getByTestId('mat-focus'));      // and out again: focus off
    expect(on()).toBe(false);
    fireEvent.click(screen.getByTestId('mat-back'));      // from Focus off
    expect(on()).toBe(false);
    expect(onBack).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByTestId('mat-focus'));
    expect(on()).toBe(true);
    unmount();                                              // leaving the view, however it happens
    expect(on()).toBe(false);
  });

  it('the Focus glyph sits to the LEFT of the Export glyph, together in the bar\'s action group', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    const focus = screen.getByTestId('mat-focus');
    const exp = screen.getByTestId('mat-export');
    const group = screen.getByTestId('mat-actions');
    expect(focus.parentElement).toBe(group);
    expect(exp.parentElement).toBe(group);
    expect(focus.compareDocumentPosition(exp) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect([...group.children].map(c => c.getAttribute('data-testid'))).toEqual(['mat-focus', 'mat-export']);
  });

  it('Focus is not part of the URL options (a reading preference, not what is materialized)', () => {
    const search = writeMaterializeParams('', { open: true, options: DEFAULT_OPTIONS });
    expect(search).not.toMatch(/focus/i);
  });

  it('the stylesheet hides exactly the Browse/Activity/Settings tabs, the rail, the library column with its splitter, and the browse toolbar under that attribute (the brand and gear stay), and the shell carries those hooks', () => {
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    const rule = css.slice(css.indexOf("html[data-mat-focus]"));
    for (const sel of [".k-rail", "[data-testid='facet-tabs']", "[data-testid='browse-toolbar']", "[data-shell-region='library']", "[data-testid='library-splitter']"]) expect(rule).toContain(sel);
    expect(rule).toContain('display: none !important');
    // the top bar's brand, rail toggle, gear and server tabs are NOT in the hiding rule
    for (const keep of ['bar-brand', 'rail-toggle', 'server-toggle', 'server-tabs', 'toknomitr-bar']) expect(rule).not.toContain(keep);
    const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8');
    expect(src('TargetRail.tsx')).toContain('className="k-rail"');
    expect(src('TopBar.tsx')).toContain('data-testid="facet-tabs"');
    expect(src('BrowseFacet.tsx')).toContain('data-shell-region="library"');
    expect(src('BrowseFacet.tsx')).toContain('testId="browse-toolbar"');
    expect(src('BrowseFacet.tsx')).toContain('testId="library-splitter"');
  });
});

describe('back control', () => {
  it('is the app\'s bare back chevron: ChevronLeftIcon, no text, no border, labelled "Back to fact", same box as the library and timeline back buttons', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    const b = screen.getByTestId('mat-back');
    expect(b.textContent).toBe('');
    expect(b).toHaveAttribute('aria-label', 'Back to fact');
    expect(b).toHaveAttribute('title', 'Back to fact');
    expect(b.innerHTML).toBe(render(<ChevronLeftIcon color="#888" size={13} />).container.innerHTML);
    expect(b).not.toHaveClass('k-mat-btn');                      // no bordered button chrome
    expect(b.style.border === 'none' || b.style.borderStyle === 'none').toBe(true);
    expect(b.style.background).toBe('none');
    expect(b.style.width).toBe('20px');                          // LibraryHeader's library-back box
    expect(b.style.height).toBe('26px');
    const lib = readFileSync(resolve(__dirname, 'LibraryHeader.tsx'), 'utf8');
    expect(lib).toContain('<ChevronLeftIcon color={canBack ?');   // the control it copies
    expect(lib).toContain('width: 20, height: 26');
  });
});

describe('end of the article: more, and back to top', () => {
  it('"more" replaces "Go deeper": a quiet borderless text action, and it still goes one level deeper', async () => {
    const { pageCalls } = setup({ total: 3, pageSize: 3, depthBeyond: 4 });
    await waitFor(() => expect(screen.getByTestId('mat-end')).toBeInTheDocument());
    const more = screen.getByTestId('mat-deeper');
    expect(more.textContent).toBe('more');
    expect(document.body.textContent).not.toMatch(/Go deeper/);
    expect(more).toHaveClass('k-mat-more');
    expect(more).not.toHaveClass('k-mat-btn');                  // not the bordered button
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    const rule = /^\.k-mat-more \{([^}]*)\}/m.exec(css)![1];
    expect(rule).toMatch(/border: none/);
    expect(rule).toMatch(/background: none/);
    expect(css).toMatch(/\.k-mat-more:hover \{ color: #9fb0c8/);   // the facet panel's "+N more" hover colour
    fireEvent.click(more);
    await waitFor(() => expect(pageCalls().at(-1)?.options.depth).toBe(5));
  });

  it('the back-to-top arrow appears only after scrolling down, and scrolls the article to the top', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    const box = screen.getByTestId('mat-scroll');
    const scrollTo = vi.fn();
    (box as HTMLElement & { scrollTo: unknown }).scrollTo = scrollTo;
    expect(screen.queryByTestId('mat-top')).toBeNull();
    Object.defineProperty(box, 'scrollTop', { value: BACK_TO_TOP_PX - 100, configurable: true, writable: true });
    fireEvent.scroll(box);
    expect(screen.queryByTestId('mat-top')).toBeNull();          // not far enough yet
    (box as unknown as { scrollTop: number }).scrollTop = BACK_TO_TOP_PX + 100;
    fireEvent.scroll(box);
    const arrow = await screen.findByTestId('mat-top');
    expect(arrow).toHaveAttribute('aria-label', 'Back to top');
    expect(arrow).toHaveAttribute('title', 'Back to top');
    expect(arrow.querySelector('svg')).not.toBeNull();
    fireEvent.click(arrow);
    expect(scrollTo).toHaveBeenCalledWith(expect.objectContaining({ top: 0 }));
    (box as unknown as { scrollTop: number }).scrollTop = 0;
    fireEvent.scroll(box);
    expect(screen.queryByTestId('mat-top')).toBeNull();          // gone again at the top
  });
});

describe('export asks where to save', () => {
  const saved: SaveOutcome = { kind: 'saved', where: '/home/me/doc.md' };

  it('a cancelled save dialog writes nothing and shows nothing alarming (no status, no error, button back to idle)', async () => {
    const save = vi.fn().mockResolvedValue({ kind: 'cancelled' } as SaveOutcome);
    setup({ total: 3, pageSize: 3 }, { save });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-export'));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    await settle();
    for (const id of ['mat-export-done', 'mat-export-failed', 'mat-export-ready', 'mat-export-confirm']) expect(screen.queryByTestId(id)).toBeNull();
    expect(screen.getByTestId('mat-export')).toHaveAttribute('aria-label', 'Export');
    expect(document.body.textContent).not.toMatch(/cancel|Not saved/i);
  });

  it('a browser that cannot ask where to save says where the file will go BEFORE exporting; Cancel exports nothing', async () => {
    const save = vi.fn().mockResolvedValue(saved);
    const { calls } = setup({ total: 3, pageSize: 3 }, { save, canChoose: () => false });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-export'));
    const note = await screen.findByTestId('mat-export-confirm');
    expect(note).toHaveTextContent('cannot ask where to save');
    expect(note).toHaveTextContent('download location');
    expect(calls.filter(c => c.kind === 'export')).toHaveLength(0);
    expect(save).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId('mat-export-nevermind'));
    await settle();
    expect(screen.queryByTestId('mat-export-confirm')).toBeNull();
    expect(calls.filter(c => c.kind === 'export')).toHaveLength(0);
    expect(save).not.toHaveBeenCalled();
  });

  it('...and Download goes ahead, saving once', async () => {
    const save = vi.fn().mockResolvedValue({ kind: 'downloaded', name: 'x.md' } as SaveOutcome);
    const { calls } = setup({ total: 3, pageSize: 3 }, { save, canChoose: () => false });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-export'));
    fireEvent.click(await screen.findByTestId('mat-export-go'));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(calls.filter(c => c.kind === 'export')).toHaveLength(1);
  });

  it('a dialog the browser will not open without a fresh click: the export waits, "Save as…" opens it, and nothing is lost', async () => {
    const save = vi.fn()
      .mockResolvedValueOnce({ kind: 'needs-gesture' } as SaveOutcome)
      .mockResolvedValueOnce(saved);
    const { calls } = setup({ total: 3, pageSize: 3 }, { save });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-export'));
    const ready = await screen.findByTestId('mat-export-ready');
    expect(ready).toHaveTextContent('ready');
    expect(screen.queryByTestId('mat-export-done')).toBeNull();
    fireEvent.click(screen.getByTestId('mat-export-saveas'));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(2));
    expect(save.mock.calls[1][1]).toBe(save.mock.calls[0][1]);             // the same fetched document, not a second export
    expect(calls.filter(c => c.kind === 'export')).toHaveLength(1);
    expect(await screen.findByTestId('mat-export-done')).toHaveTextContent('Saved /home/me/doc.md');
  });

  it('is wired to the asking save, not the silent one', () => {
    const src = readFileSync(resolve(__dirname, 'MaterializeView.tsx'), 'utf8');
    expect(src).toContain('save = saveTextFileAs');
    expect(src).not.toMatch(/save = saveTextFile\b[^A]/);
  });
});

describe('Max depth / Max facts are clamped to the server\'s range', () => {
  // The server refuses depth outside 0..10 and max_nodes outside 1..500 (materialize.Options.Validate).
  async function typeIn(id: string, value: string, commit: 'blur' | 'enter' = 'blur') {
    const input = screen.getByTestId(id) as HTMLInputElement;
    fireEvent.change(input, { target: { value } });
    const shown = input.value;
    if (commit === 'blur') fireEvent.blur(input); else fireEvent.keyDown(input, { key: 'Enter' });
    return shown;
  }
  const callOpts = (f: ReturnType<typeof makeFixtureApi>) => f.pageCalls().map(c => c.options);

  it('Max facts: typing past 500 shows 500, typing 0 shows 1; blur commits the clamped value only', async () => {
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    render(<Harness api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    expect(await typeIn('mat-facts', '99999')).toBe('500');
    await waitFor(() => expect(callOpts(f).at(-1)?.maxFacts).toBe(500));
    expect(await typeIn('mat-facts', '0')).toBe('1');
    await waitFor(() => expect(callOpts(f).at(-1)?.maxFacts).toBe(1));
    expect(callOpts(f).every(o => o.maxFacts >= 1 && o.maxFacts <= 500)).toBe(true);   // nothing out of range reached the server
  });

  it('Max depth: typing past 10 shows 10, and 0 is allowed (the server accepts 0..10); Enter commits too', async () => {
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    render(<Harness api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    expect(await typeIn('mat-depth', '99', 'enter')).toBe('10');
    await waitFor(() => expect(callOpts(f).at(-1)?.depth).toBe(10));
    expect(await typeIn('mat-depth', '0')).toBe('0');
    await waitFor(() => expect(callOpts(f).at(-1)?.depth).toBe(0));
    expect(callOpts(f).every(o => o.depth >= 0 && o.depth <= 10)).toBe(true);
  });

  it('letters are dropped and an emptied field reverts on blur without sending anything', async () => {
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    render(<Harness api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    const before = f.pageCalls().length;
    expect(await typeIn('mat-facts', 'abc')).toBe('');
    expect((screen.getByTestId('mat-facts') as HTMLInputElement).value).toBe('200');   // reverted
    expect(f.pageCalls().length).toBe(before);
    expect(await typeIn('mat-facts', '2x5')).toBe('25');
  });

  it('the steppers stop at both ends of both fields (disabled, so no out-of-range value can be sent)', async () => {
    const lo = makeFixtureApi({ total: 3, pageSize: 3 });
    const { unmount } = render(<MaterializeView target={TARGET} title="T" options={{ depth: 0, maxFacts: 1, crossKb: false, unpin: false }} onOptionsChange={() => {}} onBack={() => {}} api={lo.api} />);
    await waitFor(() => expect(ns().length).toBeGreaterThan(0));
    expect(screen.getByLabelText('Lower Max depth')).toBeDisabled();
    expect(screen.getByLabelText('Lower Max facts')).toBeDisabled();
    expect(screen.getByLabelText('Raise Max depth')).not.toBeDisabled();
    expect(screen.getByLabelText('Raise Max facts')).not.toBeDisabled();
    unmount();
    const hi = makeFixtureApi({ total: 3, pageSize: 3 });
    render(<MaterializeView target={TARGET} title="T" options={{ depth: 10, maxFacts: 500, crossKb: false, unpin: false }} onOptionsChange={() => {}} onBack={() => {}} api={hi.api} />);
    await waitFor(() => expect(ns().length).toBeGreaterThan(0));
    expect(screen.getByLabelText('Raise Max depth')).toBeDisabled();
    expect(screen.getByLabelText('Raise Max facts')).toBeDisabled();
    expect(screen.getByLabelText('Lower Max depth')).not.toBeDisabled();
  });

  it('a step from 480 facts lands on 500, not 530; a step from 5 lands on 1, not -45', async () => {
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    const { unmount } = render(<Harness api={f.api} initial={{ ...DEFAULT_OPTIONS, maxFacts: 480 }} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByLabelText('Raise Max facts'));
    await waitFor(() => expect(callOpts(f).at(-1)?.maxFacts).toBe(500));
    unmount();
    const g = makeFixtureApi({ total: 3, pageSize: 3 });
    render(<Harness api={g.api} initial={{ ...DEFAULT_OPTIONS, maxFacts: 5 }} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByLabelText('Lower Max facts'));
    await waitFor(() => expect(callOpts(g).at(-1)?.maxFacts).toBe(1));
  });
});

describe('deep section numbers: three levels, then a guide line', () => {
  // 1 -> 2 -> 3 -> 4 -> 5 -> 6 -> 7: a chain, so 2 is "1.", 3 "1.1", 4 "1.1.1", 5 "1.1.1.1", 6 "1.1.1.1.1" ...
  const chain = { 1: [2], 2: [3], 3: [4], 4: [5], 5: [6], 6: [7] };
  const head = (i: number) => screen.getAllByTestId('mat-section')[i];
  const hnum = (i: number) => head(i).querySelector('[data-testid="mat-num"]')?.textContent?.trim() ?? '';

  it('levels 1-3 carry a number; level 4 and deeper carry none but a guide line, one indent step per extra level', async () => {
    setup({ total: 7, pageSize: 7, tree: chain });
    await waitFor(() => expect(ns()).toHaveLength(7));
    expect([0, 1, 2, 3, 4, 5, 6].map(hnum)).toEqual(['', '1.', '1.1', '1.1.1', '', '', '']);
    expect(head(3).className).not.toMatch(/k-mat-deep/);               // 1.1.1 is still a numbered level
    expect(head(4)).toHaveClass('k-mat-deep');
    expect(head(4).getAttribute('data-deep')).toBe('1');
    expect(head(5).getAttribute('data-deep')).toBe('2');
    expect(head(6).getAttribute('data-deep')).toBe('3');
    expect(head(4).style.getPropertyValue('--k-deep')).toBe('1');
    expect(head(6).style.getPropertyValue('--k-deep')).toBe('3');
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    const rule = /^\.k-mat-deep \{([^}]*)\}/m.exec(css)![1];
    expect(rule).toMatch(/border-left: 1px solid #2a3040/);            // thin, in the theme's muted line colour
    expect(rule).toMatch(/margin-left: calc\(var\(--k-deep, 1\) \* 14px\)/);
    expect(head(4).querySelector('.k-mat-h')?.textContent).toBe('Fact number 5: what it decided and why it matters to the one citing it');   // no number prefix text
  });

  it('a deep heading\'s tooltip carries its FULL position; numbered headings need none; builds-on links still show no number', async () => {
    setup({ total: 7, pageSize: 7, tree: chain });
    await waitFor(() => expect(ns()).toHaveLength(7));
    expect(head(4).querySelector('.k-mat-h')).toHaveAttribute('title', '1.1.1.1');
    expect(head(5).querySelector('.k-mat-h')).toHaveAttribute('title', '1.1.1.1.1');
    expect(head(6).querySelector('.k-mat-h')).toHaveAttribute('title', '1.1.1.1.1.1');
    expect(head(3).querySelector('.k-mat-h')?.hasAttribute('title')).toBe(false);
    fireEvent.click(within(head(4)).getByTestId('mat-builds-toggle'));
    expect(within(head(4)).getByTestId('mat-builds-link').textContent).not.toMatch(/\d\.\d/);
  });

  it('the heading level still follows depth and is capped at h6', async () => {
    setup({ total: 7, pageSize: 7, tree: chain });
    await waitFor(() => expect(ns()).toHaveLength(7));
    const levels = [0, 1, 2, 3, 4, 5, 6].map(i => head(i).querySelector('.k-mat-h')?.tagName);
    expect(levels.every(l => /^H[2-6]$/.test(l ?? ''))).toBe(true);
  });

  it('numbers (and tooltips) do not change as pages append', async () => {
    setup({ total: 7, pageSize: 2, tree: chain });
    await waitFor(() => expect(ns()).toHaveLength(2));
    const snap = () => screen.getAllByTestId('mat-section').map(sec => [sec.querySelector('[data-testid="mat-num"]')?.textContent ?? '', sec.querySelector('.k-mat-h')?.getAttribute('title') ?? '']);
    let before = snap();
    for (let i = 0; i < 3; i++) {
      nearBottom();
      await waitFor(() => expect(ns().length).toBeGreaterThan(before.length));
      const after = snap();
      expect(after.slice(0, before.length)).toEqual(before);        // every earlier section keeps its number and tooltip
      before = after;
    }
    expect(ns()).toHaveLength(7);
    expect(head(6).querySelector('.k-mat-h')).toHaveAttribute('title', '1.1.1.1.1.1');
  });
});
