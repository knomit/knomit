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

  it('explicit factPath from a history state demotes asOf to live', async () => {
    const state = makeState({ asOf: { mode: 'history', commit: 'abc1234' } });
    await resolveNavRequest({ view: 'library', factPath: 'kb/foo.md' }, state, dispatch);
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/foo.md',
      asOf: { mode: 'live' },
    }));
  });

  // ── Mode-switch: { view: 'library' } ──────────────────────────────────────

  it('mode-switch preserves current factPath and demotes history asOf to live', async () => {
    const state = makeState({
      factPath: 'kb/foo.md',
      asOf: { mode: 'history', commit: 'abc123' },
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

  // ── Reveal: land in the tree where the fact lives ─────────────────────────

  it('reveal navigates the tree to the fact\'s folder and opens it', async () => {
    // Opening a highlight used to set the fact and leave the left panel wherever
    // it was — usually the ontology root — so you arrived at a fact with no idea
    // where in the tree it lived. Reveal makes it look like you browsed there.
    const req: NavRequest = { view: 'library', factPath: 'kb/technology/ai/security/b9f5e0ac.md', reveal: true };
    await resolveNavRequest(req, makeState(), dispatch);
    expect(dispatch).toHaveBeenCalledOnce();
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV', view: 'library',
      factPath: 'kb/technology/ai/security/b9f5e0ac.md',
      asOf: { mode: 'live' },
      filters: [{ category: 'path', value: 'kb/technology/ai/security' }],
      sort: 'path',
    });
  });

  it('reveal strips the kb://<id12>/ qualifier before taking the folder', async () => {
    // A lens fact is addressed kb://<id12>/kb/… but the TREE browses ontology
    // paths, so the path chip must carry the stripped form or the browse 404s.
    const req: NavRequest = { view: 'library', factPath: 'kb://d88770a51516/kb/decisions/ui/x.md', reveal: true };
    await resolveNavRequest(req, makeState(), dispatch);
    const call = (dispatch as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(call.filters).toEqual([{ category: 'path', value: 'kb/decisions/ui' }]);
    // The fact itself keeps its QUALIFIED path — that is its identity, and the
    // tree row it selects carries the same one.
    expect(call.factPath).toBe('kb://d88770a51516/kb/decisions/ui/x.md');
  });

  it('reveal drops the content chips, which would demote the tree it asked for', async () => {
    // Library reads a content chip as "you cannot have the tree" and rewrites
    // sort 'path' to 'recent'. A reveal that kept its chips would therefore land
    // in a filtered flat list rather than the fact's folder — and a highlight is
    // drawn from path-scoped stats, so the fact need not match the chip at all.
    const state = makeState({ filters: [
      { category: 'domain', value: 'ai' },
      { category: 'path', value: 'kb/somewhere/else' },
    ] });
    const req: NavRequest = { view: 'library', factPath: 'kb/meta/x.md', reveal: true };
    await resolveNavRequest(req, state, dispatch);
    const call = (dispatch as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(call.filters).toEqual([{ category: 'path', value: 'kb/meta' }]);
    expect(call.sort).toBe('path');
  });

  it('reveal of a fact directly under the root scopes to the root', async () => {
    const req: NavRequest = { view: 'library', factPath: 'kb/x.md', reveal: true };
    await resolveNavRequest(req, makeState(), dispatch);
    const call = (dispatch as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(call.filters).toEqual([{ category: 'path', value: 'kb' }]);
  });

  it('a plain selection still moves nothing — only reveal navigates the tree', async () => {
    const req: NavRequest = { view: 'library', factPath: 'kb/technology/ai/x.md' };
    await resolveNavRequest(req, makeState(), dispatch);
    const call = (dispatch as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(call.filters).toBeUndefined();
    expect(call.sort).toBeUndefined();
  });
});
