import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { initView, viewReducer, facetsFor, useViewState, draftCancellable, displayedTarget, targetLabelFor, serverPagesFor } from './facets';
import type { ViewState } from './facets';
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

// ── Pinned targets and drafts (PR E) ─────────────────────────────────────────
// A target with no store to read (archived, a create not yet registered, a
// registered repo with no store) is PINNED over the browse context rather than
// becoming it. A draft is a target too: the create form, with no tabs.
describe('pinned targets and the facet rule', () => {
  beforeEach(() => { localStorage.clear(); });
  const ALL = ['browse', 'activity', 'settings'] as const;
  const onActivity = (): ViewState => ({ ...initView(), facet: 'activity' });

  it('pinning an archived target from Activity lands on settings and remembers activity; the next repo restores it', () => {
    const pinned = viewReducer(onActivity(), { type: 'SELECT_PINNED', target: { kind: 'archived', id: 'id-q3' } });
    expect(pinned.pinned).toEqual({ kind: 'archived', id: 'id-q3' });
    expect(pinned.facet).toBe('settings');
    expect(pinned.lastFacet).toBe('activity');
    const repoB = viewReducer(pinned, { type: 'SELECT_CONTEXT_TARGET', available: [...ALL] });
    expect(repoB.pinned).toBeNull();
    expect(repoB.facet).toBe('activity');
    expect(repoB.lastFacet).toBeNull();
  });

  it('a SET_FACET while pinned clears lastFacet, so the next normal target keeps the facet just chosen', () => {
    const pinned = viewReducer(onActivity(), { type: 'SELECT_PINNED', target: { kind: 'unavailable', repo: 'vendor-docs' } });
    expect(pinned.lastFacet).toBe('activity');
    const chose = viewReducer(pinned, { type: 'SET_FACET', facet: 'browse' });
    expect(chose.lastFacet).toBeNull();
    const next = viewReducer(chose, { type: 'SELECT_CONTEXT_TARGET', available: [...ALL] });
    expect(next.facet).toBe('browse');
  });

  it('facetsFor gives every pinned target settings alone, and a draft no facets', () => {
    expect(facetsFor({ kind: 'archived', id: 'x' }, [], [], [])).toEqual(['settings']);
    expect(facetsFor({ kind: 'pending', createId: 'c1' }, [], [], [])).toEqual(['settings']);
    expect(facetsFor({ kind: 'unavailable', repo: 'gone' }, [], [], [])).toEqual(['settings']);
    expect(facetsFor({ kind: 'brokenLens', name: 'bad' }, [], [], [])).toEqual(['settings']);
    expect(facetsFor({ kind: 'draft', draft: 'repo' }, [], [], [])).toEqual([]);
  });
});

