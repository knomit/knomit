import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { MaterializeView } from './MaterializeView';
import { useMaterializeStream } from './useMaterializeStream';
import { fixtureId, makeFixtureApi } from './materializeFixture';
import type { FixtureOptions } from './materializeFixture';
import { DEFAULT_OPTIONS, MatError } from './materializeApi';
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

function Harness({ api, save, initial = DEFAULT_OPTIONS, onOptions }: {
  api: MaterializeApi; save?: (n: string, t: string, ty?: string) => Promise<SaveOutcome>; initial?: MatOptions; onOptions?: (o: MatOptions) => void;
}) {
  const [options, setOptions] = useState(initial);
  return (
    <MaterializeView target={TARGET} title="Fact number 1" options={options} api={api} save={save}
      onOptionsChange={o => { setOptions(o); onOptions?.(o); }} onBack={() => {}} />
  );
}

function setup(fx: FixtureOptions = {}, extra: { save?: (n: string, t: string, ty?: string) => Promise<SaveOutcome>; initial?: MatOptions } = {}) {
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

    it('lands on the nearest earlier section and says so when that section is gone', async () => {
      setup({ total: 12, pageSize: 3 });
      await waitFor(() => expect(ns()).toHaveLength(3));
      layout();
      const input = screen.getByTestId('mat-facts');
      fireEvent.change(input, { target: { value: '2' } });                 // the new walk has only §1 and §2
      fireEvent.blur(input);
      const note = await screen.findByTestId('mat-notice');
      expect(note).toHaveTextContent('Fact number 3');
      expect(note).toHaveTextContent('nearest earlier section');
      expect(note).toHaveTextContent('Fact number 2');
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
    expect(bar).toHaveTextContent('of 10');
    expect(bar).toHaveTextContent('Max facts');
    expect(bar).toHaveTextContent('of 500');
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
    for (const [id, label] of [['mat-export', 'Export'], ['mat-focus', 'Focus']]) {
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

  it('numbers sections like a document: the root is the unnumbered top, children 1 and 2, grandchildren 1.1, and a repeat links to its first number', async () => {
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
    expect(within(secs[3]).getByTestId('mat-builds-link')).toHaveTextContent('1.1');
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

  it('is off by default, and the toggle hides the shell regions (data-mat-focus on <html>) and shows them again', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    const t = screen.getByTestId('mat-focus');
    expect(on()).toBe(false);
    expect(t).toHaveAttribute('aria-pressed', 'false');
    expect(t).toHaveAttribute('aria-label', 'Focus');
    fireEvent.click(t);
    expect(on()).toBe(true);
    expect(t).toHaveAttribute('aria-pressed', 'true');
    expect(t).toHaveAttribute('aria-label', 'Exit focus');
    expect(t).toHaveAttribute('title', 'Exit focus');
    fireEvent.click(t);
    expect(on()).toBe(false);
  });

  it('Esc leaves focus; Esc with focus off does nothing', async () => {
    setup({ total: 3, pageSize: 3 });
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(on()).toBe(false);
    fireEvent.click(screen.getByTestId('mat-focus'));
    expect(on()).toBe(true);
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(on()).toBe(false);
    expect(screen.getByTestId('mat-focus')).toHaveAttribute('aria-pressed', 'false');
  });

  it('Esc in focus is kept from the shell\'s own Escape handler (which would clear the filters and close the fact)', async () => {
    const shellEsc = vi.fn();
    const shell = (e: KeyboardEvent) => { if (e.key === 'Escape') shellEsc(); };
    window.addEventListener('keydown', shell); // registered BEFORE the view's, as the app's is
    try {
      setup({ total: 3, pageSize: 3 });
      await waitFor(() => expect(ns()).toHaveLength(3));
      fireEvent.click(screen.getByTestId('mat-focus'));
      fireEvent.keyDown(window, { key: 'Escape' });
      expect(on()).toBe(false);
      expect(shellEsc).not.toHaveBeenCalled();
      fireEvent.keyDown(window, { key: 'Escape' }); // focus is off: the shell has it again
      expect(shellEsc).toHaveBeenCalledTimes(1);
    } finally {
      window.removeEventListener('keydown', shell);
    }
  });

  it('Back leaves focus, and so does leaving the view', async () => {
    const onBack = vi.fn();
    const f = makeFixtureApi({ total: 3, pageSize: 3 });
    const { unmount } = render(<MaterializeView target={TARGET} title="T" options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={onBack} api={f.api} />);
    await waitFor(() => expect(ns()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('mat-focus'));
    expect(on()).toBe(true);
    fireEvent.click(screen.getByTestId('mat-back'));
    expect(on()).toBe(false);
    expect(onBack).toHaveBeenCalled();
    fireEvent.click(screen.getByTestId('mat-focus'));
    expect(on()).toBe(true);
    unmount();
    expect(on()).toBe(false);
  });

  it('the stylesheet hides exactly the rail, the library column with its splitter, and the browse toolbar under that attribute, and the shell carries those hooks', () => {
    const css = readFileSync(resolve(__dirname, 'materialize.css'), 'utf8');
    const rule = css.slice(css.indexOf("html[data-mat-focus]"));
    for (const sel of [".k-rail", "[data-testid='browse-toolbar']", "[data-shell-region='library']", "[data-testid='library-splitter']"]) expect(rule).toContain(sel);
    expect(rule).toContain('display: none !important');
    const src = (f: string) => readFileSync(resolve(__dirname, f), 'utf8');
    expect(src('TargetRail.tsx')).toContain('className="k-rail"');
    expect(src('BrowseFacet.tsx')).toContain('data-shell-region="library"');
    expect(src('BrowseFacet.tsx')).toContain('testId="browse-toolbar"');
    expect(src('BrowseFacet.tsx')).toContain('testId="library-splitter"');
  });
});
