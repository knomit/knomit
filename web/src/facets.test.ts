import { describe, it, expect, beforeEach } from 'vitest';
import { initView, viewReducer } from './facets';
import { RAIL_STORAGE_KEY, loadRailOpen, saveRailOpen } from './repoSelection';

// The view state is UI chrome, not a way of looking at facts, so it lives
// outside AppState and outside NavEntry (plan section 0). In this PR it holds
// only whether the target rail is open.
describe('the rail flag', () => {
  beforeEach(() => { localStorage.clear(); });

  it('starts open when knomit.rail is "1"', () => {
    localStorage.setItem('knomit.rail', '1');
    expect(initView().railOpen).toBe(true);
  });

  it('starts collapsed when nothing is stored', () => {
    expect(localStorage.getItem('knomit.rail')).toBeNull();
    expect(initView().railOpen).toBe(false);
  });

  it('starts collapsed on a value it does not recognise', () => {
    localStorage.setItem('knomit.rail', 'garbage');
    expect(initView().railOpen).toBe(false);
  });

  it('TOGGLE_RAIL flips it, both ways', () => {
    const closed = { ...initView(), railOpen: false };
    const opened = viewReducer(closed, { type: 'TOGGLE_RAIL' });
    expect(opened.railOpen).toBe(true);
    expect(viewReducer(opened, { type: 'TOGGLE_RAIL' }).railOpen).toBe(false);
  });

  it('SET_RAIL sets the value it is given', () => {
    const closed = { ...initView(), railOpen: false };
    expect(viewReducer(closed, { type: 'SET_RAIL', open: true }).railOpen).toBe(true);
    const open = { ...initView(), railOpen: true };
    expect(viewReducer(open, { type: 'SET_RAIL', open: false }).railOpen).toBe(false);
  });

  it('persists under knomit.rail as "1" / "0" and reads back what it wrote', () => {
    expect(RAIL_STORAGE_KEY).toBe('knomit.rail');
    saveRailOpen(true);
    expect(localStorage.getItem('knomit.rail')).toBe('1');
    expect(loadRailOpen()).toBe(true);
    saveRailOpen(false);
    expect(localStorage.getItem('knomit.rail')).toBe('0');
    expect(loadRailOpen()).toBe(false);
  });
});
