import { describe, it, expect } from 'vitest';
import { init, reducer } from './state';

describe('commitDrawer state', () => {
  it('defaults to closed in init', () => {
    expect(init.commitDrawer).toEqual({ open: false });
  });

  it('OPEN_COMMIT_DRAWER opens with the given commit', () => {
    const next = reducer(init, { type: 'OPEN_COMMIT_DRAWER', commit: 'a1b2c3d' });
    expect(next.commitDrawer).toEqual({ open: true, commit: 'a1b2c3d' });
  });

  it('CLOSE_COMMIT_DRAWER closes the drawer', () => {
    const opened = reducer(init, { type: 'OPEN_COMMIT_DRAWER', commit: 'a1b2c3d' });
    const next = reducer(opened, { type: 'CLOSE_COMMIT_DRAWER' });
    expect(next.commitDrawer).toEqual({ open: false });
  });

  it('opening the drawer does not change asOf', () => {
    const scrubbed = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'aaaaaaa' } };
    const next = reducer(scrubbed, { type: 'OPEN_COMMIT_DRAWER', commit: 'bbbbbbb' });
    expect(next.asOf).toEqual({ mode: 'scrubbed', commit: 'aaaaaaa' });
  });

  it('opening the drawer does not push navStack', () => {
    const before = init.navStack;
    const next = reducer(init, { type: 'OPEN_COMMIT_DRAWER', commit: 'a1b2c3d' });
    expect(next.navStack).toBe(before);
  });
});
