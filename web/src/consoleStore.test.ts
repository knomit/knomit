import { describe, it, expect } from 'vitest';
import { consoleReducer, consoleInit, isConsoleAction, CONSOLE_MAX_ENTRIES } from './consoleStore';
import type { ConsoleState } from './consoleStore';

// The console ring buffer, previously reduced inside AppState (state.ts) and
// characterized in App.sse.test.tsx. Same assertions, same cap, same trim
// direction — now against the store that actually owns it.

function log(n: number, from = 0, start: ConsoleState = consoleInit): ConsoleState {
  let s = start;
  for (let i = from; i < from + n; i++) {
    s = consoleReducer(s, { type: 'CONSOLE_LOG', level: 'info', message: `line-${String(i).padStart(4, '0')}` });
  }
  return s;
}

describe('consoleReducer — ring buffer', () => {
  it('keeps every entry below the cap, oldest first', () => {
    expect(log(3).entries.map(e => e.message)).toEqual(['line-0000', 'line-0001', 'line-0002']);
  });

  it('holds exactly 500 entries at the cap', () => {
    expect(log(CONSOLE_MAX_ENTRIES).entries).toHaveLength(500);
  });

  it('drops the OLDEST entries past the cap and keeps the newest', () => {
    const s = log(600);
    expect(s.entries).toHaveLength(500);
    // 0..99 evicted; 100..599 retained, still in emission order.
    expect(s.entries[0].message).toBe('line-0100');
    expect(s.entries[499].message).toBe('line-0599');
    expect(s.entries.some(e => e.message === 'line-0099')).toBe(false);
  });

  it('stamps each entry with a numeric id and a millisecond timestamp', () => {
    for (const e of log(3).entries) {
      expect(typeof e.id).toBe('number');
      expect(e.id).toBeGreaterThan(0);
      expect(e.time).toBeGreaterThan(0);
    }
  });

  it('preserves the level of each entry', () => {
    let s = consoleReducer(consoleInit, { type: 'CONSOLE_LOG', level: 'info', message: 'i' });
    s = consoleReducer(s, { type: 'CONSOLE_LOG', level: 'error', message: 'e' });
    expect(s.entries.map(e => e.level)).toEqual(['info', 'error']);
  });

  // REGRESSION. Ids were `Date.now() + Math.random()`. At Date.now() magnitude
  // (~1.7e12) a double's ulp is 2^-12, so at most ~4096 distinct fractional
  // values exist per millisecond and a burst inside one tick collides by the
  // birthday bound — the old scheme measured ~197 unique ids for 200 entries.
  // Console rows are keyed on the id, so each collision silently dropped a
  // rendered row, precisely during the SSE bursts this buffer exists to capture.
  it('mints a UNIQUE id for every entry in a burst logged within one tick', () => {
    const s = log(CONSOLE_MAX_ENTRIES);
    const ids = new Set(s.entries.map(e => e.id));
    expect(ids.size).toBe(s.entries.length);
    expect(s.entries.length).toBe(500);
    // Every entry in the burst shares a timestamp or two — i.e. this really is
    // the same-millisecond case the old id scheme could not survive.
    expect(new Set(s.entries.map(e => e.time)).size).toBeLessThan(s.entries.length);
  });

  it('ids stay unique and strictly increasing across separate dispatch batches', () => {
    const first = log(50);
    const second = log(50, 50, first);
    const ids = second.entries.map(e => e.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (let i = 1; i < ids.length; i++) expect(ids[i]).toBeGreaterThan(ids[i - 1]);
  });
});

describe('consoleReducer — panel state', () => {
  it('CONSOLE_TOGGLE flips open', () => {
    expect(consoleReducer(consoleInit, { type: 'CONSOLE_TOGGLE' }).open).toBe(true);
    const open = { ...consoleInit, open: true };
    expect(consoleReducer(open, { type: 'CONSOLE_TOGGLE' }).open).toBe(false);
  });

  it('CONSOLE_SET_HEIGHT clamps between 80 and 600', () => {
    expect(consoleReducer(consoleInit, { type: 'CONSOLE_SET_HEIGHT', height: 50 }).height).toBe(80);
    expect(consoleReducer(consoleInit, { type: 'CONSOLE_SET_HEIGHT', height: 300 }).height).toBe(300);
    expect(consoleReducer(consoleInit, { type: 'CONSOLE_SET_HEIGHT', height: 900 }).height).toBe(600);
  });
});

describe('isConsoleAction — dispatch routing', () => {
  it('claims exactly the three console actions', () => {
    expect(isConsoleAction({ type: 'CONSOLE_LOG' })).toBe(true);
    expect(isConsoleAction({ type: 'CONSOLE_TOGGLE' })).toBe(true);
    expect(isConsoleAction({ type: 'CONSOLE_SET_HEIGHT' })).toBe(true);
  });

  it('leaves app actions to the app reducer', () => {
    for (const type of ['SET_HEAD', 'APPLY_NAV', 'SET_CONTEXT', 'ADD_FILTER', 'SET_NOTICE']) {
      expect(isConsoleAction({ type })).toBe(false);
    }
  });
});
