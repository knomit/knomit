// The paging state machine behind the FactSet view (plan section 3.1, 3.3, 5).
//
// A FactSet is a first call (the frozen search + options) and cursor calls.
// Its numbers are fixed when it is created, so ANY option change is a restart:
// a new first call, the old text dimmed until the new page 1 arrives, then a
// seek back to the unit that was at the top (there is no 'continue', unlike the
// single-fact stream: a larger max_nodes re-plans every unit).
//
// The view shows ONE run: contiguous sections from unit `from` on. loadMore()
// appends the next page; loadBefore() prepends the whole previous unit
// (Q-UI9); seek(u) replaces the run with one starting at unit u (when u is
// already on screen the view scrolls instead and never calls this).
//
// Exactly one call is in flight at a time, guarded by a generation counter:
// a restart or a seek bumps it and aborts the call in flight, and a late
// answer from an older generation is dropped (the stale-closure lesson of the
// single-fact stream: the machine is a class, not refs read by closures).
//
// Errors (plan section 5): a 503 is retried after Retry-After, at most
// MAX_BUSY_RETRIES times, then shown with Retry; a 404 (the cursor expired:
// idle, a server restart, a close) restarts the SAME search with the same
// options, seeks back to the reader's unit and raises the "FactSet refreshed"
// notice; a 400 is shown, never retried; a 500 or a network error is shown
// with Retry and the loaded sections stay.

import { useEffect, useState, useSyncExternalStore } from 'react';
import { FsError } from './factsetApi';
import type { FactSetApi, FactSetNode, FactSetPage, FsErrorKind, FsSearch } from './factsetApi';
import type { MatOptions } from './materializeApi';
import { sameOptions } from './materializeOptions';

/** How many times a 503 is retried before it is shown. */
export const MAX_BUSY_RETRIES = 3;
/** The longest a Retry-After is honoured, in seconds (the server sends 1; a
 *  first call can already have waited factset_anchor_wait before answering). */
export const MAX_RETRY_AFTER_S = 10;

export type FsEnv = Omit<FactSetPage, 'facts'>;
export interface Land { unit: number; anchor?: string; n?: number; epoch: number }
/** What a reach looks for in a unit: a written section's anchor, or section n. */
export interface Target { unit: number; anchor?: string; n?: number }
export interface FsFault { kind: FsErrorKind; detail: string }

export interface FsStreamState {
  /** The run on screen, in FactSet order. */
  nodes: FactSetNode[];
  env: FsEnv | null;
  /** The FIRST page: loading, ready, or error. A restart or a seek over text
   *  keeps 'ready' and sets `stale`. */
  phase: 'loading' | 'ready' | 'error';
  error: FsFault | null;
  stale: boolean;
  /** After the run's last section: more (idle), loading, failed, or done. */
  tail: 'idle' | 'loading' | 'failed' | 'done';
  tailError: FsFault | null;
  /** Before the run's first unit: more (idle), loading, failed, or done. */
  head: 'idle' | 'loading' | 'failed' | 'done';
  headError: FsFault | null;
  /** Set after an expired FactSet was re-created; the view shows its notice. */
  notice: 'refreshed' | null;
  /** Counts the times a new run replaced the text. */
  epoch: number;
  /** Where the view should scroll once the run of epoch `land.epoch` is on
   *  screen: a unit (its root), an anchor in it, or the section n of the unit
   *  (a builds-on target, which may be a stub to follow). */
  land: Land | null;
  /** The latest live cursor (any of the FactSet's works for a seek or the export). */
  cursor: string | null;
}

const INITIAL: FsStreamState = {
  nodes: [], env: null, phase: 'loading', error: null, stale: false,
  tail: 'idle', tailError: null, head: 'done', headError: null, notice: null, epoch: 0, land: null, cursor: null,
};

interface Inputs { api: FactSetApi; search: FsSearch; options: MatOptions }

export function searchKey(s: FsSearch): string {
  const t = s.target.kind === 'repo' ? `repo\n${s.target.repo}\n${s.target.branch}` : `lens\n${s.target.lens}`;
  return `${t}\n${s.sort}\n${s.params}`;
}

