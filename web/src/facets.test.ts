import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { initView, viewReducer, facetsFor, useViewState } from './facets';
import { RAIL_STORAGE_KEY, loadRailOpen, saveRailOpen } from './repoSelection';
import type { RepoInfo, Lens, RepoCreateStatus } from './api';

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

// ── The facet (PR D) ─────────────────────────────────────────────────────────
// Which page of the current target is on screen. Persisted under knomit.facet
// beside knomit.context, and like the rail it is NOT on AppState.
describe('the facet', () => {
  beforeEach(() => { localStorage.clear(); });

  it('SET_FACET("activity") persists "activity" under knomit.facet', () => {
    const { result } = renderHook(() => useViewState());
    expect(result.current[0].facet).toBe('browse');
    act(() => { result.current[1]({ type: 'SET_FACET', facet: 'activity' }); });
    expect(result.current[0].facet).toBe('activity');
    expect(localStorage.getItem('knomit.facet')).toBe('activity');
  });

  it('initView starts on the stored facet, and on browse for a value it does not know', () => {
    localStorage.setItem('knomit.facet', 'settings');
    expect(initView().facet).toBe('settings');
    localStorage.setItem('knomit.facet', 'nope');
    expect(initView().facet).toBe('browse');
  });

  it('SET_FACET clears lastFacet', () => {
    const s = { ...initView(), facet: 'settings' as const, lastFacet: 'activity' as const };
    const next = viewReducer(s, { type: 'SET_FACET', facet: 'browse' });
    expect(next.facet).toBe('browse');
    expect(next.lastFacet).toBeNull();
  });

  it('SET_FACET carries a one-shot focus, and CLEAR_FOCUS drops it', () => {
    const s = viewReducer(initView(), { type: 'SET_FACET', facet: 'settings', focus: 'remote' });
    expect(s.focus).toBe('remote');
    expect(viewReducer(s, { type: 'CLEAR_FOCUS' }).focus).toBeNull();
  });

  it('selecting a target that lacks the facet lands on settings and remembers it; the next target that has it restores it', () => {
    const onActivity = { ...initView(), facet: 'activity' as const };
    const away = viewReducer(onActivity, { type: 'SELECT_CONTEXT_TARGET', available: ['settings'] });
    expect(away.facet).toBe('settings');
    expect(away.lastFacet).toBe('activity');
    const back = viewReducer(away, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'] });
    expect(back.facet).toBe('activity');
    expect(back.lastFacet).toBeNull();
  });

  it('selecting a target that has the facet keeps it (control for the rule above)', () => {
    const onActivity = { ...initView(), facet: 'activity' as const };
    const next = viewReducer(onActivity, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'] });
    expect(next.facet).toBe('activity');
    expect(next.lastFacet).toBeNull();
  });

  it('SET_LOCKED sets the connect lock', () => {
    expect(viewReducer(initView(), { type: 'SET_LOCKED', locked: true }).locked).toBe(true);
    expect(viewReducer({ ...initView(), locked: true }, { type: 'SET_LOCKED', locked: false }).locked).toBe(false);
  });
});

describe('facetsFor', () => {
  const repos = [
    { name: 'ops-kb', uid: 'u-ops' },
    { name: 'core-kb', uid: 'u-core' },
    { name: 'gone', uid: 'u-gone', state: 'missing' },
  ] as RepoInfo[];
  const healthy = { name: 'dev', uid: 'l-dev', write: { uid: 'u-ops', name: 'ops-kb' }, reads: [{ uid: 'u-core', name: 'core-kb' }] } as Lens;
  const broken = { name: 'bad', uid: 'l-bad', write: { uid: 'u-ops', name: 'ops-kb' }, reads: [{ uid: 'u-gone', name: 'gone' }] } as Lens;
  const lenses = [healthy, broken];
  const creating = [{ create_id: 'c1', name: 'core-kb', state: 'running' } as RepoCreateStatus];

  it('gives an available repo browse, activity and settings', () => {
    expect(facetsFor({ kind: 'repo', repo: 'ops-kb' }, repos, lenses, [])).toEqual(['browse', 'activity', 'settings']);
  });

  it('gives a repo with an active create only settings', () => {
    expect(facetsFor({ kind: 'repo', repo: 'core-kb' }, repos, lenses, creating)).toEqual(['settings']);
  });

  it('gives a repo whose create is running but which is not listed yet only settings', () => {
    const unlisted = [{ create_id: 'c2', name: 'new-kb', state: 'running' } as RepoCreateStatus];
    expect(facetsFor({ kind: 'repo', repo: 'new-kb' }, repos, lenses, unlisted)).toEqual(['settings']);
    // Control: unlisted with no create gets all three (the list lags by a refresh).
    expect(facetsFor({ kind: 'repo', repo: 'new-kb' }, repos, lenses, [])).toEqual(['browse', 'activity', 'settings']);
  });

  it('gives a lens with all mounts readable all three', () => {
    expect(facetsFor({ kind: 'lens', name: 'dev' }, repos, lenses, [])).toEqual(['browse', 'activity', 'settings']);
  });

  it('gives a lens with a broken mount activity and settings', () => {
    expect(facetsFor({ kind: 'lens', name: 'bad' }, repos, lenses, [])).toEqual(['activity', 'settings']);
  });
});
