import { describe, it, expect, vi } from 'vitest';
import { resolveLens, refreshContextAfterChange } from './App';
import type { Action } from './state';
import type { Lens, RepoInfo } from './api';

const repos = (...names: string[]): RepoInfo[] => names.map(name => ({ name, uid: `uid-${name}` }));

describe('resolveLens — App-level lens resolution', () => {
  it('dispatches SET_LENS when the lens resolves', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const lens: Lens = { name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }] };
    await resolveLens('dev', repos('core', 'work'), dispatch, vi.fn().mockResolvedValue(lens));
    expect(actions).toEqual([{ type: 'SET_LENS', lens }]);
  });

  it('falls back to the first repo and surfaces a notice when the lens is gone', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn().mockRejectedValue(new Error('404 not found'));
    await resolveLens('deleted', repos('core', 'work'), dispatch, getLens);

    // A user-visible notice, then a fall back to the first repo. The paired
    // developer line goes to the browser console (App's `diag`), not through
    // dispatch — a failed resolve must not be reported to a user twice.
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
    // Never dispatches SET_LENS on failure.
    expect(actions.some(a => a.type === 'SET_LENS')).toBe(false);
  });

  it('surfaces the notice but does not fall back when no repos exist', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await resolveLens('deleted', repos(), dispatch, vi.fn().mockRejectedValue(new Error('gone')));
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions.some(a => a.type === 'SET_CONTEXT')).toBe(false);
  });

  // I3: a slow failing resolve for lens A must not yank the user out of lens B
  // (or a repo) they've since switched to. The guard suppresses the whole
  // fallback (no notice, no console, no context change) when A is no longer current.
  it('does not fall back when the lens context drifted while the resolve was in flight (I3)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn().mockRejectedValue(new Error('slow 404'));
    await resolveLens('A', repos('core', 'work'), dispatch, getLens, () => false);
    expect(actions).toEqual([]);
  });

  it('still falls back when the failing lens is the current one (guard passes)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await resolveLens('A', repos('core'), dispatch, vi.fn().mockRejectedValue(new Error('gone')), () => true);
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
  });

  // The fallback is a rescue: landing it on a repo with no live store would
  // replace one broken surface with another. resolveLens gates its OWN list
  // rather than trusting every caller to have pre-filtered.
  it('skips an unavailable repo when choosing the fallback', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const list: RepoInfo[] = [{ name: 'aaa', uid: 'uid-aaa', state: 'missing' }, ...repos('core')];
    await resolveLens('deleted', list, dispatch, vi.fn().mockRejectedValue(new Error('gone')));
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
  });

  it('surfaces the notice but does not fall back when every repo is unavailable', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const list: RepoInfo[] = [{ name: 'aaa', uid: 'uid-aaa', state: 'missing' }];
    await resolveLens('deleted', list, dispatch, vi.fn().mockRejectedValue(new Error('gone')));
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions.some(a => a.type === 'SET_CONTEXT')).toBe(false);
  });
});

