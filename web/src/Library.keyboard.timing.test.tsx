import { describe, it, expect, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { api } from './api';
import { init } from './state';
import type { AppState } from './state';

// A LIST THAT IS ON SCREEN MUST ANSWER THE KEYBOARD.
//
// Library's window keydown handler closes over `activeList` and `selectedIdx`,
// and used to be registered by a passive effect. React runs the DOM commit and
// the passive-effect flush in SEPARATE tasks, so between them the rows were
// painted while the registered handler still closed over the EMPTY list it was
// built with. Arrows arriving in that gap hit `activeList.length === 0`,
// returned, and were silently swallowed: no selection, no NAVIGATE, no error.
//
// That gap is why Library.keyboard.test.tsx's "drives selection/navigation when
// live" failed roughly 1 full-suite run in 8 — its `waitFor` resolves on the
// row count, which is satisfied by the commit, and under load the test could
// resume inside the gap. It is also a real (if narrow) user-facing bug: press
// ArrowDown the instant the rows appear and the keypress does nothing.
//
// The fix is that the handler is registered in a LAYOUT effect, which runs
// synchronously in the commit task — so the handler can never describe a list
// older than the one on screen.
//
// This test drives the gap deliberately rather than waiting for load to find
// it. The browse promise is resolved OUTSIDE act(), so React schedules with its
// real scheduler; a MutationObserver — the same signal RTL's waitFor watches —
// fires the keys the instant the rows land, which is inside the commit task.
// Wrapping the resolution in act() would defeat it: act drives React's
// scheduler itself, collapsing the two tasks into one and hiding the gap.
let resolveBrowse: (v: unknown) => void;
vi.mock('./api', () => ({
  api: {
    browse: vi.fn(() => new Promise(res => { resolveBrowse = res as (v: unknown) => void; })),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    search: vi.fn().mockResolvedValue({ results: [] }),
  },
}));

const CHILDREN = {
  path: 'kb',
  children: [
    { name: 'sub', is_dir: true },
    { name: 'fact.md', is_dir: false, title: 'A Fact', type: 'observation', fullPath: 'kb/fact.md' },
  ],
};

describe('Library — the keyboard is live as soon as the rows are', () => {
  it('answers arrows pressed in the same task the rows are painted in', async () => {
    const dispatch = vi.fn();
    const state: AppState = {
      ...init, repo: 'knomit', branch: 'machine/test', headCommit: 'aaaaaaa',
      librarySort: 'path', asOf: { mode: 'live' },
    };
    render(<Library state={state} dispatch={dispatch} navigate={vi.fn()} />);

    const rowCount = () => document.querySelectorAll('[data-testid=dir-entry]').length;
    let rowsWhenFiring = 0;

    const fired = new Promise<void>(done => {
      const mo = new MutationObserver(() => {
        if (rowCount() === 0) return;
        mo.disconnect();
        rowsWhenFiring = rowCount();
        // ArrowDown selects the first row (a dir); ArrowRight activates it.
        fireEvent.keyDown(window, { key: 'ArrowDown' });
        fireEvent.keyDown(window, { key: 'ArrowRight' });
        done();
      });
      mo.observe(document.body, { childList: true, subtree: true });
    });

    // `resolveBrowse` is assigned only if api.browse actually ran, which needs
    // effectiveSort === 'path' — DERIVED state, not the librarySort the fixture
    // sets. Asserting the call first means a future change to `init` that trips
    // that reports "browse was never called" rather than a TypeError on the
    // line below, which would look like a bug in this test.
    expect(api.browse).toHaveBeenCalled();
    resolveBrowse(CHILDREN);
    await fired;

    // Guards the guard: if the observer ever stopped seeing rows, the assertion
    // below would pass or fail for a reason that has nothing to do with timing.
    expect(rowsWhenFiring).toBe(2);
    // The PATH matters, not just that some NAVIGATE fired: Library has three
    // NAVIGATE producers (enterDir, activateSelected, jumpAncestor), so a
    // regression that selects the wrong row or enters the wrong directory would
    // satisfy a bare type check. 'kb/sub' is the first row — the dir — which is
    // what ArrowDown then ArrowRight is supposed to reach.
    expect(dispatch).toHaveBeenCalledWith({ type: 'NAVIGATE', path: 'kb/sub' });
  });
});