describe('drafts', () => {
  beforeEach(() => { localStorage.clear(); });
  const ops = { kind: 'repo', repo: 'ops-kb' } as const;

  it('OPEN_DRAFT records returnTo with the exact context, pinned and facet; CANCEL_DRAFT restores those three', () => {
    const start: ViewState = { ...initView(), facet: 'settings', lastFacet: 'activity', pinned: { kind: 'archived', id: 'id-q3' } };
    const open = viewReducer(start, { type: 'OPEN_DRAFT', kind: 'repo', from: ops });
    expect(open.draft).toBe('repo');
    expect(open.draftShown).toBe(true);
    expect(open.returnTo).toEqual({ context: ops, pinned: { kind: 'archived', id: 'id-q3' }, facet: 'settings' });
    const cancelled = viewReducer(open, { type: 'CANCEL_DRAFT' });
    expect(cancelled.draft).toBeNull();
    expect(cancelled.draftShown).toBe(false);
    expect(cancelled.returnTo).toBeNull();
    expect(cancelled.pinned).toEqual({ kind: 'archived', id: 'id-q3' });
    expect(cancelled.facet).toBe('settings');
  });

  it('picking another target hides the draft but keeps it; showing it again records where the reader came from', () => {
    const open = viewReducer({ ...initView(), facet: 'browse' }, { type: 'OPEN_DRAFT', kind: 'repo', from: ops });
    const away = viewReducer(open, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'] });
    expect(away.draft).toBe('repo');
    expect(away.draftShown).toBe(false);
    const core = { kind: 'repo', repo: 'core-kb' } as const;
    const back = viewReducer({ ...away, facet: 'activity' }, { type: 'SHOW_DRAFT', from: core });
    expect(back.draftShown).toBe(true);
    expect(back.returnTo).toEqual({ context: core, pinned: null, facet: 'activity' });
  });

  it('E3: DRAFT_REGISTERED lands on settings, remembers the facet, and a job reaching done changes nothing', () => {
    const open = viewReducer({ ...initView(), facet: 'browse' }, { type: 'OPEN_DRAFT', kind: 'repo', from: ops });
    const landed = viewReducer(open, { type: 'DRAFT_REGISTERED', kind: 'repo', facet: 'settings' });
    expect(landed.draft).toBeNull();
    expect(landed.draftShown).toBe(false);
    expect(landed.returnTo).toBeNull();
    expect(landed.facet).toBe('settings');
    expect(landed.lastFacet).toBe('browse');
    // The job reaching done is not a view action: there is nothing to dispatch,
    // so the facet stays where it is. The shown facet is view.facet as long as
    // the target has it, which a done repo does.
    expect(facetsFor({ kind: 'repo', repo: 'ops-new' }, [{ name: 'ops-new', uid: 'u-new' }], [], [])).toContain(landed.facet);
    expect(landed.facet).toBe('settings');
  });

  it('a lens draft registered lands on browse and forgets the remembered facet', () => {
    const open = viewReducer({ ...initView(), facet: 'settings', lastFacet: 'activity' }, { type: 'OPEN_DRAFT', kind: 'lens', from: ops });
    const landed = viewReducer(open, { type: 'DRAFT_REGISTERED', kind: 'lens', facet: 'browse' });
    expect(landed.facet).toBe('browse');
    expect(landed.lastFacet).toBeNull();
  });

  it('a draft that registers while HIDDEN is dropped without moving the reader', () => {
    const open = viewReducer({ ...initView(), facet: 'browse' }, { type: 'OPEN_DRAFT', kind: 'repo', from: ops });
    const away = viewReducer({ ...open, facet: 'activity' }, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'] });
    const done = viewReducer(away, { type: 'DRAFT_REGISTERED', kind: 'repo', facet: 'settings' });
    expect(done.draft).toBeNull();
    expect(done.facet).toBe('activity');
    expect(done.lastFacet).toBeNull();
  });

  // The repo wizard's create keeps polling after the lens draft takes the one
  // slot, so its result can arrive for a draft that is not there any more.
  it('a registration for the OTHER kind of draft leaves the draft on screen alone', () => {
    const repoOpen = viewReducer({ ...initView(), facet: 'browse' }, { type: 'OPEN_DRAFT', kind: 'repo', from: ops });
    const lensOpen = viewReducer(repoOpen, { type: 'OPEN_DRAFT', kind: 'lens', from: ops });
    const late = viewReducer(lensOpen, { type: 'DRAFT_REGISTERED', kind: 'repo', facet: 'settings' });
    expect(late).toBe(lensOpen);
    expect(late.draft).toBe('lens');
    expect(late.draftShown).toBe(true);
    // Control: the lens draft's own registration does retire it.
    expect(viewReducer(lensOpen, { type: 'DRAFT_REGISTERED', kind: 'lens', facet: 'browse' }).draft).toBeNull();
  });

  it('with no repos the repo draft cannot be cancelled; with one it can, and a lens draft always can', () => {
    expect(draftCancellable('repo', [])).toBe(false);
    expect(draftCancellable('repo', [{ name: 'a', uid: 'u-a' }])).toBe(true);
    expect(draftCancellable('lens', [])).toBe(true);
  });

  it('displayedTarget: a shown draft, then a pin, then the context; with no repos the repo draft is the fallback', () => {
    const base = initView();
    expect(displayedTarget(base, ops, false)).toEqual(ops);
    expect(displayedTarget(base, ops, true)).toEqual({ kind: 'draft', draft: 'repo' });
    const pinned = { ...base, pinned: { kind: 'archived' as const, id: 'id-q3' } };
    expect(displayedTarget(pinned, ops, true)).toEqual({ kind: 'archived', id: 'id-q3' });
    const draft = { ...pinned, draft: 'lens' as const, draftShown: true };
    expect(displayedTarget(draft, ops, false)).toEqual({ kind: 'draft', draft: 'lens' });
  });
});

// The top bar's label: a target with only Settings says why with a chip.
describe('targetLabelFor', () => {
  const repos = [{ name: 'ops-kb', uid: 'u-ops' }, { name: 'vendor-docs', uid: 'u-v', state: 'missing' }, { name: 'odd', uid: 'u-o', state: 'conflict' }] as RepoInfo[];
  const archived = [{ id: 'id-q3', name: 'q3-planning', origin: '', archivedAt: '2026-08-12T10:00:00Z' }];
  const creates = [
    { create_id: 'c1', name: 'b-kb', state: 'running' },
    { create_id: 'c2', name: 'x-kb', state: 'failed' },
    { create_id: 'c3', name: 'ops-kb', state: 'cancelling' },
  ] as RepoCreateStatus[];

  it('chips an archived target "archived" in the archive tone', () => {
    expect(targetLabelFor({ kind: 'archived', id: 'id-q3' }, repos, archived, [])).toEqual(
      { kind: 'repo', name: 'q3-planning', chip: { text: 'archived', tone: 'archived' } });
  });

  it('chips a repo with no store by its state, "store missing" for missing', () => {
    expect(targetLabelFor({ kind: 'unavailable', repo: 'vendor-docs' }, repos, [], []).chip).toEqual({ text: 'store missing', tone: 'bad' });
    expect(targetLabelFor({ kind: 'unavailable', repo: 'odd' }, repos, [], []).chip).toEqual({ text: 'conflict', tone: 'bad' });
  });

  it('chips a create by its flag: creating, or failed in the bad tone', () => {
    expect(targetLabelFor({ kind: 'pending', createId: 'c1' }, repos, [], creates)).toEqual(
      { kind: 'repo', name: 'b-kb', chip: { text: 'creating', tone: 'creating' } });
    expect(targetLabelFor({ kind: 'pending', createId: 'c2' }, repos, [], creates).chip).toEqual({ text: 'failed', tone: 'bad' });
  });

  it('chips a registered repo whose create is still working, and leaves a plain repo bare', () => {
    expect(targetLabelFor({ kind: 'repo', repo: 'ops-kb' }, repos, [], creates).chip).toEqual({ text: 'cancelling', tone: 'creating' });
    expect(targetLabelFor({ kind: 'repo', repo: 'ops-kb' }, repos, [], [])).toEqual({ kind: 'repo', name: 'ops-kb' });
  });

  it('chips a pinned broken lens "mount missing" in the bad tone', () => {
    expect(targetLabelFor({ kind: 'brokenLens', name: 'haunted' }, repos, [], [])).toEqual(
      { kind: 'lens', name: 'haunted', chip: { text: 'mount missing', tone: 'bad' } });
  });

  it('labels a draft by what it will create', () => {
    expect(targetLabelFor({ kind: 'draft', draft: 'repo' }, repos, [], [])).toEqual({ kind: 'repo', name: 'New repository', draft: true });
    expect(targetLabelFor({ kind: 'draft', draft: 'lens' }, repos, [], [])).toEqual({ kind: 'lens', name: 'New lens', draft: true });
  });
});

// The server pages sit OVER the displayed target and leave it alone, so
// closing them is the way back to the target and facet that were on screen.
describe('the server pages', () => {
  const ops = { kind: 'repo' as const, repo: 'ops-kb' };
  const onActivity: ViewState = { ...initView(), facet: 'activity', lastFacet: null, server: null };

  it('the gear opens them on Fleet and closes them, leaving the facet, the pin and the draft untouched', () => {
    const start: ViewState = { ...onActivity, pinned: { kind: 'archived', id: 'a1' }, draft: 'lens', draftShown: false };
    const open = viewReducer(start, { type: 'TOGGLE_SERVER' });
    expect(open.server).toBe('fleet');
    const closed = viewReducer({ ...open, server: 'logs' }, { type: 'TOGGLE_SERVER' });
    expect(closed).toEqual({ ...start, server: null });
  });

  it('a tab click moves between pages', () => {
    const open = viewReducer(onActivity, { type: 'TOGGLE_SERVER' });
    expect(viewReducer(open, { type: 'SET_SERVER_PAGE', page: 'auth' }).server).toBe('auth');
  });

  it('every way of picking a target leaves them', () => {
    const on: ViewState = { ...onActivity, server: 'logs' };
    expect(viewReducer(on, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'] }).server).toBeNull();
    expect(viewReducer(on, { type: 'SELECT_PINNED', target: { kind: 'archived', id: 'a1' } }).server).toBeNull();
    expect(viewReducer(on, { type: 'OPEN_DRAFT', kind: 'repo', from: ops }).server).toBeNull();
    // The draft row already on screen under them.
    const underDraft: ViewState = { ...on, draft: 'repo', draftShown: true };
    expect(viewReducer(underDraft, { type: 'SHOW_DRAFT', from: ops }).server).toBeNull();
    expect(viewReducer(underDraft, { type: 'OPEN_DRAFT', kind: 'repo', from: ops }).server).toBeNull();
    // A facet route (Reconnect) lands on the target's facet.
    expect(viewReducer(on, { type: 'SET_FACET', facet: 'settings', focus: 'remote' }).server).toBeNull();
  });

  it('picking the target that is already under them keeps its facet', () => {
    const on: ViewState = { ...onActivity, server: 'fleet' };
    const back = viewReducer(on, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'] });
    expect(back).toEqual({ ...onActivity, server: null });
  });

  // The app's own corrections (a pin that vanished or healed while the server
  // pages were open) fix the target underneath without taking the reader off
  // the page they chose. Control: the same actions from a pick do leave.
  it('an automatic unpin or heal under them keeps them open, and still fixes the target underneath', () => {
    const on: ViewState = { ...onActivity, server: 'logs', pinned: { kind: 'brokenLens', name: 'haunted' }, facet: 'settings', lastFacet: 'activity' };
    const healed = viewReducer(
      viewReducer(on, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'], keepServer: true }),
      { type: 'SET_FACET', facet: 'settings', keepServer: true },
    );
    expect(healed.server).toBe('logs');
    expect(healed.pinned).toBeNull();
    expect(healed.facet).toBe('settings');
    const gone = viewReducer(on, { type: 'SELECT_CONTEXT_TARGET', available: ['browse', 'activity', 'settings'], keepServer: true });
    expect(gone.server).toBe('logs');
    expect(gone.pinned).toBeNull();
    expect(gone.facet).toBe('activity');
    // Control: without the flag the same actions leave them.
    expect(viewReducer(on, { type: 'SELECT_CONTEXT_TARGET', available: ['settings'] }).server).toBeNull();
    expect(viewReducer(on, { type: 'SET_FACET', facet: 'settings' }).server).toBeNull();
  });

  it('a reload never lands on them', () => {
    expect(initView().server).toBeNull();
  });

  it('serverPagesFor omits Logs and Authorizations on a read-only server, and Authorizations without an issuer', () => {
    expect(serverPagesFor(false, true)).toEqual(['fleet', 'logs', 'auth']);
    expect(serverPagesFor(false, false)).toEqual(['fleet', 'logs']);
    expect(serverPagesFor(true, true)).toEqual(['fleet']);
    expect(serverPagesFor(true, false)).toEqual(['fleet']);
  });
});