function envOf(p: FactSetPage): FsEnv {
  const { facts: _facts, ...env } = p;
  void _facts;
  return env;
}

function fault(e: unknown): FsFault {
  if (e instanceof FsError) return { kind: e.kind, detail: e.detail };
  return { kind: 'failed', detail: e instanceof Error ? e.message : String(e) };
}

/** The unit a cursor "<id>.<unit>.<section>" points at. */
export function cursorUnit(c: string): number {
  const parts = c.split('.');
  return parts.length >= 3 ? Number(parts[parts.length - 2]) : 0;
}

/** Whether nodes hold the target: its written section (by anchor) or section n. */
function has(nodes: FactSetNode[], t: Target): boolean {
  return nodes.some(x => x.unit === t.unit && (
    (t.anchor !== undefined && x.anchor_id === t.anchor && (x.written || !!x.unavailable))
    || (t.n !== undefined && x.n === t.n)));
}

/** Nodes with each (unit, n) once, in the order first seen. */
function dedupe(nodes: FactSetNode[]): FactSetNode[] {
  const seen = new Set<string>();
  return nodes.filter(x => { const k = `${x.unit}.${x.n}`; if (seen.has(k)) return false; seen.add(k); return true; });
}

/** Thrown inside a call chain whose generation has moved on. */
const STALE = Symbol('stale');

export class FactSetStream {
  private state: FsStreamState = INITIAL;
  private listeners = new Set<() => void>();
  private inputs: Inputs | null = null;
  private seen: { key: string; options: MatOptions } | null = null;

  private gen = 0;
  private busy = false;
  private abort: AbortController | null = null;
  /** The position after the run's last section. */
  private tailCursor: string | null = null;
  /** The unit at the top of the reader's pane (the view reports it). */
  private topUnit = 1;
  /** Re-creations since a CURSOR call last succeeded: at most one in a row. A
   *  first call's own page never resets it: a FactSet whose every cursor call
   *  404s (another server without sticky routing, an owner mismatch) would
   *  otherwise be re-created on every sentinel tick. */
  private recoveries = 0;

  private sleep: (ms: number) => Promise<void>;

  constructor(sleep: (ms: number) => Promise<void> = ms => new Promise(r => setTimeout(r, ms))) {
    this.sleep = sleep;
  }

  subscribe = (l: () => void): (() => void) => {
    this.listeners.add(l);
    return () => { this.listeners.delete(l); };
  };
  getState = (): FsStreamState => this.state;

  private commit(patch: Partial<FsStreamState>): void {
    this.state = { ...this.state, ...patch };
    this.listeners.forEach(l => l());
  }

  /** Called by the hook, from an effect, with what the latest render asked for. */
  sync(inputs: Inputs): void {
    this.inputs = inputs;
    const key = searchKey(inputs.search);
    const p = this.seen;
    this.seen = { key, options: inputs.options };
    if (!p || p.key !== key) { this.start({ clear: true }); return; }
    if (!sameOptions(p.options, inputs.options)) this.start({ seekTo: this.topUnit });
  }

  /** The view tells the stream which unit is at the top of the pane. */
  reportTop = (unit: number): void => { if (unit > 0) this.topUnit = unit; };

  dispose(): void {
    this.gen++;
    this.busy = false;
    this.abort?.abort();
    this.closeCurrent();
    // A remount (StrictMode) must start again, not find the FactSet aborted.
    this.seen = null;
  }

  clearNotice = (): void => { if (this.state.notice) this.commit({ notice: null }); };

