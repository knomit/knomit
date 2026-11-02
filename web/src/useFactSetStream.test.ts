import { describe, expect, it, vi } from 'vitest';
import { FactSetStream, MAX_BUSY_RETRIES, MAX_RETRY_AFTER_S } from './useFactSetStream';
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
});

/** A FactSet of `units` units of `per` sections each, paged `size` sections at
 *  a time ACROSS unit boundaries, as the server pages (buildPage): cursors are
 *  "<id>.<unit>.<section>" (0-based section), each call recorded. */
function pagedApi(units: number, per: number, size: number, fail: (op: string, arg: string | number) => FsError | null = () => null) {
  const calls: string[] = [];
  const sec = (u: number, k: number): FactSetNode => ({
    ...node(u, k + 1), anchor_id: `f-${u}-${k + 1}`, number: k === 0 ? String(u) : `${u}.${k}`, tree_depth: k === 0 ? 0 : 1,
  });
  const from = (u: number, k: number): FactSetPage => {
    const facts: FactSetNode[] = [];
    let cu = u, ck = k;
    while (facts.length < size && cu <= units) {
      facts.push(sec(cu, ck));
      ck++;
      if (ck >= per) { cu++; ck = 0; }
    }
    return { ...pageAt('F', u, units), facts, unit_from: u, unit_to: facts[facts.length - 1].unit,
      cursor: `F.${cu}.${ck}`, has_more: cu <= units };
  };
  const api: FactSetApi = {
    first: async () => { calls.push('first'); const e = fail('first', ''); if (e) throw e; return from(1, 0); },
    next: async (_t, c) => {
      calls.push(`next ${c}`); const e = fail('next', c); if (e) throw e;
      const [, u, k] = c.split('.'); return from(Number(u), Number(k));
    },
    seek: async (_t, _c, u) => { calls.push(`seek ${u}`); const e = fail('seek', u); if (e) throw e; return from(u, 0); },
    exportMarkdown: vi.fn(), close: vi.fn(async () => {}),
  };
  return { api, calls };
}
const settle = async () => { for (let i = 0; i < 6; i++) await flush(); };

// H1: a 404 is recovered only on a cursor call, at most once in a row. A first
// call answered 404 (the repo or lens is gone) is an error, and a re-creation
// that is answered 404 ends there: the number of first calls is exact.
//
// Sabotage: let the re-creating first call recover too (chain(..., true) in
// start): the second case asks first() again and again.
describe('FactSetStream 404 recovery is bounded (H1)', () => {
  it('a 404 on the first call is shown, with no re-creation', async () => {
    const { api, calls } = pagedApi(3, 2, 2, op => op === 'first' ? new FsError('Not found', 404) : null);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    expect(calls).toEqual(['first']);
    expect(s.getState().phase).toBe('error');
    expect(s.getState().error).toEqual({ kind: 'expired', detail: 'Not found' });
    expect(s.getState().notice).toBeNull();
  });

  it('the route gone after page 1: one re-creation, then the error; exactly two first calls', async () => {
    let gone = false;
    const { api, calls } = pagedApi(3, 2, 2, () => gone ? new FsError('Lens not found', 404) : null);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    gone = true;
    s.loadMore();
    await settle();
    expect(calls).toEqual(['first', 'next F.2.0', 'first']);
    expect(s.getState().phase).toBe('error');
    expect(s.getState().error).toEqual({ kind: 'expired', detail: 'Lens not found' });
  });
});

// M1: a seek reads on inside its unit until the anchor has arrived (and no
// further), so a link to a section past the first page lands on it.
//
// Sabotage: drop readOn from seek: the run holds sections 1-5 of unit 3 only
// and the land anchor is not on screen.
describe('FactSetStream seek reads on to its target (M1)', () => {
  it('a back link to section 7 of unit 3 (pages of 5): seek, one next, stop; lands on that anchor', async () => {
    const { api, calls } = pagedApi(5, 9, 5);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    s.seek(3, 'f-3-7');
    await settle();
    expect(calls).toEqual(['first', 'seek 3', 'next F.3.5']);
    const st = s.getState();
    expect(st.nodes.some(n => n.anchor_id === 'f-3-7' && n.written)).toBe(true);
    expect(st.land).toEqual({ unit: 3, anchor: 'f-3-7', n: undefined, epoch: st.epoch });
    expect(st.nodes[0].anchor_id).toBe('f-3-1');
  });

  it('a target that is not in the unit stops at the unit end, never reads the next unit', async () => {
    const { api, calls } = pagedApi(5, 6, 5);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    s.seek(2, 'f-nowhere');
    await settle();
    // seek 2 -> 2.0..2.4 (cursor F.2.5), next -> 2.5, 3.0..3.3 (cursor F.3.4): past unit 2, stop.
    expect(calls).toEqual(['first', 'seek 2', 'next F.2.5']);
  });

  it('reach: section n of a unit already in the run is read on to from the tail (L3)', async () => {
    const { api, calls } = pagedApi(3, 9, 5);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    s.reach({ unit: 1, n: 8 });
    await settle();
    expect(calls).toEqual(['first', 'next F.1.5']);
    const st = s.getState();
    expect(st.nodes.some(n => n.unit === 1 && n.n === 8)).toBe(true);
    expect(st.land).toMatchObject({ unit: 1, n: 8 });
  });
});

