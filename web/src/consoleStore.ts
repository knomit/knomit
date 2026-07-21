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
  | { type: 'CONSOLE_LOG'; level: 'info' | 'error'; message: string }
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
}

export const consoleInit: ConsoleState = {
  entries: [],
  open: false,
  height: 200,
};

export const CONSOLE_MAX_ENTRIES = 500;

// Monotonic entry id. Ids were `Date.now() + Math.random()`: at Date.now()
// magnitude (~1.7e12) a double's ulp is 2^-12, so only ~4096 distinct
// fractional values exist per millisecond and a burst of a few hundred entries
// in one tick collides by the birthday bound (measured: ~197 unique out of
// 200). Console rows are keyed on the id, so a collision silently dropped a
// rendered row — exactly during the SSE bursts this buffer exists to capture.
// A counter is unique by construction.
let nextEntryId = 0;

export function consoleReducer(s: ConsoleState, a: ConsoleAction): ConsoleState {
  switch (a.type) {
    case 'CONSOLE_LOG': {
      const entry: ConsoleEntry = { id: ++nextEntryId, time: Date.now(), level: a.level, message: a.message };
      const entries = [...s.entries, entry];
      if (entries.length > CONSOLE_MAX_ENTRIES) entries.splice(0, entries.length - CONSOLE_MAX_ENTRIES);
      return { ...s, entries };
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