  /** The next page of the run, when there is one and nothing is in flight. */
  loadMore = (): void => {
    const s = this.state;
    if (this.busy || s.phase !== 'ready' || s.stale || s.tail !== 'idle' || !this.tailCursor) return;
    this.commit({ tail: 'loading', tailError: null });
    const gen = this.gen;
    const cursor = this.tailCursor;
    void this.chain(gen, async signal => {
      const page = await this.call(gen, sig => this.inputs!.api.next(this.inputs!.search.target, cursor, sig), signal);
      const have = new Set(this.state.nodes.map(n => `${n.unit}.${n.n}`));
      const add = page.facts.filter(n => !have.has(`${n.unit}.${n.n}`));
      this.tailCursor = page.cursor;
      this.recoveries = 0;
      this.commit({
        nodes: [...this.state.nodes, ...add], env: envOf(page), cursor: page.cursor ?? this.state.cursor,
        tail: page.has_more ? 'idle' : 'done',
      });
    }, f => this.commit({ tail: 'failed', tailError: f }), true);
  };

  /** Prepends the whole unit before the run (Q-UI9). */
  loadBefore = (): void => {
    const s = this.state;
    const from = s.nodes[0]?.unit ?? 0;
    if (this.busy || s.phase !== 'ready' || s.stale || s.head !== 'idle' || from <= 1 || !s.cursor) return;
    this.commit({ head: 'loading', headError: null });
    const gen = this.gen;
    const u = from - 1;
    void this.chain(gen, async signal => {
      const target = this.inputs!.search.target;
      let page = await this.call(gen, sig => this.inputs!.api.seek(target, this.state.cursor!, u, sig), signal);
      const got: FactSetNode[] = [];
      for (;;) {
        got.push(...page.facts.filter(n => n.unit === u));
        const last = page.facts[page.facts.length - 1];
        if (!page.has_more || !page.cursor || !last || last.unit > u || cursorUnit(page.cursor) > u) break;
        const c = page.cursor;
        page = await this.call(gen, sig => this.inputs!.api.next(target, c, sig), signal);
      }
      this.recoveries = 0;
      this.commit({ nodes: [...got, ...this.state.nodes], head: u > 1 ? 'idle' : 'done', cursor: page.cursor ?? this.state.cursor });
    }, f => this.commit({ head: 'failed', headError: f }), true);
  };

  /** Replaces the run with one starting at unit, reading on inside that unit
   *  until the anchor (or section n) has arrived, then lands on it (or the
   *  unit's root). */
  seek = (unit: number, anchor?: string, n?: number): void => {
    const s = this.state;
    const units = s.env?.units ?? 0;
    if (s.phase !== 'ready' || !s.cursor || unit < 1 || unit > units) return;
    this.gen++;
    this.abort?.abort();
    this.busy = false;
    const gen = this.gen;
    const cursor = s.cursor;
    this.commit({ stale: true });
    void this.chain(gen, async signal => {
      const page = await this.call(gen, sig => this.inputs!.api.seek(this.inputs!.search.target, cursor, unit, sig), signal);
      const r = await this.readOn(gen, signal, page.facts, page, { unit, anchor, n });
      this.recoveries = 0;
      this.showRun(r.nodes, r.page, { unit, anchor, n });
    }, f => this.commit({ stale: false, tail: 'failed', tailError: f }), true);
  };

  /** Brings a section of a unit on screen: when the unit is in the run, reads
   *  on from the run's tail until the target has arrived (or the cursor has
   *  passed the unit) and lands on it; otherwise seeks to the unit. */
  reach = (t: Target): void => {
    const s = this.state;
    const inRun = s.nodes.some(x => x.unit === t.unit);
    if (!inRun) { this.seek(t.unit, t.anchor, t.n); return; }
    if (has(s.nodes, t)) { this.commit({ land: { ...t, epoch: s.epoch } }); return; }
    if (this.busy || s.phase !== 'ready' || s.stale || s.tail !== 'idle' || !this.tailCursor) return;
    this.commit({ tail: 'loading', tailError: null });
    const gen = this.gen;
    const cursor = this.tailCursor;
    void this.chain(gen, async signal => {
      const target = this.inputs!.search.target;
      const page = await this.call(gen, sig => this.inputs!.api.next(target, cursor, sig), signal);
      const r = await this.readOn(gen, signal, [...this.state.nodes, ...page.facts], page, t);
      this.tailCursor = r.page.cursor;
      this.recoveries = 0;
      this.commit({
        nodes: dedupe(r.nodes), env: envOf(r.page), cursor: r.page.cursor ?? this.state.cursor,
        tail: r.page.has_more ? 'idle' : 'done', land: { ...t, epoch: this.state.epoch },
      });
    }, f => this.commit({ tail: 'failed', tailError: f }), true);
  };

