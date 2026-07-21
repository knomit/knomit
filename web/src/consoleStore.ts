import { createContext, useContext } from 'react';
import type { Dispatch } from 'react';

// The console panel owns its own store, deliberately OUTSIDE AppState. Every
// SSE task/status/remote event writes a console line, and while the 500-entry
// ring buffer lived in AppState each of those lines produced a fresh AppState
// object — re-rendering the whole app (Library rows and all) once per log line
// during a burst. Splitting it out means a log line re-renders the Console and
// nothing else.
//
// consoleOpen/consoleHeight moved with the entries rather than staying behind:
// nothing persists them (the only localStorage keys are the repo/context
// selection and the splitter width), so there was no persistence tie to
// AppState, and leaving them would have made the Console read one panel's state
// from two stores while AppState kept a vestigial console surface. One panel,
// one owner.

export interface ConsoleEntry {
  id: number;
  time: number; // Date.now()
  level: 'info' | 'error';
  message: string;
}

// Console actions stay part of the app-wide Action union (state.ts re-exports
// them) so every existing call site keeps dispatching through the single
// `dispatch` it already holds — App fans console actions out to this reducer.
export type ConsoleAction =
  // `time` is stamped at DISPATCH time by stampConsoleAction, never read off the
  // clock inside the reducer — see the purity note on consoleReducer. Call sites
  // omit it; the store's dispatch wrapper fills it in.
  | { type: 'CONSOLE_LOG'; level: 'info' | 'error'; message: string; time?: number }
  | { type: 'CONSOLE_TOGGLE' }
  | { type: 'CONSOLE_SET_HEIGHT'; height: number };

const CONSOLE_ACTION_TYPES = new Set<string>(['CONSOLE_LOG', 'CONSOLE_TOGGLE', 'CONSOLE_SET_HEIGHT']);

// isConsoleAction routes a dispatched action to the right store. Narrowing on
// the type tag keeps the fan-out in App a one-liner.
export function isConsoleAction(a: { type: string }): a is ConsoleAction {
  return CONSOLE_ACTION_TYPES.has(a.type);
}

export interface ConsoleState {
  entries: ConsoleEntry[];
  open: boolean;
  height: number;
  // Monotonic entry id, carried IN STATE rather than in a module counter. Ids
  // were `Date.now() + Math.random()`: at Date.now() magnitude (~1.7e12) a
  // double's ulp is 2^-12, so only ~4096 distinct fractional values exist per
  // millisecond and a burst of a few hundred entries in one tick collides by
  // the birthday bound (measured: ~197 unique out of 200). Console rows are
  // keyed on the id, so a collision silently dropped a rendered row — exactly
  // during the SSE bursts this buffer exists to capture. A counter is unique by
  // construction; keeping it in state also keeps it REPLAYABLE (see below).
  nextId: number;
}

export const consoleInit: ConsoleState = {
  entries: [],
  open: false,
  height: 200,
  nextId: 1,
};

export const CONSOLE_MAX_ENTRIES = 500;

// stampConsoleAction fills in the impure inputs a log line needs, at the
// DISPATCH boundary. App wraps the store's dispatch in this, so every call site
// keeps dispatching `{ type: 'CONSOLE_LOG', level, message }` unchanged.
export function stampConsoleAction(a: ConsoleAction): ConsoleAction {
  return a.type === 'CONSOLE_LOG' && a.time === undefined ? { ...a, time: Date.now() } : a;
}

// consoleReducer is a PURE function of (state, action) — no clock read, no
// module counter. That is a correctness requirement, not hygiene: main.tsx
// mounts under StrictMode on a concurrent root, and React re-runs pending
// reducer updates when a render is discarded or rebased behind a
// higher-priority one. A reducer that minted `++moduleCounter` handed an
// ALREADY-RENDERED entry a different id on the replay, and since rows are keyed
// on the id the visible list unmounted and remounted — losing scroll position
// mid-burst, the exact symptom the console store exists to avoid. Ids now come
// off state.nextId and the timestamp off the action, so a replay reproduces the
// same entry byte for byte.
export function consoleReducer(s: ConsoleState, a: ConsoleAction): ConsoleState {
  switch (a.type) {
    case 'CONSOLE_LOG': {
      const entry: ConsoleEntry = { id: s.nextId, time: a.time ?? 0, level: a.level, message: a.message };
      const entries = [...s.entries, entry];
      if (entries.length > CONSOLE_MAX_ENTRIES) entries.splice(0, entries.length - CONSOLE_MAX_ENTRIES);
      return { ...s, entries, nextId: s.nextId + 1 };
    }
    case 'CONSOLE_TOGGLE':
      return { ...s, open: !s.open };
    case 'CONSOLE_SET_HEIGHT':
      return { ...s, height: Math.max(80, Math.min(a.height, 600)) };
    default:
      return s;
  }
}

// Two contexts, not one: the state changes on every log line, the dispatch
// never does. Splitting them means a producer (App) can hold the dispatch
// without subscribing to the entries it writes.
export const ConsoleStateContext = createContext<ConsoleState>(consoleInit);
export const ConsoleDispatchContext = createContext<Dispatch<ConsoleAction>>(() => {});

export function useConsoleState(): ConsoleState {
  return useContext(ConsoleStateContext);
}

export function useConsoleDispatch(): Dispatch<ConsoleAction> {
  return useContext(ConsoleDispatchContext);
}