// I4: after the RepoManager reports a create/delete/edit, re-sync the browse
// surface. A mutated active lens re-resolves (refreshing state.lens); a deleted
// browsed lens falls back with the notice; a repo context switches off a removed
// active repo.
describe('refreshContextAfterChange — post-mutation resync', () => {
  it('re-resolves the active lens so edited mounts refresh state.lens (I4)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const edited: Lens = { name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }, { uid: 'uid-infra', name: 'infra' }] };
    const getLens = vi.fn().mockResolvedValue(edited);
    await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'eng' }, 'core', {
      listLenses: vi.fn().mockResolvedValue([edited]),
      repos: vi.fn().mockResolvedValue(repos('core', 'docs', 'infra')),
      getLens,
    });
    expect(getLens).toHaveBeenCalledWith('eng');
    expect(actions).toContainEqual({ type: 'SET_LENS', lens: edited });
    // No fallback/notice while the lens still exists.
    expect(actions.some(a => a.type === 'SET_CONTEXT')).toBe(false);
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(false);
  });

  it('falls back with a notice when the browsed lens was deleted (I4)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn();
    await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'gone' }, 'core', {
      listLenses: vi.fn().mockResolvedValue([]), // no longer listed
      repos: vi.fn().mockResolvedValue(repos('core', 'work')),
      getLens,
    });
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
    // A deleted lens isn't in the list → no wasted getLens round-trip.
    expect(getLens).not.toHaveBeenCalled();
  });

  it('switches off an archived active repo in a repo context (existing behavior)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'gone' }, 'gone', {
      listLenses: vi.fn().mockResolvedValue([]),
      repos: vi.fn().mockResolvedValue(repos('core', 'work')),
    });
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: 'core' });
  });

  // I5: a rename of the ACTIVE repo is not the same case as it going missing.
  // The Danger zone rename control reports {from, to} through onChanged, and
  // this must follow the browse surface to the new name rather than landing
  // on whichever remaining repo happens to sort first — a stale selection
  // still pointed at the old name would 404 on its next read.
  it('follows the active repo to its new name on a rename, instead of falling back to an unrelated repo', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    // 'aaa' is UNRELATED and sorts/lists first — readable[0] — so the OLD
    // fallback (and a broken re-implementation of the fix) would dispatch
    // SET_REPO 'aaa'. Only the renamed-hint branch produces the correct
    // 'zeta', which is deliberately NOT readable[0], so this test actually
    // discriminates between the two behaviors.
    await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'core' }, 'core', {
      listLenses: vi.fn().mockResolvedValue([]),
      repos: vi.fn().mockResolvedValue(repos('aaa', 'zeta')), // 'core' renamed to 'zeta'
    }, () => true, { from: 'core', to: 'zeta' });
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: 'zeta' });
    expect(actions).not.toContainEqual({ type: 'SET_REPO', repo: 'aaa' });
  });

  // A rename elsewhere must not hijack an UNRELATED archive/removal's
  // fallback — `renamed` only steers the branch when it names the repo that
  // was actually active.
  it('ignores a rename hint for a different repo and falls back normally', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'gone' }, 'gone', {
      listLenses: vi.fn().mockResolvedValue([]),
      repos: vi.fn().mockResolvedValue(repos('core', 'other2')),
    }, () => true, { from: 'other', to: 'other2' });
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: 'core' });
  });

  it('clears the repo context when the last repo is archived', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'only' }, 'only', {
      listLenses: vi.fn().mockResolvedValue([]),
      repos: vi.fn().mockResolvedValue([]),
    });
    // state.repo/state.branch key the SSE subscription. Leaving them on the repo
    // that was just archived holds an EventSource open against a route that now
    // 404s, and EventSource retries that forever.
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: '' });
  });

  // The repo listing now carries registered repos with NO live store, whose
  // every endpoint answers 409. pickRepo already refuses to land on one, but
  // this is the sibling path — it runs after every archive/create/restore and
  // after a lens delete — and it used to reach into the raw list.
  describe('with repos that have no live store', () => {
    const broken = (name: string, state: string): RepoInfo => ({ name, uid: `uid-${name}`, state });

    it('does not fall back onto an unavailable repo when the active one is archived', async () => {
      const actions: Action[] = [];
      const dispatch = (a: Action) => void actions.push(a);
      // "aaa" sorts first, so the raw repoList[0] is the broken one.
      await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'gone' }, 'gone', {
        listLenses: vi.fn().mockResolvedValue([]),
        repos: vi.fn().mockResolvedValue([broken('aaa', 'missing'), ...repos('core')]),
      });
      expect(actions).toContainEqual({ type: 'SET_REPO', repo: 'core' });
      expect(actions).not.toContainEqual({ type: 'SET_REPO', repo: 'aaa' });
    });

    it('moves off the current repo once it goes unavailable', async () => {
      const actions: Action[] = [];
      const dispatch = (a: Action) => void actions.push(a);
      // "core" is still LISTED — it is registered — but it has no store, so
      // keeping it would leave the browse surface reading 409s.
      await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'core' }, 'core', {
        listLenses: vi.fn().mockResolvedValue([]),
        repos: vi.fn().mockResolvedValue([broken('core', 'unopenable'), ...repos('work')]),
      });
      expect(actions).toContainEqual({ type: 'SET_REPO', repo: 'work' });
    });

    it('clears the repo context when every listed repo is unavailable', async () => {
      const actions: Action[] = [];
      const dispatch = (a: Action) => void actions.push(a);
      await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'core' }, 'core', {
        listLenses: vi.fn().mockResolvedValue([]),
        repos: vi.fn().mockResolvedValue([broken('core', 'missing')]),
      });
      // Same reasoning as the zero-repo case: state.repo keys the SSE
      // subscription, and there is nothing readable to point it at.
      expect(actions).toContainEqual({ type: 'SET_REPO', repo: '' });
    });

    it('does not land a deleted-lens fallback on an unavailable repo', async () => {
      const actions: Action[] = [];
      const dispatch = (a: Action) => void actions.push(a);
      await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'gone' }, 'core', {
        listLenses: vi.fn().mockResolvedValue([]),
        repos: vi.fn().mockResolvedValue([broken('aaa', 'conflict'), ...repos('core')]),
        getLens: vi.fn(),
      });
      expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
    });

    it('does not hand an unavailable repo to resolveLens as its fallback', async () => {
      const actions: Action[] = [];
      const dispatch = (a: Action) => void actions.push(a);
      // The lens still exists but fails to resolve — resolveLens's own fallback
      // must land somewhere readable too.
      await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'eng' }, 'core', {
        listLenses: vi.fn().mockResolvedValue([{ name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [] }]),
        repos: vi.fn().mockResolvedValue([broken('aaa', 'missing'), ...repos('core')]),
        getLens: vi.fn().mockRejectedValue(new Error('500')),
      });
      expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
    });

    // The two things resolveLens does with the repo list pull in opposite
    // directions. Its FALLBACK wants only readable repos; its readability CHECK
    // wants the whole listing, because brokenLensMember counts positive
    // evidence only — a member the list does not carry is not evidence. Handing
    // it a pre-filtered list therefore disarms the check silently: the dead
    // mount is simply absent, the lens looks fine, and the user lands on a
    // surface where every read answers 503. resolveLens re-filters for its own
    // fallback, so the full listing is the correct input.
    it('refuses a lens whose mount died, rather than pre-filtering the evidence away', async () => {
      const actions: Action[] = [];
      const dispatch = (a: Action) => void actions.push(a);
      const lens = { name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-dead', name: 'dead' }] };
      await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'eng' }, 'core', {
        listLenses: vi.fn().mockResolvedValue([lens]),
        repos: vi.fn().mockResolvedValue([broken('dead', 'missing'), ...repos('core')]),
        getLens: vi.fn().mockResolvedValue(lens),
      });
      expect(actions.some(a => a.type === 'SET_LENS')).toBe(false);
      expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
    });
  });

  it('clears the repo context from a lens surface too when no repos remain', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn();
    await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'eng' }, 'only', {
      listLenses: vi.fn().mockResolvedValue([{ name: 'eng', write: { uid: 'uid-only', name: 'only' }, reads: [] }]),
      repos: vi.fn().mockResolvedValue([]),
      getLens,
    });
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: '' });
    // A lens over zero repos has nothing to resolve, and the app is showing the
    // zero-repo screen regardless — the round-trip would be pure waste.
    expect(getLens).not.toHaveBeenCalled();
  });
});