  /** Reads next pages after `page` while the target is not in `nodes` and the
   *  cursor is still inside the target's unit (loadBefore's stop rule). */
  private async readOn(gen: number, signal: AbortSignal, nodes: FactSetNode[], page: FactSetPage, t: Target): Promise<{ nodes: FactSetNode[]; page: FactSetPage }> {
    if (t.anchor === undefined && t.n === undefined) return { nodes, page };
    const target = this.inputs!.search.target;
    let all = nodes;
    while (!has(all, t) && page.has_more && page.cursor && cursorUnit(page.cursor) <= t.unit) {
      const last = page.facts[page.facts.length - 1];
      if (last && last.unit > t.unit) break;
      const c = page.cursor;
      page = await this.call(gen, sig => this.inputs!.api.next(target, c, sig), signal);
      all = [...all, ...page.facts];
    }
    return { nodes: all, page };
  }

  /** Retry what failed: the first page, the tail, or the head. */
  retry = (): void => {
    const s = this.state;
    if (s.phase === 'error') { this.recoveries = 0; this.start({ clear: true }); return; }
    // Retry is the reader's own act, so an expired FactSet may be re-created
    // again (it cannot loop): without this a second expiry before any cursor
    // call succeeded would leave Retry asking the same dead cursor forever.
    if (s.tail === 'failed') {
      if (s.tailError?.kind === 'expired') this.recoveries = 0;
      this.commit({ tail: 'idle', tailError: null }); this.loadMore(); return;
    }
    if (s.head === 'failed') {
      if (s.headError?.kind === 'expired') this.recoveries = 0;
      this.commit({ head: 'idle', headError: null }); this.loadBefore();
    }
  };

  /** Re-creates an expired FactSet: the same search and options, back to the
   *  reader's unit, with the "FactSet refreshed" notice. The export calls this
   *  on a 404 too. */
  recover = (): void => {
    // Once in a row: while a re-creation has not yet brought a page, another
    // 404 (the export's, say) does not start a second one; and the re-creating
    // first call is not itself recoverable, so a gone repo or lens ends in
    // the error it answered, not in a loop.
    if (this.recoveries >= 1) return;
    this.recoveries++;
    this.start({ seekTo: this.topUnit, notice: 'refreshed' });
  };

  private closeCurrent(): void {
    const c = this.state.cursor;
    if (c && this.inputs) void this.inputs.api.close(this.inputs.search.target, c);
  }

  private start(o: { clear?: boolean; seekTo?: number; notice?: 'refreshed' }): void {
    if (!this.inputs) return;
    this.abort?.abort();
    this.gen++;
    this.busy = false;
    // The FactSet being replaced is no longer needed: close it (best effort)
    // so idle ones do not pile up before they expire.
    this.closeCurrent();
    this.tailCursor = null;
    const gen = this.gen;
    const keep = !o.clear && this.state.nodes.length > 0;
    if (keep) {
      this.commit({ stale: true, error: null, cursor: null, notice: o.notice ?? this.state.notice });
    } else {
      this.commit({ ...INITIAL, epoch: this.state.epoch, notice: o.notice ?? null });
    }
    const { search, options } = this.inputs;
    void this.chain(gen, async signal => {
      const first = await this.call(gen, sig => this.inputs!.api.first(search, options, sig), signal);
      if (first.units === 0 || !first.cursor) {
        this.commit({ ...INITIAL, env: envOf(first), phase: 'ready', tail: 'done', epoch: this.state.epoch + 1, notice: this.state.notice });
        return;
      }
      const to = o.seekTo ?? 1;
      if (to > 1 && to <= first.units) {
        this.commit({ cursor: first.cursor, env: envOf(first) });
        const c = first.cursor;
        const page = await this.call(gen, sig => this.inputs!.api.seek(search.target, c, to, sig), signal);
        this.recoveries = 0; // the seek is a cursor call
        this.showRun(page.facts, page, { unit: to });
        return;
      }
      this.showRun(first.facts, first, keep ? { unit: 1 } : undefined);
    }, f => this.commit({ stale: false, phase: 'error', error: f }), false);
  }