// L1: loadBefore reads the previous unit over SEVERAL pages and stops on the
// cursor's unit, never fetching the page after it.
//
// Sabotage: drop the cursorUnit(page.cursor) > u stop: an extra next call
// (the one that starts at unit 3's first section) is made.
describe('FactSetStream loadBefore across pages (L1)', () => {
  it('prepends the whole of unit 2 read in two pages, then stops', async () => {
    const { api, calls } = pagedApi(4, 4, 2);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    s.seek(3);
    await settle();
    calls.length = 0;
    s.loadBefore();
    await settle();
    expect(calls).toEqual(['seek 2', 'next F.2.2']);
    expect(s.getState().nodes.map(n => n.number)).toEqual(['2', '2.1', '2.2', '2.3', '3', '3.1']);
    expect(s.getState().head).toBe('idle');
  });

  it('a loadBefore failure is a head error, not a tail error (L2)', async () => {
    let fail = false;
    const { api } = pagedApi(4, 2, 2, op => fail && op === 'seek' ? new FsError('boom', 500) : null);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    s.seek(3);
    await settle();
    fail = true;
    s.loadBefore();
    await settle();
    expect(s.getState().head).toBe('failed');
    expect(s.getState().headError).toEqual({ kind: 'failed', detail: 'boom' });
    expect(s.getState().tailError).toBeNull();
  });
});

// L4: Retry-After is honoured up to MAX_RETRY_AFTER_S.
// Sabotage: drop the clamp: the wait is 3600000 ms.
describe('FactSetStream Retry-After clamp (L4)', () => {
  it('a Retry-After of an hour waits MAX_RETRY_AFTER_S seconds', async () => {
    const waits: number[] = [];
    let n = 0;
    const { api } = fakeApi(async (_c, id) => { if (n++ === 0) throw new FsError('busy', 503, 3600); return pageAt(`F${id}`, 1, 1); });
    const s = new FactSetStream(async ms => { waits.push(ms); });
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    expect(MAX_RETRY_AFTER_S).toBe(10);
    expect(waits).toEqual([10000]);
    expect(s.getState().phase).toBe('ready');
  });
});

// R1: the bound holds when the re-created FactSet's first call succeeds but
// its cursor calls keep failing: only a CURSOR call's success resets it.
//
// Sabotage: reset the counter in showRun again (any page, the first call's
// own included): loadMore/reach re-create on every call.
describe('FactSetStream recovery bound with a healthy first call (R1)', () => {
  const always404 = (op: string) => op === 'first' ? null : new FsError('Cursor not found', 404);

  it('loadMore: first OK, every cursor call 404 -> exactly two first calls, then the error', async () => {
    const { api, calls } = pagedApi(9, 2, 2, always404);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    for (let i = 0; i < 5; i++) { s.loadMore(); await settle(); }
    expect(calls.filter(c => c === 'first')).toHaveLength(2);
    expect(calls).toEqual(['first', 'next F.2.0', 'first', 'next F.2.0']);
    expect(s.getState().tail).toBe('failed');
    expect(s.getState().tailError).toEqual({ kind: 'expired', detail: 'Cursor not found' });
  });

  it('reach: first OK, every cursor call 404 -> exactly two first calls, then the error', async () => {
    const { api, calls } = pagedApi(9, 6, 2, always404);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    for (let i = 0; i < 5; i++) { s.reach({ unit: 1, n: 5 }); await settle(); }
    expect(calls.filter(c => c === 'first')).toHaveLength(2);
    expect(s.getState().tail).toBe('failed');
    expect(s.getState().tailError?.kind).toBe('expired');
  });

  it('control: a recovery followed by a successful cursor page may recover once more later', async () => {
    let fails = 0;
    const { api, calls } = pagedApi(9, 2, 2, op => (op === 'next' && fails-- > 0) ? new FsError('Cursor not found', 404) : null);
    const s = new FactSetStream(async () => {});
    s.sync({ api, search: SEARCH, options: DEFAULT_OPTIONS });
    await settle();
    fails = 1;
    s.loadMore(); await settle();   // 404 -> recovery 1
    s.loadMore(); await settle();   // a cursor page succeeds: the bound resets
    fails = 1;
    s.loadMore(); await settle();   // a later expiry -> recovery 2
    expect(calls.filter(c => c === 'first')).toHaveLength(3);
    expect(s.getState().phase).toBe('ready');
    expect(s.getState().notice).toBe('refreshed');
  });
});
