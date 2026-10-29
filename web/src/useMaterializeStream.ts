// The paging state machine behind the materialize view.
//
// One WALK is a first call (options, no cursor) and the cursor calls that
// follow it. A walk is never fetched in one shot: page 1 is fetched when the
// walk starts, every later page only when loadMore() is called (the view calls
// it when the reader nears the bottom), and nothing is fetched once the cursor
// is exhausted.
//
// WHEN OPTIONS CHANGE (optionChange):
//   continue  only the count went up. What is on screen stays; a new walk is
//             started (a cursor is frozen to its first call's options) and its
//             pages are read PAST what is already rendered: nodes with an
//             ordinal already shown are checked against what is shown, then
//             dropped. A mismatch means the order was not stable, and the walk
//             is restarted instead.
//   restart   anything else. The old text stays, dimmed (`stale`), until the
//             new page 1 arrives and replaces it; `epoch` counts those
//             arrivals so the view can put the reader back where they were.
//
// The machine is a plain class (a store React subscribes to) rather than a pile
// of refs: its fetches outlive renders, and what they read must be what the
// latest render asked for, not what a stale closure captured.

import { useEffect, useState, useSyncExternalStore } from 'react';
import { MatError } from './materializeApi';
import type { MaterializeApi, MatNode, MatOptions, MatPage, MatTarget } from './materializeApi';
import { optionChange } from './materializeOptions';

export type StreamEnv = Omit<MatPage, 'facts'>;

export interface StreamState {
  sections: MatNode[];
  env: StreamEnv | null;
  /** The FIRST page of the walk: loading, ready (it arrived), or error. While
   *  a restart is in flight over old text this stays 'ready' and `stale` says
   *  the text is the old walk's. */
  phase: 'loading' | 'ready' | 'error';
  error: string | null;
  stale: boolean;
  /** After the first page: idle (more to fetch), loading, failed, or done. */
  tail: 'idle' | 'loading' | 'failed' | 'done';
  tailError: string | null;
  /** The failed call was refused because the server no longer holds the cursor. */
  tailExpired: boolean;
  /** Incremented each time a restart's first page replaces the text. */
  epoch: number;
}

const INITIAL: StreamState = {
  sections: [], env: null, phase: 'loading', error: null, stale: false,
  tail: 'idle', tailError: null, tailExpired: false, epoch: 0,
};

interface Inputs { api: MaterializeApi; target: MatTarget; options: MatOptions }

function targetKey(t: MatTarget): string {
  return `${t.repo}\n${t.branch}\n${t.path}\n${t.commit ?? ''}`;
}

function envOf(p: MatPage): StreamEnv {
  const { facts: _facts, ...env } = p;
  void _facts;
  return env;
}

export class MaterializeStream {
  private state: StreamState = INITIAL;
  private listeners = new Set<() => void>();
  private inputs: Inputs | null = null;
  private seen: { key: string; options: MatOptions } | null = null;

  private gen = 0;
  private cursor: string | null = null;
  private hasMore = false;
  private busy = false;
  private abort: AbortController | null = null;
  private mode: 'restart' | 'continue' = 'restart';
  /** Ordinals up to here are already on screen (continue mode). */
  private skipThrough = 0;
  private walkOptions!: MatOptions;

  subscribe = (l: () => void): (() => void) => {
    this.listeners.add(l);
    return () => { this.listeners.delete(l); };
  };
  getState = (): StreamState => this.state;

  private commit(patch: Partial<StreamState>): void {
    this.state = { ...this.state, ...patch };
    this.listeners.forEach(l => l());
  }

  /** Called by the hook, from an effect, with what the latest render asked for. */
  sync(inputs: Inputs): void {
    this.inputs = inputs;
    const key = targetKey(inputs.target);
    const p = this.seen;
    this.seen = { key, options: inputs.options };
    if (!p || p.key !== key) { this.start('restart', true); return; }
    const change = optionChange(p.options, inputs.options);
    if (change !== 'same') this.start(change);
  }

  dispose(): void {
    this.gen++;
    this.busy = false;
    this.abort?.abort();
    // A remount (StrictMode) must start the walk again, not find it aborted.
    this.seen = null;
  }

  /** Fetch the next page, if there is one and nothing is already in flight.
   *  The hook never calls this itself; the view does, near the bottom. */
  loadMore = (): void => {
    const s = this.state;
    if (this.busy || !this.hasMore || s.phase !== 'ready' || s.stale || s.tail !== 'idle') return;
    this.commit({ tail: 'loading' });
    void this.pull(this.gen);
  };