  /** Shows nodes as the new run; `page` is the last page read (its cursor
   *  continues the run). */
  private showRun(nodes: FactSetNode[], page: FactSetPage, land?: Target): void {
    const epoch = this.state.epoch + 1;
    this.tailCursor = page.cursor;
    const from = nodes[0]?.unit ?? 1;
    this.commit({
      nodes: dedupe(nodes), env: envOf(page), phase: 'ready', error: null, stale: false,
      tail: page.has_more ? 'idle' : 'done', tailError: null, head: from > 1 ? 'idle' : 'done', headError: null,
      epoch, land: land ? { ...land, epoch } : null, cursor: page.cursor ?? this.state.cursor,
    });
  }

  /** One API call with the 503 rule: retried after Retry-After, at most
   *  MAX_BUSY_RETRIES times. Throws STALE once the generation moved on. */
  private async call<T>(gen: number, f: (signal: AbortSignal) => Promise<T>, signal: AbortSignal): Promise<T> {
    for (let attempt = 0; ; attempt++) {
      try {
        const v = await f(signal);
        if (gen !== this.gen) throw STALE;
        return v;
      } catch (e) {
        if (e === STALE || gen !== this.gen || signal.aborted) throw STALE;
        if (e instanceof FsError && e.kind === 'busy' && attempt < MAX_BUSY_RETRIES) {
          await this.sleep(Math.min(MAX_RETRY_AFTER_S, Math.max(1, e.retryAfter ?? 1)) * 1000);
          if (gen !== this.gen) throw STALE;
          continue;
        }
        throw e;
      }
    }
  }

  /** Runs one chain of calls as THE call in flight. A 404 on a cursor call of
   *  a FactSet already read from (`recoverable`) re-creates it; any other
   *  failure, a 404 on a first call included, goes to onFail. Nothing of an
   *  older generation lands. */
  private async chain(gen: number, body: (signal: AbortSignal) => Promise<void>, onFail: (f: FsFault) => void, recoverable: boolean): Promise<void> {
    const ctl = new AbortController();
    this.abort = ctl;
    this.busy = true;
    try {
      await body(ctl.signal);
      if (this.gen === gen) this.busy = false;
    } catch (e) {
      if (this.gen === gen) this.busy = false;
      if (e === STALE || this.gen !== gen) return;
      const f = fault(e);
      // An expired cursor on a FactSet already read from: re-create it. A
      // first call (or the seek right after it) answered 404 is shown:
      // re-creating it would only ask the same route again.
      // Past the bound the 404 is shown where it happened (tail, head, seek).
      if (f.kind === 'expired' && recoverable && this.recoveries < 1) { this.recover(); return; }
      onFail(f);
    }
  }
}

export function useFactSetStream(api: FactSetApi, search: FsSearch, options: MatOptions) {
  const [stream] = useState(() => new FactSetStream());
  const state = useSyncExternalStore(stream.subscribe, stream.getState);
  // Re-run when the SEARCH or any OPTION changes (api is the caller's seam and
  // is stable for a view's life). `sync` reads what changed itself.
  const skey = searchKey(search);
  const okey = `${options.depth}|${options.references}|${options.crossKb}|${options.unpin}`;
  useEffect(() => {
    stream.sync({ api, search, options });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stream, skey, okey]);
  useEffect(() => () => stream.dispose(), [stream]);
  return {
    ...state,
    loadMore: stream.loadMore, loadBefore: stream.loadBefore, seek: stream.seek, reach: stream.reach, retry: stream.retry,
    recover: stream.recover, reportTop: stream.reportTop, clearNotice: stream.clearNotice,
  };
}