// A lens binds ALL of its members or none — internal/repos/binding.go fails the
// whole binding the moment one member has no live instance. But
// GET /lenses/{lens} sits OUTSIDE LensMiddleware, so it answers 200 for such a
// lens, and every read endpoint underneath then answers 503.
//
// That is why the rescue below could never fire on this case before: it hangs
// off the catch, and the fetch SUCCEEDED. A persisted context pointing at a
// lens whose mount died overnight dropped the user straight into a surface
// where nothing loads.
describe('resolveLens — a lens with an unreadable mount', () => {
  const brokenLens: Lens = {
    name: 'dev',
    write: { uid: 'uid-work', name: 'work' },
    reads: [{ uid: 'uid-work', name: 'work' }, { uid: 'uid-dead', name: 'dead' }],
  };
  const listing: RepoInfo[] = [
    { name: 'core', uid: 'uid-core' },
    { name: 'work', uid: 'uid-work' },
    { name: 'dead', uid: 'uid-dead', state: 'missing', detail: 'database file not found' },
  ];

  it('refuses the lens and rescues to a readable repo even though the fetch succeeded', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await resolveLens('dev', listing, dispatch, vi.fn().mockResolvedValue(brokenLens));

    expect(actions.some(a => a.type === 'SET_LENS')).toBe(false);
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
  });

  it('still resolves when every mount is readable', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const ok: Lens = { name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }] };
    await resolveLens('dev', listing, dispatch, vi.fn().mockResolvedValue(ok));
    expect(actions).toEqual([{ type: 'SET_LENS', lens: ok }]);
  });

  // A member the listing does not carry is not evidence of breakage — the
  // listing can simply be older than the lens. Gating on absence would refuse
  // every lens for the first frame after mount.
  it('does not refuse a lens whose member the listing has not caught up with', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const fresh: Lens = { name: 'dev', write: { uid: 'uid-brand-new', name: 'brand-new' }, reads: [] };
    await resolveLens('dev', listing, dispatch, vi.fn().mockResolvedValue(fresh));
    expect(actions).toEqual([{ type: 'SET_LENS', lens: fresh }]);
  });
});
