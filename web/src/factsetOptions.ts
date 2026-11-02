// The FactSet view's open state and options: in the page's query string (no
// router, a relative asset base: query string only, replaceState, gotcha
// a11a0b03), under their own keys so they never collide with the single-fact
// view's `materialize=1` / mat_* keys (Q-UI6). The URL carries no FactSet id
// or cursor: the search is the results list's, frozen when the view opens, and
// the FactSet is re-created from it.
//
// The Library opens the view and the right panel shows it; they share this
// small store (state.ts is unchanged).

import { useSyncExternalStore } from 'react';
import { DEFAULT_OPTIONS } from './materializeApi';
import type { MatOptions } from './materializeApi';
import { clampDepth, clampReferences, sameOptions } from './materializeOptions';
import type { FsSearch } from './factsetApi';

const P_OPEN = 'factset';
const P_DEPTH = 'fs_depth';
const P_REFS = 'fs_refs';
const P_XKB = 'fs_cross_kb';
const P_UNPIN = 'fs_unpin';
const KEYS = [P_OPEN, P_DEPTH, P_REFS, P_XKB, P_UNPIN];

export interface FsUrlState { open: boolean; options: MatOptions }

export function parseFactSetParams(search: string): FsUrlState {
  const p = new URLSearchParams(search);
  const num = (k: string, d: number, clamp: (n: number) => number) => {
    const raw = p.get(k);
    return raw === null || raw.trim() === '' ? d : clamp(Number(raw));
  };
  return {
    open: p.get(P_OPEN) === '1',
    options: {
      depth: num(P_DEPTH, DEFAULT_OPTIONS.depth, clampDepth),
      references: num(P_REFS, DEFAULT_OPTIONS.references, clampReferences),
      crossKb: p.get(P_XKB) === '1',
      unpin: p.get(P_UNPIN) === '1',
    },
  };
}

/** The query string (with its '?', or '') carrying s, keeping every parameter
 *  that is not ours. Only values off the defaults are written. */
export function writeFactSetParams(search: string, s: FsUrlState): string {
  const p = new URLSearchParams(search);
  for (const k of KEYS) p.delete(k);
  if (s.open) {
    p.set(P_OPEN, '1');
    if (s.options.depth !== DEFAULT_OPTIONS.depth) p.set(P_DEPTH, String(s.options.depth));
    if (s.options.references !== DEFAULT_OPTIONS.references) p.set(P_REFS, String(s.options.references));
    if (s.options.crossKb) p.set(P_XKB, '1');
    if (s.options.unpin) p.set(P_UNPIN, '1');
  }
  const q = p.toString();
  return q ? `?${q}` : '';
}

/** What the right panel shows: the FactSet of `search` at `options`, or
 *  nothing (search null). */
export interface FsView { search: FsSearch | null; options: MatOptions }

type Listener = () => void;

/** The shared FactSet view state: the frozen search (memory only) plus the
 *  open flag and options (mirrored into the query string). */
class FactSetViewStore {
  private view: FsView;
  private listeners = new Set<Listener>();

  constructor() {
    const url = parseFactSetParams(typeof window !== 'undefined' ? window.location.search : '');
    // A reload has no list search to re-create the FactSet from (the list's
    // state is not in the URL), so a stale open flag is cleared on first write.
    this.view = { search: null, options: url.options };
  }

  subscribe = (l: Listener): (() => void) => {
    this.listeners.add(l);
    return () => { this.listeners.delete(l); };
  };
  get = (): FsView => this.view;

  private set(v: FsView): void {
    this.view = v;
    if (typeof window !== 'undefined') {
      const q = writeFactSetParams(window.location.search, { open: v.search !== null, options: v.options });
      if (q !== window.location.search) {
        window.history.replaceState(window.history.state, '', window.location.pathname + q + window.location.hash);
      }
    }
    this.listeners.forEach(l => l());
  }

  /** Opens the FactSet of the list's current search (frozen from here on). */
  open(search: FsSearch): void { this.set({ ...this.view, search }); }
  close(): void { if (this.view.search) this.set({ ...this.view, search: null }); }
  setOptions(options: MatOptions): void {
    if (!sameOptions(options, this.view.options)) this.set({ ...this.view, options });
  }
}

export const factsetView = new FactSetViewStore();

export function useFactSetView(): FsView {
  return useSyncExternalStore(factsetView.subscribe, factsetView.get);
}
