import { describe, expect, it, vi } from 'vitest';
import { FactSetStream, MAX_BUSY_RETRIES } from './useFactSetStream';
import { FsError } from './factsetApi';
import type { FactSetApi, FactSetNode, FactSetPage, FsSearch } from './factsetApi';
import { DEFAULT_OPTIONS } from './materializeApi';

const SEARCH: FsSearch = { target: { kind: 'repo', repo: 'kb', branch: 'main' }, sort: 'recent', params: 'path=kb%2Fu' };

function node(unit: number, n: number): FactSetNode {
  return {
    n, path: `kb/u/${unit}-${n}.md`, repo: 'r', commit: 'c', depth: n - 1, role: n === 1 ? 'root' : 'ref', title: `U${unit}.${n}`,
    type: 'observation', kind: 'epistemic', confidence: 0.7, anchor_id: `f-${unit}-${n}`, unit,
    number: n === 1 ? String(unit) : `${unit}.${n - 1}`, tree_depth: n === 1 ? 0 : 1, written: true,
  };
}

/** A FactSet of `units` units with two sections each, paged two sections at a
 *  time (one unit per page), cursors "<id>.<unit>". */
function pageAt(id: string, unit: number, units: number): FactSetPage {
  return {
    mode: 'factset', title: 'T', search_line: 's', anchor: 'a', unpinned: false, cross_kb: false, depth: 4, max_nodes: 200,
    facts_limit: 500, units, facts: [node(unit, 1), node(unit, 2)], unit_from: unit, unit_to: unit,
    cursor: `${id}.${unit + 1}.0`, has_more: unit < units,
  };
}

type Call = { op: string; arg?: string | number };

/** A fake API: each call is recorded; answers come from `answer`, which may
 *  return a promise the test resolves later. */
function fakeApi(answer: (c: Call, id: number) => Promise<FactSetPage>) {
  const calls: Call[] = [];
  let ids = 0;
  const api: FactSetApi = {
    first: () => { const c = { op: 'first' }; calls.push(c); return answer(c, ++ids); },
    next: (_t, cursor) => { const c = { op: 'next', arg: cursor }; calls.push(c); return answer(c, ids); },
    seek: (_t, cursor, unit) => { const c = { op: 'seek', arg: unit }; calls.push({ ...c, cursor } as Call); return answer(c, ids); },
    exportMarkdown: vi.fn(),
    close: vi.fn(async () => {}),
  };
  return { api, calls };
}

const flush = () => new Promise(r => setTimeout(r, 0));
const nextUnit = (cursor: string) => Number(cursor.split('.')[1]);

