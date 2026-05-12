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
    history: vi.fn(),
  },
}));

const mockCommitDetail = api.commitDetail as ReturnType<typeof vi.fn>;
const mockHistory = api.history as ReturnType<typeof vi.fn>;

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
    const req: NavRequest = {
      view: 'history',
      factPath: 'kb/foo.md',
      asOf: { mode: 'scrubbed', commit: 'abc1234' },
    };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledOnce();
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: 'kb/foo.md', asOf: { mode: 'scrubbed', commit: 'abc1234' },
    });
  });

  it('history request with factPath null fetches commitDetail and dispatches first file', async () => {
    mockCommitDetail.mockResolvedValue({
      commit: 'abc123', date: '', message: '',
      files: [{ path: 'kb/first.md', action: 'modified' }, { path: 'kb/second.md', action: 'added' }],
    });
    const req: NavRequest = {
      view: 'history',
      factPath: null,
      asOf: { mode: 'scrubbed', commit: 'abc123' },
    };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(mockCommitDetail).toHaveBeenCalledWith('myrepo', '', 'abc123');
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: 'kb/first.md', asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
  });

  it('history request with factPath null and empty files dispatches with factPath null', async () => {
    mockCommitDetail.mockResolvedValue({ commit: 'abc123', date: '', message: '', files: [] });
    const req: NavRequest = {
      view: 'history',
      factPath: null,
      asOf: { mode: 'scrubbed', commit: 'abc123' },
    };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: null, asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
  });

  it('history request with factPath null: api failure dispatches APPLY_NAV with factPath null', async () => {
    mockCommitDetail.mockRejectedValue(new Error('network error'));
    const req: NavRequest = {
      view: 'history',
      factPath: null,
      asOf: { mode: 'scrubbed', commit: 'abc123' },
    };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: null, asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
  });

  it('tree request dispatches with current asOf carried forward', async () => {
    const req: NavRequest = { view: 'tree', factPath: 'kb/foo.md' };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'tree',
      factPath: 'kb/foo.md', asOf: { mode: 'live' },
    });
  });

  it('chrono request with null factPath dispatches with factPath null', async () => {
    const req: NavRequest = { view: 'chrono', factPath: null };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'chrono',
      factPath: null, asOf: { mode: 'live' },
    });
  });

  // ── Mode-switch: { view } ─────────────────────────────────────────────────

  it('mode-switch to history with factPath resolves fact last-touched commit via history()', async () => {
    mockHistory.mockResolvedValue({ entries: [{ commit: 'last999', date: '', message: '' }] });
    const state = makeState({
      factPath: 'kb/foo.md',
      asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
    await resolveNavRequest({ view: 'history' }, state, dispatch);
    expect(mockHistory).toHaveBeenCalledWith('myrepo', '', 'kb/foo.md');
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: 'kb/foo.md', asOf: { mode: 'scrubbed', commit: 'last999' },
    });
  });

  it('mode-switch to history with factPath falls back to current anchor on history() failure', async () => {
    mockHistory.mockRejectedValue(new Error('boom'));
    const state = makeState({
      factPath: 'kb/foo.md',
      asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
    await resolveNavRequest({ view: 'history' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: 'kb/foo.md', asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
  });

  it('mode-switch to history with no factPath but headCommit dispatches at headCommit', async () => {
    const state = makeState({ factPath: null, asOf: { mode: 'live' }, headCommit: 'head1' });
    await resolveNavRequest({ view: 'history' }, state, dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: null, asOf: { mode: 'scrubbed', commit: 'head1' },
    });
  });

  it('mode-switch to history with no headCommit dispatches with live (HistoryTimeline will amend)', async () => {
    const state = makeState({ factPath: null, asOf: { mode: 'live' }, headCommit: '' });
    await resolveNavRequest({ view: 'history' }, state, dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'history',
      factPath: null, asOf: { mode: 'live' },
    });
  });

  it('mode-switch to tree preserves current factPath but demotes asOf to live', async () => {
    const state = makeState({
      factPath: 'kb/foo.md',
      asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
    await resolveNavRequest({ view: 'tree' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'tree',
      factPath: 'kb/foo.md', asOf: { mode: 'live' },
    });
  });

  it('mode-switch from chrono to tree clears factPath (selection from flat list does not carry into hierarchical browser)', async () => {
    // User selects a fact from Recent (chrono), then switches to Tree. The
    // tree resets to the kb root and the right panel must show the stats
    // view, not the previously-selected fact whose path is unrelated to root.
    const state = makeState({ view: 'chrono', factPath: 'kb/tech/gpt5.md' });
    await resolveNavRequest({ view: 'tree' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'tree',
      factPath: null, asOf: { mode: 'live' },
    });
  });

  it('mode-switch from tree to tree preserves factPath (re-entering same context)', async () => {
    const state = makeState({ view: 'tree', factPath: 'kb/tech/foo.md' });
    await resolveNavRequest({ view: 'tree' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'tree',
      factPath: 'kb/tech/foo.md', asOf: { mode: 'live' },
    });
  });

  it('mode-switch to chrono dispatches with null factPath (ChronoView will amend)', async () => {
    const state = makeState({ factPath: 'kb/foo.md' });
    await resolveNavRequest({ view: 'chrono' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'chrono',
      factPath: null, asOf: { mode: 'live' },
    });
  });

  // ── Regression: HEAD-only views demote scrubbed asOf to live ──────────────

  it('switching to tree from a scrubbed history view demotes asOf to live', async () => {
    const state = makeState({
      view: 'history',
      asOf: { mode: 'scrubbed', commit: 'abc1234' },
    });
    await resolveNavRequest({ view: 'tree' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'APPLY_NAV',
      view: 'tree',
      asOf: { mode: 'live' },
    }));
  });

  it('switching to chrono from a scrubbed view demotes asOf to live', async () => {
    const state = makeState({
      view: 'history',
      asOf: { mode: 'scrubbed', commit: 'abc1234' },
    });
    await resolveNavRequest({ view: 'chrono' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'APPLY_NAV',
      view: 'chrono',
      asOf: { mode: 'live' },
    }));
  });

  it('explicit tree fact-path nav from a scrubbed view demotes asOf to live', async () => {
    const state = makeState({
      view: 'history',
      asOf: { mode: 'scrubbed', commit: 'abc1234' },
    });
    await resolveNavRequest({ view: 'tree', factPath: 'kb/foo.md' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'APPLY_NAV',
      view: 'tree',
      factPath: 'kb/foo.md',
      asOf: { mode: 'live' },
    }));
  });

  it('explicit chrono fact-path nav from a diff view demotes asOf to live', async () => {
    const state = makeState({
      view: 'history',
      asOf: { mode: 'diff', from: 'aaaa', to: 'bbbb' },
    });
    await resolveNavRequest({ view: 'chrono', factPath: 'kb/foo.md' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'APPLY_NAV',
      view: 'chrono',
      factPath: 'kb/foo.md',
      asOf: { mode: 'live' },
    }));
  });
});
