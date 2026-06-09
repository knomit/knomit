import { describe, it, expect, vi, beforeEach } from 'vitest';
import type { Dispatch } from 'react';
import type { Action, AppState } from './state';
import { init } from './state';
import { resolveNavRequest } from './useNavigationManager';
import type { NavRequest } from './useNavigationManager';

function makeState(overrides: Partial<AppState> = {}): AppState {
  return { ...init, repo: 'myrepo', headCommit: 'head1', ...overrides };
}

describe('resolveNavRequest', () => {
  let dispatch: Dispatch<Action>;

  beforeEach(() => {
    dispatch = vi.fn() as unknown as Dispatch<Action>;
  });

  // ── Explicit library navigation ────────────────────────────────────────────

  it('explicit factPath dispatches with live anchor', async () => {
    const req: NavRequest = { view: 'library', factPath: 'kb/foo.md' };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledOnce();
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'library',
      factPath: 'kb/foo.md', asOf: { mode: 'live' },
    });
  });

  it('explicit factPath null dispatches with live anchor', async () => {
    const req: NavRequest = { view: 'library', factPath: null };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'library',
      factPath: null, asOf: { mode: 'live' },
    });
  });

  it('explicit factPath from a scrubbed state demotes asOf to live', async () => {
    const state = makeState({ asOf: { mode: 'scrubbed', commit: 'abc1234' } });
    await resolveNavRequest({ view: 'library', factPath: 'kb/foo.md' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/foo.md',
      asOf: { mode: 'live' },
    }));
  });

  // ── Mode-switch: { view: 'library' } ──────────────────────────────────────

  it('mode-switch preserves current factPath and demotes scrubbed asOf to live', async () => {
    const state = makeState({
      factPath: 'kb/foo.md',
      asOf: { mode: 'scrubbed', commit: 'abc123' },
    });
    await resolveNavRequest({ view: 'library' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'library',
      factPath: 'kb/foo.md', asOf: { mode: 'live' },
    });
  });

  it('mode-switch preserves factPath when already in library view', async () => {
    const state = makeState({ view: 'library', factPath: 'kb/tech/foo.md' });
    await resolveNavRequest({ view: 'library' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'library',
      factPath: 'kb/tech/foo.md', asOf: { mode: 'live' },
    });
  });

  it('mode-switch with no open fact preserves null factPath', async () => {
    const state = makeState({ factPath: null });
    await resolveNavRequest({ view: 'library' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'library',
      factPath: null, asOf: { mode: 'live' },
    });
  });
});