// Test 4 (plan 7.4): the stream. Each case asserts which calls were made and
// which sections ended up on screen.
//
// Sabotage, one run each: (a) drop the busy guard in loadMore (two calls in
// flight, "next" twice); (b) restart with continue instead of a seek (the
// option change shows unit 1); (c) retry a 503 forever (more than
// MAX_BUSY_RETRIES + 1 first calls); (d) a 404 shows an error instead of
// recovering; (e) drop the generation check (the late page lands).
describe('FactSetStream (test 4)', () => {
  it('pages in order, one call in flight, done at has_more=false', async () => {
    const units = 3;
    const { api, calls } = fakeApi(async (c, id) => c.op === 'first' ? pageAt(`F${id}`, 1, units) : pageAt(`F${id}`, nextUnit(String(c.arg)), units));
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await flush();
    expect(s.getState().nodes.map(n => n.number)).toEqual(['1', '1.1']);
    s.loadMore();
    s.loadMore(); // ignored: one call in flight
    await flush();
    s.loadMore();
    await flush();
    expect(calls.map(c => c.op)).toEqual(['first', 'next', 'next']);
    expect(s.getState().nodes.map(n => n.number)).toEqual(['1', '1.1', '2', '2.1', '3', '3.1']);
    expect(s.getState().tail).toBe('done');
    s.loadMore();
    expect(calls).toHaveLength(3);
  });

  it('an option change restarts (a new first call), dims the old text, then seeks back to the top unit', async () => {
    const { api, calls } = fakeApi(async (c, id) => c.op === 'seek' ? pageAt(`F${id}`, Number(c.arg), 9) : pageAt(`F${id}`, 1, 9));
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await flush();
    s.reportTop(4);
    s.sync({ api, search: SEARCH, options: { ...DEFAULT_OPTIONS, depth: 2 } });
    expect(s.getState().stale).toBe(true);
    expect(s.getState().nodes[0].unit).toBe(1); // the old text, dimmed
    await flush();
    expect(calls.map(c => c.op + (c.arg ?? ''))).toEqual(['first', 'first', 'seek4']);
    const st = s.getState();
    expect(st.stale).toBe(false);
    expect(st.nodes.map(n => n.number)).toEqual(['4', '4.1']);
    expect(st.land).toEqual({ unit: 4, epoch: st.epoch });
    expect(st.head).toBe('idle');
    expect(api.close).toHaveBeenCalledWith(SEARCH.target, 'F1.2.0'); // the superseded FactSet is closed
  });

  it('a 503 is retried after Retry-After, at most MAX_BUSY_RETRIES times, then fails with Retry', async () => {
    const waits: number[] = [];
    const { api, calls } = fakeApi(async () => { throw new FsError('the index moved', 503, 1); });
    const s = new FactSetStream(async ms => { waits.push(ms); });
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    for (let i = 0; i < 10; i++) await flush();
    expect(calls.filter(c => c.op === 'first')).toHaveLength(MAX_BUSY_RETRIES + 1);
    expect(waits).toEqual([1000, 1000, 1000]);
    expect(s.getState().phase).toBe('error');
    expect(s.getState().error).toEqual({ kind: 'busy', detail: 'the index moved' });
  });

  it('a delayed 503 that then succeeds never times out on the client', async () => {
    let n = 0;
    const { api } = fakeApi(async (_c, id) => {
      if (n++ === 0) { await new Promise(r => setTimeout(r, 30)); throw new FsError('busy', 503, 2); }
      return pageAt(`F${id}`, 1, 1);
    });
    const waits: number[] = [];
    const s = new FactSetStream(async ms => { waits.push(ms); });
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await new Promise(r => setTimeout(r, 60));
    expect(waits).toEqual([2000]);
    expect(s.getState().phase).toBe('ready');
    expect(s.getState().nodes.map(n => n.number)).toEqual(['1', '1.1']);
  });

  it('a 404 on a page re-creates the FactSet, seeks back to the reader\'s unit and raises the notice', async () => {
    let expired = false;
    const { api, calls } = fakeApi(async (c, id) => {
      if (c.op === 'next' && !expired) { expired = true; throw new FsError('Cursor not found', 404); }
      if (c.op === 'seek') return pageAt(`F${id}`, Number(c.arg), 9);
      return c.op === 'first' ? pageAt(`F${id}`, 1, 9) : pageAt(`F${id}`, nextUnit(String(c.arg)), 9);
    });
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await flush();
    s.reportTop(1);
    s.loadMore();
    for (let i = 0; i < 4; i++) await flush();
    expect(calls.map(c => c.op)).toEqual(['first', 'next', 'first']);
    expect(s.getState().notice).toBe('refreshed');
    expect(s.getState().nodes.map(n => n.unit)).toEqual([1, 1]);
    // From a later unit: the re-created FactSet seeks back to it.
    s.seek(5);
    await flush();
    s.reportTop(5);
    expired = false;
    s.loadMore();
    for (let i = 0; i < 4; i++) await flush();
    expect(calls.slice(3).map(c => c.op + (c.arg ?? ''))).toEqual(['seek5', 'nextF2.6.0', 'first', 'seek5']);
    expect(s.getState().nodes.map(n => n.number)).toEqual(['5', '5.1']);
    expect(s.getState().phase).toBe('ready');
  });

  it('units: 0 is an empty, finished FactSet with no cursor', async () => {
    const { api } = fakeApi(async () => ({ ...pageAt('F', 1, 0), facts: [], cursor: null, has_more: false }));
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await flush();
    const st = s.getState();
    expect(st.phase).toBe('ready');
    expect(st.env?.units).toBe(0);
    expect(st.nodes).toEqual([]);
    expect(st.tail).toBe('done');
    expect(st.cursor).toBeNull();
  });

  it('a late answer from an older generation is dropped', async () => {
    let releaseNext!: (p: FactSetPage) => void;
    const { api } = fakeApi((c, id) => {
      if (c.op === 'next') return new Promise(r => { releaseNext = r; });
      if (c.op === 'seek') return Promise.resolve(pageAt(`F${id}`, Number(c.arg), 9));
      return Promise.resolve(pageAt(`F${id}`, 1, 9));
    });
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await flush();
    s.loadMore();          // in flight...
    s.seek(7);             // ...a seek supersedes it
    await flush();
    releaseNext(pageAt('F1', 2, 9)); // the old page answers late
    await flush();
    expect(s.getState().nodes.map(n => n.number)).toEqual(['7', '7.1']);
  });

  it('loadBefore prepends the whole previous unit across pages', async () => {
    const { api, calls } = fakeApi(async (c, id) => {
      if (c.op === 'seek') return pageAt(`F${id}`, Number(c.arg), 9);
      if (c.op === 'next') return pageAt(`F${id}`, nextUnit(String(c.arg)), 9);
      return pageAt(`F${id}`, 1, 9);
    });
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await flush();
    s.seek(6);
    await flush();
    s.loadBefore();
    await flush();
    expect(calls.slice(1).map(c => c.op + (c.arg ?? ''))).toEqual(['seek6', 'seek5']);
    expect(s.getState().nodes.map(n => n.number)).toEqual(['5', '5.1', '6', '6.1']);
    expect(s.getState().head).toBe('idle');
  });
});
