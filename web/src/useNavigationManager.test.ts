import { describe, it, expect, vi, beforeEach } from 'vitest';
import type { Dispatch } from 'react';
import type { Action, AppState } from './state';
import { init } from './state';
import { resolveNavRequest } from './useNavigationManager';
import type { NavRequest } from './useNavigationManager';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    commitDetail: vi.fn(),
  },
}));

const mockCommitDetail = api.commitDetail as ReturnType<typeof vi.fn>;

function makeState(overrides: Partial<AppState> = {}): AppState {
  return { ...init, repo: 'myrepo', headCommit: 'head1', ...overrides };
}

describe('resolveNavRequest', () => {
  let dispatch: Dispatch<Action>;

  beforeEach(() => {
    dispatch = vi.fn() as unknown as Dispatch<Action>;
    vi.clearAllMocks();
  });

  // ── Explicit history navigation ────────────────────────────────────────────

  it('fully-specified history request dispatches synchronously without API call', async () => {
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: 'kb/foo.md', factCommit: 'abc123' };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledOnce();
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: 'abc123', factPath: 'kb/foo.md', factCommit: 'abc123' });
  });

  it('fully-specified history request without factCommit defaults factCommit to historyCommit', async () => {
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: 'kb/foo.md' };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: 'abc123', factPath: 'kb/foo.md', factCommit: 'abc123' });
  });

  it('history request with factPath null fetches commitDetail and dispatches first file', async () => {
    mockCommitDetail.mockResolvedValue({
      commit: 'abc123', date: '', message: '',
      files: [{ path: 'kb/first.md', action: 'modified' }, { path: 'kb/second.md', action: 'added' }],
    });
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: null };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(mockCommitDetail).toHaveBeenCalledWith('myrepo', 'abc123');
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: 'abc123', factPath: 'kb/first.md', factCommit: 'abc123' });
  });

  it('history request with factPath null and empty files dispatches with factPath null', async () => {
    mockCommitDetail.mockResolvedValue({ commit: 'abc123', date: '', message: '', files: [] });
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: null };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: 'abc123', factPath: null, factCommit: 'abc123' });
  });

  it('history request with factPath null: api failure dispatches APPLY_NAV with factPath null', async () => {
    mockCommitDetail.mockRejectedValue(new Error('network error'));
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: null };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: 'abc123', factPath: null, factCommit: 'abc123' });
  });

  it('tree request dispatches with historyCommit null and factCommit null', async () => {
    const req: NavRequest = { view: 'tree', factPath: 'kb/foo.md' };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'tree', historyCommit: null, factPath: 'kb/foo.md', factCommit: null });
  });

  it('chrono request with null factPath dispatches with factPath null', async () => {
    const req: NavRequest = { view: 'chrono', factPath: null };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'chrono', historyCommit: null, factPath: null, factCommit: null });
  });

  // ── Mode-switch: { view } ─────────────────────────────────────────────────

  it('mode-switch to history with known factCommit dispatches immediately preserving path filter', async () => {
    const state = makeState({ factPath: 'kb/foo.md', factCommit: 'abc123' });
    await resolveNavRequest({ view: 'history' }, state, dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: 'abc123', factPath: 'kb/foo.md', factCommit: 'abc123' });
  });

  it('mode-switch to history with no factCommit but headCommit dispatches immediately', async () => {
    const state = makeState({ factPath: null, factCommit: null, headCommit: 'head1' });
    await resolveNavRequest({ view: 'history' }, state, dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: 'head1', factPath: null, factCommit: 'head1' });
  });

  it('mode-switch to history with no headCommit dispatches with nulls (HistoryTimeline will amend)', async () => {
    const state = makeState({ factPath: null, factCommit: null, headCommit: '' });
    await resolveNavRequest({ view: 'history' }, state, dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'history', historyCommit: null, factPath: null, factCommit: null });
  });

  it('mode-switch to tree preserves current factPath', async () => {
    const state = makeState({ factPath: 'kb/foo.md', factCommit: 'abc123' });
    await resolveNavRequest({ view: 'tree' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'tree', historyCommit: null, factPath: 'kb/foo.md', factCommit: null });
  });

  it('mode-switch to chrono dispatches with null factPath (ChronoView will amend)', async () => {
    const state = makeState({ factPath: 'kb/foo.md' });
    await resolveNavRequest({ view: 'chrono' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({ type: 'APPLY_NAV', view: 'chrono', historyCommit: null, factPath: null, factCommit: null });
  });
});