  /** Retry what failed: the first page (a new walk) or the failed cursor call. */
  retry = (): void => {
    if (this.cursor === null) { this.start(this.mode); return; }
    this.commit({ tail: 'loading', tailError: null, tailExpired: false });
    void this.pull(this.gen);
  };

  /** A new first call with the current options, keeping the text dimmed. */
  restart = (): void => this.start('restart');

  private start(mode: 'restart' | 'continue', clear = false): void {
    if (!this.inputs) return;
    this.abort?.abort();
    this.gen++;
    this.busy = false;
    this.cursor = null;
    this.hasMore = true;
    this.mode = mode;
    this.walkOptions = this.inputs.options;
    const have = clear ? [] : this.state.sections;
    this.skipThrough = mode === 'continue' && have.length ? have[have.length - 1].n : 0;
    if (clear) {
      this.commit({ ...INITIAL, epoch: this.state.epoch });
    } else if (mode === 'restart') {
      this.commit({ phase: have.length ? 'ready' : 'loading', error: null, stale: have.length > 0, tail: 'idle', tailError: null, tailExpired: false });
    } else {
      this.commit({ tail: 'loading', tailError: null, tailExpired: false });
    }
    void this.pull(this.gen);
  }

  private async pull(gen: number): Promise<void> {
    if (this.busy || this.gen !== gen || !this.inputs) return;
    this.busy = true;
    const ctl = new AbortController();
    this.abort = ctl;
    const first = this.cursor === null;
    let page: MatPage;
    try {
      page = await this.inputs.api.page(this.inputs.target, this.walkOptions, this.cursor, ctl.signal);
    } catch (e) {
      // Only the CURRENT load owns the busy flag: a stale one finishing late
      // must not clear the newer load's.
      if (this.gen === gen) this.busy = false;
      if (this.gen !== gen || ctl.signal.aborted) return;
      const msg = e instanceof Error ? e.message : String(e);
      if (first && this.mode === 'restart') {
        this.commit({ phase: 'error', error: msg });
      } else {
        this.commit({ tail: 'failed', tailError: msg, tailExpired: e instanceof MatError && e.expired });
      }
      return;
    }
    if (this.gen !== gen) return;
    this.busy = false;

    const replace = first && this.mode === 'restart';
    const secs = replace ? [] : this.state.sections.slice();
    const have = new Set(secs.map(s => s.n));
    const keys = new Set(secs.map(s => `${s.repo}\n${s.path}`));
    let appended = 0;
    for (const f of page.facts) {
      if (!replace && f.n <= this.skipThrough) {
        const shown = secs.find(s => s.n === f.n);
        if (shown && (shown.path !== f.path || shown.repo !== f.repo)) {
          // The longer walk does not begin with what was read: start over.
          this.start('restart');
          return;
        }
        continue;
      }
      if (have.has(f.n) || keys.has(`${f.repo}\n${f.path}`)) continue;
      have.add(f.n);
      keys.add(`${f.repo}\n${f.path}`);
      secs.push(f);
      appended++;
    }
    secs.sort((a, b) => a.n - b.n);
    this.cursor = page.cursor;
    this.hasMore = page.has_more && page.cursor !== null;
    // Still reading past what was already on screen: keep going without
    // waiting for the reader, but only until a new section shows up.
    if (!replace && appended === 0 && this.hasMore && this.skipThrough > 0 && page.facts.length > 0) {
      this.commit({ env: envOf(page) });
      void this.pull(gen);
      return;
    }
    this.commit({
      sections: secs,
      env: envOf(page),
      phase: 'ready',
      error: null,
      stale: false,
      tail: this.hasMore ? 'idle' : 'done',
      tailError: null,
      tailExpired: false,
      epoch: replace ? this.state.epoch + 1 : this.state.epoch,
    });
  }
}

export function useMaterializeStream(api: MaterializeApi, target: MatTarget, options: MatOptions) {
  const [stream] = useState(() => new MaterializeStream());
  const state = useSyncExternalStore(stream.subscribe, stream.getState);
  // Re-run when the TARGET or any OPTION changes (api is the caller's seam and
  // is stable for a view's life). `sync` reads what changed itself.
  const tkey = targetKey(target);
  const okey = `${options.depth}|${options.references}|${options.crossKb}|${options.unpin}`;
  useEffect(() => {
    stream.sync({ api, target, options });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stream, tkey, okey]);
  useEffect(() => () => stream.dispose(), [stream]);
  return { ...state, loadMore: stream.loadMore, retry: stream.retry, restart: stream.restart };
}
