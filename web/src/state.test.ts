import { describe, it, expect } from 'vitest';
import { reducer, init, currentPath, selectAnchorCommit, selectTrail, isLive, isReadOnly, isLensContext, lensResolutionPending, openFactSource, factHistoryAnchor, edgeAnchorCommit } from './state';
import type { AppState, FilterChip } from './state';
import type { Lens, LensSource } from './api';

describe('reducer — cycle-collapse on hop (APPLY_NAV hop:true)', () => {
  const histHop = (factPath: string, commit: string) =>
    ({ type: 'APPLY_NAV' as const, view: 'library' as const, factPath, asOf: { mode: 'history' as const, commit }, hop: true });
  const liveAt = (factPath: string) =>
    ({ type: 'APPLY_NAV' as const, view: 'library' as const, factPath, asOf: { mode: 'live' as const } });

  it('revisiting a fact already in the trail unwinds instead of pushing a duplicate', () => {
    let s = reducer(init, liveAt('kb/a.md'));
    s = reducer(s, histHop('kb/b.md', 'c1'));
    s = reducer(s, histHop('kb/c.md', 'c2'));
    expect(selectTrail(s).map(c => c.factPath)).toEqual(['kb/a.md', 'kb/b.md', 'kb/c.md']);
    // Revisit B (e.g. clicked from C's files-affected) → collapse to [A, B].
    s = reducer(s, histHop('kb/b.md', 'c1'));
    expect(selectTrail(s).map(c => c.factPath)).toEqual(['kb/a.md', 'kb/b.md']);
    expect(s.factPath).toBe('kb/b.md');
    // Oscillate to C, then back to the live root A → trail is just [A], live again.
    s = reducer(s, histHop('kb/c.md', 'c2'));
    s = reducer(s, histHop('kb/a.md', 'c0'));
    expect(selectTrail(s).map(c => c.factPath)).toEqual(['kb/a.md']);
    expect(isLive(s)).toBe(true);
  });

  it('the trail never contains the same fact twice across a long oscillation', () => {
    let s = reducer(init, liveAt('kb/a.md'));
    for (let n = 0; n < 6; n++) {
      s = reducer(s, histHop('kb/b.md', 'c1'));
      s = reducer(s, histHop('kb/a.md', 'c0'));
    }
    const paths = selectTrail(s).map(c => c.factPath);
    expect(new Set(paths).size).toBe(paths.length);
  });

  it('a non-hop APPLY_NAV (deliberate selection / return-to-live) still pushes, never unwinds', () => {
    let s = reducer(init, liveAt('kb/a.md'));        // navStack len 1
    s = reducer(s, histHop('kb/b.md', 'c1'));        // push → len 2
    s = reducer(s, liveAt('kb/a.md'));               // non-hop: push → len 3 (not unwind to 1)
    expect(s.navStack.length).toBe(3);
    expect(s.factPath).toBe('kb/a.md');
    expect(isLive(s)).toBe(true);
  });
});

describe('reducer — filters', () => {
  it('ADD_FILTER appends chip and pushes nav', () => {
    const chip: FilterChip = { category: 'domain', value: 'tech' };
    const s = reducer(init, { type: 'ADD_FILTER', chip });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual(chip);
    expect(s.navStack.length).toBe(1);
  });

  it('ADD_FILTER with path category replaces existing path chip', () => {
    const first: FilterChip = { category: 'path', value: 'kb/tech' };
    const second: FilterChip = { category: 'path', value: 'kb/science' };
    let s = reducer(init, { type: 'ADD_FILTER', chip: first });
    s = reducer(s, { type: 'ADD_FILTER', chip: second });
    const pathChips = s.filters.filter(f => f.category === 'path');
    expect(pathChips).toHaveLength(1);
    expect(pathChips[0].value).toBe('kb/science');
  });

  it('ADD_FILTER with path category clears factPath (breadcrumb up-navigation)', () => {
    // User opens a fact, then clicks a parent breadcrumb segment. The fact must
    // be cleared so the right panel switches back to the stats view for the new
    // path. Other ADD_FILTER categories (domain/entity/type/ep) keep factPath
    // because they're refinements, not navigations.
    let s: AppState = { ...init, factPath: 'kb/tech/ai/foo.md' };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    expect(s.factPath).toBeNull();
  });

  it('ADD_FILTER with non-path category preserves factPath', () => {
    let s: AppState = { ...init, factPath: 'kb/tech/ai/foo.md' };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'ai' } });
    expect(s.factPath).toBe('kb/tech/ai/foo.md');
  });

  it('ADD_FILTER with path category keeps other chips', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/science' } });
    expect(s.filters.find(f => f.category === 'domain')?.value).toBe('tech');
    expect(s.filters.filter(f => f.category === 'path')).toHaveLength(1);
  });

  it('REMOVE_FILTER removes chip at given index and pushes nav', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'a' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'entity', value: 'b' } });
    const before = s.navStack.length;
    s = reducer(s, { type: 'REMOVE_FILTER', index: 0 });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0].value).toBe('b');
    expect(s.navStack.length).toBe(before + 1);
  });

  it('SET_FREE_TEXT sets freeText without pushing nav', () => {
    const s = reducer(init, { type: 'SET_FREE_TEXT', text: 'hello' });
    expect(s.freeText).toBe('hello');
    expect(s.navStack.length).toBe(0);
  });

  it('CLEAR_FILTERS clears filters, freeText and pushes nav', () => {
    let s: AppState = { ...init, filters: [{ category: 'domain', value: 'tech' }], freeText: 'q' };
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    expect(s.filters).toHaveLength(0);
    expect(s.freeText).toBe('');
    expect(s.navStack.length).toBe(1);
  });
});

describe('reducer — nav', () => {
  it('NAV_BACK restores previous view/filters/freeText', () => {
    let s: AppState = { ...init, freeText: 'q' };
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.view).toBe('library');
    expect(s.freeText).toBe('q');
    expect(s.navStack.length).toBe(0);
  });

  it('NAV_BACK restores filters from stack', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'entity', value: 'ai' } });
    const withFilters = s;
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.filters).toEqual(withFilters.filters);
  });

  it('NAV_BACK on empty stack is no-op', () => {
    const s = reducer(init, { type: 'NAV_BACK' });
    expect(s).toBe(init);
  });

  it('NAV_BACK clears rightPanelFocused', () => {
    let s = reducer(init, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });
    s = reducer({ ...s, rightPanelFocused: true }, { type: 'NAV_BACK' });
    expect(s.rightPanelFocused).toBe(false);
  });

  it('nav stack caps at 20 entries', () => {
    let s = init;
    for (let i = 0; i < 22; i++) {
      s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });
    }
    expect(s.navStack.length).toBe(20);
  });
});

describe('currentPath()', () => {
  it('returns ontologyRoot when no path chip', () => {
    const s = { ...init, ontologyRoot: 'kb/tech' };
    expect(currentPath(s)).toBe('kb/tech');
  });

  it('returns path chip value when present', () => {
    const s = { ...init, filters: [{ category: 'path' as const, value: 'kb/technology/ai' }] };
    expect(currentPath(s)).toBe('kb/technology/ai');
  });

  it('returns ontologyRoot when no path chip exists', () => {
    const s = { ...init, ontologyRoot: 'knowledge' };
    expect(currentPath(s)).toBe('knowledge');
  });

  it('returns kb fallback when both empty', () => {
    const s = { ...init, ontologyRoot: '' };
    expect(currentPath(s)).toBe('kb');
  });
});

describe('reducer — NAVIGATE', () => {
  it('replaces path chip and pushes nav', () => {
    const s = reducer(init, { type: 'NAVIGATE', path: 'kb/tech' });
    expect(currentPath(s)).toBe('kb/tech');
    expect(s.navStack.length).toBe(1);
  });

  it('does not affect non-path filter chips', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    s = reducer(s, { type: 'NAVIGATE', path: 'kb/tech' });
    expect(currentPath(s)).toBe('kb/tech');
    expect(s.filters.find(f => f.category === 'domain')!.value).toBe('go');
  });
});

describe('reducer — GO_UP', () => {
  it('removes last path segment from path chip', () => {
    let s0 = reducer(init, { type: 'NAVIGATE', path: 'kb/tech/go' });
    const s = reducer(s0, { type: 'GO_UP' });
    expect(currentPath(s)).toBe('kb/tech');
  });

  it('does nothing at root', () => {
    // With ontologyRoot='kb' and no path chip, currentPath = 'kb' (single segment)
    const s = reducer(init, { type: 'GO_UP' });
    expect(currentPath(s)).toBe('kb');
  });
});

describe('reducer — SET_REPO', () => {
  it('resets navigation state when switching repos', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });
    s = reducer(s, { type: 'SET_REPO', repo: 'work' });
    expect(s.repo).toBe('work');
    expect(s.view).toBe('library');
    expect(s.filters).toHaveLength(0);
    expect(s.freeText).toBe('');
    expect(s.navStack).toHaveLength(0);
    expect(s.headCommit).toBe('');
    expect(s.branch).toBe('');
    expect(s.remoteError).toBe('');
    expect(s.rightPanelFocused).toBe(false);
  });

  it('init has no repo selected (repo is chosen from the server list on mount)', () => {
    expect(init.repo).toBe('');
  });
});

describe('reducer — BrowseContext (SET_CONTEXT / SET_REPO wrapper)', () => {
  const lens: Lens = { name: 'dev', write: 'work', reads: [{ repo: 'work' }, { repo: 'core' }] };
  const source: LensSource = { repo: 'core', id: 'abc123def456', branch: 'main' };

  it('init is a repo context matching init.repo', () => {
    expect(init.context).toEqual({ kind: 'repo', repo: '' });
    expect(isLensContext(init)).toBe(false);
    expect(init.lens).toBeNull();
    expect(init.lensSources).toBeNull();
    expect(init.factSource).toBeNull();
  });

  it('SET_CONTEXT {kind:repo} resets asOf/fact/filters like a repo switch', () => {
    let s: AppState = { ...init };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/x.md', asOf: { mode: 'history', commit: 'abc1234' } });
    s = reducer(s, { type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'work' } });
    expect(s.context).toEqual({ kind: 'repo', repo: 'work' });
    expect(s.repo).toBe('work');
    expect(s.view).toBe('library');
    expect(s.factPath).toBeNull();
    expect(s.asOf).toEqual({ mode: 'live' });
    expect(s.filters).toHaveLength(0);
    expect(s.freeText).toBe('');
    expect(s.navStack).toHaveLength(0);
    expect(s.headCommit).toBe('');
    expect(s.branch).toBe('');
    expect(s.remoteError).toBe('');
    expect(s.rightPanelFocused).toBe(false);
    expect(s.lens).toBeNull();
    expect(s.lensSources).toBeNull();
  });

  it('SET_REPO behaves byte-identically to SET_CONTEXT {kind:repo} on the shared fields', () => {
    const start: AppState = { ...init, filters: [{ category: 'domain', value: 'go' }], factPath: 'kb/y.md', freeText: 'q' };
    const viaRepo = reducer(start, { type: 'SET_REPO', repo: 'work' });
    const viaContext = reducer(start, { type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'work' } });
    expect(viaRepo).toEqual(viaContext);
  });

  it('SET_REPO still resets navigation state (regression: existing consumers)', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'SET_REPO', repo: 'work' });
    expect(s.repo).toBe('work');
    expect(s.context).toEqual({ kind: 'repo', repo: 'work' });
    expect(s.filters).toHaveLength(0);
    expect(s.branch).toBe('');
  });

  it('SET_CONTEXT {kind:lens} enters a lens context and resets navigation', () => {
    let s: AppState = { ...init, repo: 'work', branch: 'agent/x' };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'dev' } });
    expect(s.context).toEqual({ kind: 'lens', name: 'dev' });
    expect(isLensContext(s)).toBe(true);
    expect(s.lens).toBeNull();
    expect(s.filters).toHaveLength(0);
    expect(s.asOf).toEqual({ mode: 'live' });
    // repo/branch stay valid (previous values) until SET_LENS resolves the write mount.
    expect(s.repo).toBe('work');
    expect(s.branch).toBe('agent/x');
  });

  it('SET_LENS stores the lens doc and points repo at the write mount', () => {
    let s = reducer(init, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'dev' } });
    s = reducer(s, { type: 'SET_LENS', lens });
    expect(s.lens).toEqual(lens);
    expect(s.repo).toBe('work');
    expect(s.branch).toBe(''); // cleared → status bootstrap resolves the agent branch
  });

  it('lensResolutionPending flags an unresolved lens context from ANY entry surface', () => {
    // Regression: entering a lens via the TopBar switcher only dispatches
    // SET_CONTEXT — nothing else runs. App's resolution effect keys off this
    // predicate; if it missed the unresolved state, RightPanel fell through to
    // the repo-scoped fetch with a raw kb:// path (404).
    let s = reducer(init, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'dev' } });
    expect(lensResolutionPending(s)).toBe(true);       // entered, not resolved
    s = reducer(s, { type: 'SET_LENS', lens });
    expect(lensResolutionPending(s)).toBe(false);      // resolved
    s = reducer(s, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'other' } });
    expect(lensResolutionPending(s)).toBe(true);       // switched to another lens
    s = reducer(s, { type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'work' } });
    expect(lensResolutionPending(s)).toBe(false);      // repo context never pends
  });

  it('SET_LENS_SOURCES sets the sources selection (null = all mounts)', () => {
    let s = reducer(init, { type: 'SET_LENS_SOURCES', repos: ['core', 'work'] });
    expect(s.lensSources).toEqual(['core', 'work']);
    s = reducer(s, { type: 'SET_LENS_SOURCES', repos: null });
    expect(s.lensSources).toBeNull();
  });

  it('SET_FACT_SOURCE sets and clears the open fact source mount', () => {
    // A source is only meaningful while a fact is open, so open one first.
    let s = reducer(init, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/x.md', asOf: { mode: 'live' } });
    s = reducer(s, { type: 'SET_FACT_SOURCE', source });
    expect(s.factSource).toEqual(source);
    s = reducer(s, { type: 'SET_FACT_SOURCE', source: null });
    expect(s.factSource).toBeNull();
  });

  it('factSource is cleared on context switch', () => {
    let s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, factSource: source, factPath: 'kb/x.md' };
    s = reducer(s, { type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'work' } });
    expect(s.factSource).toBeNull();
  });

  it('factSource is cleared when the open fact closes (factPath -> null)', () => {
    // Open a lens fact and record its source mount, then navigate away (fact
    // closes). The source must not linger for a direct reader.
    let s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens };
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/x.md', asOf: { mode: 'live' } });
    s = reducer(s, { type: 'SET_FACT_SOURCE', source });
    expect(s.factSource).toEqual(source);
    // Close the fact via APPLY_NAV to null.
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });
    expect(s.factPath).toBeNull();
    expect(s.factSource).toBeNull();
  });

  it('SET_LENS is a no-op after switching back to a repo context (stale resolution)', () => {
    // resolveLens(A) is in flight; user switches to a repo context; the late
    // SET_LENS{A} must not repoint repo or set lens in a repo context.
    let s = reducer(init, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'dev' } });
    s = reducer(s, { type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'other' } });
    const before = s;
    s = reducer(s, { type: 'SET_LENS', lens });
    expect(s).toBe(before);          // untouched
    expect(s.lens).toBeNull();
    expect(s.repo).toBe('other');    // not yanked to lens.write
    expect(s.context).toEqual({ kind: 'repo', repo: 'other' });
  });

  it('SET_LENS is a no-op when a different lens is now active (out-of-order resolution)', () => {
    // resolveLens(A) resolves after the user already switched to lens B.
    let s = reducer(init, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'dev' } });
    s = reducer(s, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'other' } });
    const before = s;
    s = reducer(s, { type: 'SET_LENS', lens }); // lens.name === 'dev', context is 'other'
    expect(s).toBe(before);
    expect(s.lens).toBeNull();
    expect(s.context).toEqual({ kind: 'lens', name: 'other' });
  });

  it('SET_LENS applies when its name matches the active lens context', () => {
    let s = reducer(init, { type: 'SET_CONTEXT', context: { kind: 'lens', name: 'dev' } });
    s = reducer(s, { type: 'SET_LENS', lens });
    expect(s.lens).toEqual(lens);
    expect(s.repo).toBe('work');
  });
});

describe('openFactSource — the temporal/write anchor', () => {
  const lens: Lens = { name: 'dev', write: 'work', reads: [{ repo: 'work' }, { repo: 'core' }] };
  const source: LensSource = { repo: 'core', id: 'abc123def456', branch: 'main' };

  it('repo context → {state.repo, state.branch}', () => {
    const s: AppState = { ...init, context: { kind: 'repo', repo: 'work' }, repo: 'work', branch: 'agent/x' };
    expect(openFactSource(s)).toEqual({ repo: 'work', branch: 'agent/x' });
  });

  it('lens context with an open fact → the fact source mount', () => {
    const s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, factPath: 'kb://abc123def456/kb/x.md', factSource: source };
    expect(openFactSource(s)).toEqual({ repo: 'core', branch: 'main' });
  });

  it('lens context with no open fact → {lens.write, ""} (branch "" = resolve agent branch)', () => {
    const s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, factPath: null, factSource: null };
    expect(openFactSource(s)).toEqual({ repo: 'work', branch: '' });
  });

  it('lens context falls back to lens.write once the fact is closed (stale factSource ignored)', () => {
    const s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, factPath: null, factSource: source };
    expect(openFactSource(s)).toEqual({ repo: 'work', branch: '' });
  });
});

describe('factHistoryAnchor — mount + RELATIVE path, co-located', () => {
  const lens: Lens = { name: 'dev', write: 'work', reads: [{ repo: 'work' }, { repo: 'core' }] };
  const source: LensSource = { repo: 'core', id: 'abc123def456', branch: 'main' };

  it('repo context → {state.repo, state.branch, bare path} (byte-identical)', () => {
    const s: AppState = { ...init, context: { kind: 'repo', repo: 'work' }, repo: 'work', branch: 'agent/x', factPath: 'kb/a.md' };
    expect(factHistoryAnchor(s)).toEqual({ repo: 'work', branch: 'agent/x', path: 'kb/a.md' });
  });

  it('lens read-mount fact → mount repo/branch + relative path (kb://<id12>/ stripped)', () => {
    const s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, factPath: 'kb://abc123def456/kb/x.md', factSource: source };
    expect(factHistoryAnchor(s)).toEqual({ repo: 'core', branch: 'main', path: 'kb/x.md' });
  });

  it('honours an explicit path arg (e.g. an edge target), still stripping the qualifier', () => {
    const s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, factPath: 'kb://abc123def456/kb/x.md', factSource: source };
    expect(factHistoryAnchor(s, 'kb://abc123def456/kb/y.md')).toEqual({ repo: 'core', branch: 'main', path: 'kb/y.md' });
  });
});

describe('edgeAnchorCommit — mount-safe live edge anchor', () => {
  const lens: Lens = { name: 'dev', write: 'work', reads: [{ repo: 'work' }, { repo: 'core' }] };
  const source: LensSource = { repo: 'core', id: 'abc123def456', branch: 'main' };

  it('repo context, live → state.headCommit (the repo\'s own head)', () => {
    const s: AppState = { ...init, context: { kind: 'repo', repo: 'work' }, repo: 'work', branch: 'agent/x', headCommit: 'repohead', asOf: { mode: 'live' } };
    expect(edgeAnchorCommit(s)).toBe('repohead');
  });

  it('lens context, live with an open read-mount fact → "" (never the WRITE repo head)', () => {
    // state.headCommit here is the write repo\'s head — passing it against the read
    // mount would 404. Live lens edges must resolve at the mount\'s live HEAD.
    const s: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, repo: 'work', branch: 'agent/x', headCommit: 'writehead', factPath: 'kb://abc123def456/kb/x.md', factSource: source, asOf: { mode: 'live' } };
    expect(edgeAnchorCommit(s)).toBe('');
  });

  it('history/diff → the time-travel anchor (a commit from the fact\'s own mount timeline)', () => {
    const hist: AppState = { ...init, context: { kind: 'lens', name: 'dev' }, lens, factSource: source, factPath: 'kb://abc123def456/kb/x.md', asOf: { mode: 'history', commit: 'mountc1' } };
    expect(edgeAnchorCommit(hist)).toBe('mountc1');
    const diff: AppState = { ...init, asOf: { mode: 'diff', from: 'f1', to: 't1' } };
    expect(edgeAnchorCommit(diff)).toBe('t1');
  });
});

describe('reducer — shared infrastructure', () => {
  it('SET_TASK updates task status', () => {
    const next = reducer(init, { type: 'SET_TASK', op: 'sync', status: 'running', message: 'syncing' });
    expect(next.tasks.sync).toEqual({ status: 'running', message: 'syncing' });
  });

  it('SET_TASK returns same reference when no change', () => {
    const s = { ...init, tasks: { ...init.tasks, sync: { status: 'idle' as const, message: '' } } };
    const next = reducer(s, { type: 'SET_TASK', op: 'sync', status: 'idle', message: '' });
    expect(next).toBe(s);
  });

  it('SET_STATUS sets head, branch, embeddingsEnabled, ontologyRoot', () => {
    const next = reducer(init, { type: 'SET_STATUS', head: 'abc123', branch: 'main', embeddingsEnabled: true, ontologyRoot: 'knowledge' });
    expect(next.headCommit).toBe('abc123');
    expect(next.branch).toBe('main');
    expect(next.embeddingsEnabled).toBe(true);
    expect(next.ontologyRoot).toBe('knowledge');
  });

  it('SET_STATUS keeps ontologyRoot when empty string provided', () => {
    const s = { ...init, ontologyRoot: 'kb/custom' };
    const next = reducer(s, { type: 'SET_STATUS', head: 'abc', branch: 'main', embeddingsEnabled: false, ontologyRoot: '' });
    expect(next.ontologyRoot).toBe('kb/custom');
  });

  it('SET_HEAD only updates headCommit', () => {
    const next = reducer(init, { type: 'SET_HEAD', head: 'def456' });
    expect(next.headCommit).toBe('def456');
    expect(next.branch).toBe(init.branch);
  });

  // The console reducer cases moved to consoleStore.test.ts along with the ring
  // buffer itself — see consoleStore.tsx for why. What remains to assert here is
  // that the app reducer treats them as inert: console actions ride the same
  // Action union, so a mis-routed one must not perturb AppState (and must not
  // even produce a new object, or it would re-render the app).
  it('console actions are inert in the app reducer (the console store owns them)', () => {
    expect(reducer(init, { type: 'CONSOLE_LOG', level: 'info', message: 'hello' })).toBe(init);
    expect(reducer(init, { type: 'CONSOLE_TOGGLE' })).toBe(init);
    expect(reducer(init, { type: 'CONSOLE_SET_HEIGHT', height: 300 })).toBe(init);
  });

  it('SET_REMOTE_ERROR sets remoteError', () => {
    const s = reducer(init, { type: 'SET_REMOTE_ERROR', error: 'auth failed' });
    expect(s.remoteError).toBe('auth failed');
  });

  it('FOCUS_RIGHT_PANEL sets rightPanelFocused to true', () => {
    expect(reducer(init, { type: 'FOCUS_RIGHT_PANEL' }).rightPanelFocused).toBe(true);
  });

  it('BLUR_RIGHT_PANEL sets rightPanelFocused to false', () => {
    const s = reducer({ ...init, rightPanelFocused: true }, { type: 'BLUR_RIGHT_PANEL' });
    expect(s.rightPanelFocused).toBe(false);
  });
});

// ─── Regression tests for breadcrumb/path sync bugs ─────────────────────────

describe('currentPath — path chip takes precedence', () => {
  it('returns path chip value when both chip and ontologyRoot exist', () => {
    const s: AppState = {
      ...init,
      filters: [{ category: 'path', value: 'kb/technology/ai' }],
    };
    expect(currentPath(s)).toBe('kb/technology/ai');
  });

  it('falls back to ontologyRoot when no path chip', () => {
    const s: AppState = {
      ...init,
      ontologyRoot: 'kb/technology',
      filters: [{ category: 'domain', value: 'go' }],
    };
    expect(currentPath(s)).toBe('kb/technology');
  });
});

describe('breadcrumb and path chip stay in sync', () => {
  it('ADD_FILTER with path replaces existing path chip (simulates breadcrumb click)', () => {
    // Navigate deep: kb -> kb/technology -> kb/technology/ai -> kb/technology/ai/anthropic
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/technology' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/technology/ai' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/technology/ai/anthropic' } });
    expect(currentPath(s)).toBe('kb/technology/ai/anthropic');
    expect(s.filters.filter(f => f.category === 'path')).toHaveLength(1);

    // Click breadcrumb "kb/technology" — replaces path chip, doesn't add second one
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/technology' } });
    expect(currentPath(s)).toBe('kb/technology');
    expect(s.filters.filter(f => f.category === 'path')).toHaveLength(1);
    expect(s.filters.find(f => f.category === 'path')!.value).toBe('kb/technology');
  });

  it('non-path filters are preserved when path chip changes', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb' } });
    expect(s.filters).toHaveLength(2); // domain:go + path:kb
    expect(s.filters.find(f => f.category === 'domain')!.value).toBe('go');
    expect(s.filters.find(f => f.category === 'path')!.value).toBe('kb');
  });
});

describe('removing path chip resets to ontology root', () => {
  it('REMOVE_FILTER on path chip resets to ontologyRoot', () => {
    let s: AppState = { ...init, ontologyRoot: 'kb' };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/technology/ai' } });
    expect(currentPath(s)).toBe('kb/technology/ai');

    // Remove the path chip (find its index)
    const pathIdx = s.filters.findIndex(f => f.category === 'path');
    s = reducer(s, { type: 'REMOVE_FILTER', index: pathIdx });
    expect(s.filters.filter(f => f.category === 'path')).toHaveLength(0);
    expect(currentPath(s)).toBe('kb');
  });

  it('REMOVE_FILTER on non-path chip does not reset path', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });

    // Remove the domain chip
    const domainIdx = s.filters.findIndex(f => f.category === 'domain');
    s = reducer(s, { type: 'REMOVE_FILTER', index: domainIdx });
    expect(currentPath(s)).toBe('kb/tech'); // path chip unchanged
  });

  it('CLEAR_FILTERS resets to ontologyRoot', () => {
    let s: AppState = { ...init, ontologyRoot: 'kb' };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/deep/nested' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    expect(s.filters).toHaveLength(0);
    expect(currentPath(s)).toBe('kb');
  });
});

describe('back navigation restores path state', () => {
  it('NAV_BACK after deep navigation restores previous path', () => {
    let s: AppState = init;
    // Navigate deep
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech/go' } });
    expect(currentPath(s)).toBe('kb/tech/go');

    // Go back
    s = reducer(s, { type: 'NAV_BACK' });
    expect(currentPath(s)).toBe('kb/tech');

    // Go back again
    s = reducer(s, { type: 'NAV_BACK' });
    expect(currentPath(s)).toBe('kb');
  });
});

// ─── Regression: NAVIGATE/GO_UP actions ─────────────────────────────────────

describe('NAVIGATE action', () => {
  it('sets path chip and pushes nav', () => {
    const s = reducer(init, { type: 'NAVIGATE', path: 'kb/technology/ai' });
    expect(currentPath(s)).toBe('kb/technology/ai');
    expect(s.navStack.length).toBe(1);
  });

  it('does not affect non-path filter chips', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    s = reducer(s, { type: 'NAVIGATE', path: 'kb/tech' });
    expect(currentPath(s)).toBe('kb/tech');
    expect(s.filters.find(f => f.category === 'domain')!.value).toBe('go');
  });
});

describe('GO_UP action', () => {
  it('removes last path segment via path chip', () => {
    let s = reducer(init, { type: 'NAVIGATE', path: 'kb/tech/go' });
    s = reducer(s, { type: 'GO_UP' });
    expect(currentPath(s)).toBe('kb/tech');
  });

  it('does nothing at single-segment root', () => {
    // ontologyRoot='kb', no path chip => currentPath='kb' (single segment)
    const s = reducer(init, { type: 'GO_UP' });
    expect(currentPath(s)).toBe('kb');
    expect(s.navStack.length).toBe(0); // no nav push when no-op
  });

});

// ─── Regression: multiple same-category type chips (OR semantics) ───────────

describe('multiple type chips accumulate (OR semantics)', () => {
  it('adding two type chips keeps both', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'concept' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'principle' } });
    const typeChips = s.filters.filter(f => f.category === 'type');
    expect(typeChips).toHaveLength(2);
    expect(typeChips.map(c => c.value).sort()).toEqual(['concept', 'principle']);
  });

  it('removing one type chip keeps the other', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'concept' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'principle' } });
    // Remove first type chip (index 0)
    s = reducer(s, { type: 'REMOVE_FILTER', index: 0 });
    const typeChips = s.filters.filter(f => f.category === 'type');
    expect(typeChips).toHaveLength(1);
    expect(typeChips[0].value).toBe('principle');
  });
});

// ─── Regression: multiple domain chips accumulate (OR semantics) ────────────

describe('multiple domain chips accumulate (OR semantics)', () => {
  it('adding two domain chips keeps both', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'rust' } });
    const domainChips = s.filters.filter(f => f.category === 'domain');
    expect(domainChips).toHaveLength(2);
  });
});

// ─── Regression: multiple entity chips accumulate (AND semantics) ───────────

describe('multiple entity chips accumulate (AND semantics)', () => {
  it('adding two entity chips keeps both', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'entity', value: 'goroutine' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'entity', value: 'channel' } });
    const entityChips = s.filters.filter(f => f.category === 'entity');
    expect(entityChips).toHaveLength(2);
  });
});

// ─── Regression: SET_STATUS ─────────────────────────────────────────────────

describe('SET_STATUS initializes ontologyRoot', () => {
  it('updates ontologyRoot on first load', () => {
    const s = reducer(init, {
      type: 'SET_STATUS', head: 'abc', branch: 'main',
      embeddingsEnabled: true, ontologyRoot: 'knowledge',
    });
    expect(s.ontologyRoot).toBe('knowledge');
    expect(currentPath(s)).toBe('knowledge');
  });

  it('does not overwrite ontologyRoot if already set and new is empty', () => {
    let s: AppState = { ...init, ontologyRoot: 'kb/custom' };
    s = reducer(s, {
      type: 'SET_STATUS', head: 'abc', branch: 'main',
      embeddingsEnabled: true, ontologyRoot: '',
    });
    expect(s.ontologyRoot).toBe('kb/custom');
  });
});

// ─── Regression: mixed filters and path navigation ─────────────────────────

describe('mixed filters with path navigation', () => {
  it('adding type chip then navigating preserves type chip', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'concept' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    expect(s.filters).toHaveLength(2);
    expect(s.filters.find(f => f.category === 'type')!.value).toBe('concept');
    expect(currentPath(s)).toBe('kb/tech');
  });

  it('removing path chip preserves type chip and resets path', () => {
    let s: AppState = { ...init, ontologyRoot: 'kb' };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'concept' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    const pathIdx = s.filters.findIndex(f => f.category === 'path');
    s = reducer(s, { type: 'REMOVE_FILTER', index: pathIdx });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual({ category: 'type', value: 'concept' });
    expect(currentPath(s)).toBe('kb');
  });

  it('NAV_BACK restores both path and type filters', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'concept' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    // Clear everything
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    expect(s.filters).toHaveLength(0);
    // Go back — should restore both chips
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.filters).toHaveLength(2);
    expect(s.filters.find(f => f.category === 'type')!.value).toBe('concept');
    expect(currentPath(s)).toBe('kb/tech');
  });
});

// ─── Regression: free text persistence ──────────────────────────────────────

describe('free text state management', () => {
  it('SET_FREE_TEXT sets freeText', () => {
    const s = reducer(init, { type: 'SET_FREE_TEXT', text: 'enforcement is active' });
    expect(s.freeText).toBe('enforcement is active');
  });

  it('SET_FREE_TEXT with empty string clears freeText', () => {
    let s: AppState = { ...init, freeText: 'some query' };
    s = reducer(s, { type: 'SET_FREE_TEXT', text: '' });
    expect(s.freeText).toBe('');
  });

  it('SET_FREE_TEXT does not push to navStack (typing should not flood history)', () => {
    const s = reducer(init, { type: 'SET_FREE_TEXT', text: 'hello' });
    expect(s.navStack).toHaveLength(0);
  });

  it('APPLY_NAV preserves freeText when not explicitly passed', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'search query' });
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });
    expect(s.freeText).toBe('search query');
  });

  it('CLEAR_FILTERS clears freeText', () => {
    let s: AppState = { ...init, freeText: 'active query' };
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    expect(s.freeText).toBe('');
    expect(s.filters).toHaveLength(0);
  });

  it('NAV_BACK restores freeText from previous state', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'original query' });
    // ADD_FILTER pushes nav (which captures current freeText)
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    // Change freeText
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'new query' });
    // Clear everything
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    expect(s.freeText).toBe('');
    // Go back — should restore freeText from before CLEAR
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.freeText).toBe('new query');
  });

  it('SET_REPO clears freeText', () => {
    let s: AppState = { ...init, freeText: 'active query' };
    s = reducer(s, { type: 'SET_REPO', repo: 'other' });
    expect(s.freeText).toBe('');
  });

  it('SET_FREE_TEXT to empty clears auto-selected factPath when no other filters remain', () => {
    // Regression: in tree mode, searching auto-selects the first result into
    // factPath. Clicking the 'x' on the freeText chip dispatched SET_FREE_TEXT
    // with text='' but did NOT clear factPath, so the right panel kept showing
    // the search-auto-selected fact instead of returning to root stats.
    let s: AppState = { ...init, freeText: 'some query', factPath: 'kb/x.md' };
    s = reducer(s, { type: 'SET_FREE_TEXT', text: '' });
    expect(s.freeText).toBe('');
    expect(s.factPath).toBeNull();
  });

  it('SET_FREE_TEXT to empty preserves factPath when other non-path filters remain', () => {
    // If chips are still active, the user is still in search/filter mode;
    // their selected fact remains relevant.
    let s: AppState = {
      ...init,
      freeText: 'some query',
      factPath: 'kb/x.md',
      filters: [{ category: 'type', value: 'hypothesis' }],
    };
    s = reducer(s, { type: 'SET_FREE_TEXT', text: '' });
    expect(s.freeText).toBe('');
    expect(s.factPath).toBe('kb/x.md');
  });

  it('SET_FREE_TEXT to non-empty preserves factPath (user typing)', () => {
    let s: AppState = { ...init, freeText: '', factPath: 'kb/x.md' };
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'hello' });
    expect(s.factPath).toBe('kb/x.md');
  });
});

// ─── Regression: free text + filter chips coexist ───────────────────────────

describe('free text and filter chips coexist', () => {
  it('freeText and type chip both active simultaneously', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'scheduling' });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'concept' } });
    expect(s.freeText).toBe('scheduling');
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual({ category: 'type', value: 'concept' });
  });

  it('removing type chip preserves freeText', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'scheduling' });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'type', value: 'concept' } });
    s = reducer(s, { type: 'REMOVE_FILTER', index: 0 });
    expect(s.freeText).toBe('scheduling');
    expect(s.filters).toHaveLength(0);
  });

  it('freeText + path chip + domain chip all active', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'goroutine' });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'go' } });
    expect(s.freeText).toBe('goroutine');
    expect(s.filters).toHaveLength(2);
    expect(currentPath(s)).toBe('kb/tech');
  });
});

// ─── Regression: episode filtering ─────────────────────────────────────────

describe('episode (ep) chips', () => {
  it('ep chip can be added alongside other filters', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'ep', value: 'learn' } });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual({ category: 'ep', value: 'learn' });
  });

  it('multiple ep chips accumulate (OR semantics)', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'ep', value: 'learn' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'ep', value: 'retract' } });
    const epChips = s.filters.filter(f => f.category === 'ep');
    expect(epChips).toHaveLength(2);
    expect(epChips.map(c => c.value).sort()).toEqual(['learn', 'retract']);
  });

  it('CLEAR_FILTERS removes ep chips', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'ep', value: 'learn' } });
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    expect(s.filters).toHaveLength(0);
  });

  it('ep chip + freeText coexist for filtering', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'ep', value: 'retract' } });
    s = reducer(s, { type: 'SET_FREE_TEXT', text: 'cybersecurity' });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual({ category: 'ep', value: 'retract' });
    expect(s.freeText).toBe('cybersecurity');
  });
});


// ─── Full workflow scenarios: operation hierarchy ────────────────────────────

describe('operation hierarchy — full workflow scenarios', () => {
  it('APPLY_NAV with factPath → APPLY_NAV change → NAV_BACK restores each step', () => {
    let s: AppState = init;
    // Open a fact
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/tree-fact.md', asOf: { mode: 'live' } });
    expect(s.view).toBe('library');
    expect(s.factPath).toBe('kb/tree-fact.md');

    // Navigate away
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });
    expect(s.view).toBe('library');

    // APPLY_NAV: select a commit (history asOf)
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'history', commit: 'ccc333' } });
    expect(selectAnchorCommit(s)).toBe('ccc333');

    // APPLY_NAV: select a fact with that asOf
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/hist-fact.md', asOf: { mode: 'history', commit: 'ccc333' } });
    expect(s.factPath).toBe('kb/hist-fact.md');

    // NAV_BACK: restore before fact selection
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.factPath).toBeNull();
    expect(selectAnchorCommit(s)).toBe('ccc333');

    // NAV_BACK: restore before commit selection
    s = reducer(s, { type: 'NAV_BACK' });
    expect(selectAnchorCommit(s)).toBeNull();

    // NAV_BACK: restore before navigation away
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.view).toBe('library');
    expect(s.factPath).toBe('kb/tree-fact.md');
  });

  it('ep filter → change filter → NAV_BACK restores previous filter', () => {
    let s: AppState = init;
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'ep', value: 'learn' } });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual({ category: 'ep', value: 'learn' });

    // Add another ep filter
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'ep', value: 'retract' } });
    expect(s.filters).toHaveLength(2);

    // NAV_BACK: restore before adding retract
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual({ category: 'ep', value: 'learn' });
  });

  it('open fact → navigate → select commit → select fact → NAV_BACK ×N returns to original', () => {
    let s: AppState = init;

    // Open a fact
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/original.md', asOf: { mode: 'live' } });
    expect(s.view).toBe('library');
    expect(s.factPath).toBe('kb/original.md');

    // Navigate away (clear factPath)
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'live' } });

    // Select a commit
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: null, asOf: { mode: 'history', commit: 'xxx' } });

    // Select a fact at that commit
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/history-fact.md', asOf: { mode: 'history', commit: 'xxx' } });
    expect(s.factPath).toBe('kb/history-fact.md');

    // NAV_BACK ×3 should get us back to original fact
    s = reducer(s, { type: 'NAV_BACK' }); // undo APPLY_NAV fact
    s = reducer(s, { type: 'NAV_BACK' }); // undo APPLY_NAV commit
    s = reducer(s, { type: 'NAV_BACK' }); // undo APPLY_NAV away → back to original

    expect(s.view).toBe('library');
    expect(s.factPath).toBe('kb/original.md');
  });

});

describe('reducer — APPLY_NAV', () => {
  it('sets view, factPath, asOf atomically and pushes nav', () => {
    const s = reducer(init, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/foo.md',
      asOf: { mode: 'history', commit: 'abc123' },
    });
    expect(s.view).toBe('library');
    expect(selectAnchorCommit(s)).toBe('abc123');
    expect(s.factPath).toBe('kb/foo.md');
    expect(s.asOf).toEqual({ mode: 'history', commit: 'abc123' });
    expect(s.navStack.length).toBe(1);
  });

  it('APPLY_NAV clears asOf back to live when live is passed', () => {
    const s = reducer(
      { ...init, factPath: 'kb/x.md', asOf: { mode: 'history' as const, commit: 'abc' } },
      { type: 'APPLY_NAV', view: 'library', factPath: 'kb/x.md', asOf: { mode: 'live' } },
    );
    expect(s.asOf).toEqual({ mode: 'live' });
    expect(selectAnchorCommit(s)).toBeNull();
    expect(s.factPath).toBe('kb/x.md');
  });

  it('APPLY_NAV with explicit filters clears non-path filters', () => {
    const s = { ...init, filters: [{ category: 'domain' as const, value: 'tech' }] };
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: null,
      asOf: { mode: 'history', commit: 'abc123' },
      filters: [],
      freeText: '',
    });
    expect(next.filters).toHaveLength(0);
  });

  it('APPLY_NAV without explicit filters preserves existing filters', () => {
    const s = { ...init, filters: [{ category: 'domain' as const, value: 'tech' }] };
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: null,
      asOf: { mode: 'history', commit: 'abc123' },
      // filters and freeText intentionally omitted
    });
    expect(next.filters).toHaveLength(1);
    expect(next.filters[0].value).toBe('tech');
  });
});


describe('reducer — NAV_BACK with new fields', () => {
  it('NAV_BACK restores asOf, factPath', () => {
    const s = { ...init, factPath: 'kb/f.md', asOf: { mode: 'history' as const, commit: 'abc' } };
    const sAfter = reducer(s, {
      type: 'APPLY_NAV', view: 'library',
      factPath: 'kb/g.md', asOf: { mode: 'history', commit: 'xyz' },
    });
    const back = reducer(sAfter, { type: 'NAV_BACK' });
    expect(back.asOf).toEqual({ mode: 'history', commit: 'abc' });
    expect(back.factPath).toBe('kb/f.md');
  });
});

describe('librarySort', () => {
  it('defaults to "recent" in init state', () => {
    expect(init.librarySort).toBe('recent');
  });

  it('SET_LIBRARY_SORT updates the stored value', () => {
    const next = reducer(init, { type: 'SET_LIBRARY_SORT', sort: 'path' });
    expect(next.librarySort).toBe('path');
  });

  it('SET_LIBRARY_SORT clears factPath so the right panel does not strand a stale selection', () => {
    const s: AppState = { ...init, factPath: 'kb/something.md', librarySort: 'recent' };
    const next = reducer(s, { type: 'SET_LIBRARY_SORT', sort: 'path' });
    expect(next.factPath).toBeNull();
    expect(next.librarySort).toBe('path');
  });
});

describe('notice', () => {
  it('SET_NOTICE sets text and CLEAR_NOTICE clears it', () => {
    const s1 = reducer(init, { type: 'SET_NOTICE', text: 'returned to now' });
    expect(s1.notice).toBe('returned to now');
    const s2 = reducer(s1, { type: 'CLEAR_NOTICE' });
    expect(s2.notice).toBe('');
  });
});

it('serverReadOnly forces isReadOnly even when live', () => {
  const live = init;
  expect(isReadOnly(live)).toBe(false);
  const ro = reducer(live, { type: 'SET_SERVER_READONLY', value: true });
  expect(ro.serverReadOnly).toBe(true);
  expect(isReadOnly(ro)).toBe(true);
});

function liveSelect(s: typeof init, path: string) {
  return reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: path, asOf: { mode: 'live' } });
}
function hop(s: typeof init, path: string, commit: string) {
  return reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: path, asOf: { mode: 'history', commit } });
}

describe('selectTrail', () => {
  it('a live view is a single crumb', () => {
    const s = liveSelect(init, 'kb/a.md');
    expect(selectTrail(s)).toEqual([{ factPath: 'kb/a.md', asOf: { mode: 'live' } }]);
  });
  it('hops build [liveRoot, ...hops, current]', () => {
    let s = liveSelect(init, 'kb/a.md');     // A live (current)
    s = hop(s, 'kb/b.md', 'bbb1111');         // hop A->B (pushes A live)
    s = hop(s, 'kb/c.md', 'ccc2222');         // hop B->C (pushes B history)
    expect(selectTrail(s)).toEqual([
      { factPath: 'kb/a.md', asOf: { mode: 'live' } },
      { factPath: 'kb/b.md', asOf: { mode: 'history', commit: 'bbb1111' } },
      { factPath: 'kb/c.md', asOf: { mode: 'history', commit: 'ccc2222' } },
    ]);
  });

  // Breadcrumb jump = unwind. Clicking crumb i pops (depth - i) entries via
  // NAV_BACK rather than pushing a new entry (the App onJumpTrail contract).
  it('NAV_BACK x (depth - i) jumps to crumb i without growing the trail', () => {
    let s = liveSelect(init, 'kb/a.md');
    s = hop(s, 'kb/b.md', 'bbb1111');
    s = hop(s, 'kb/c.md', 'ccc2222');         // trail [a,b,c], depth=2, current=c
    const back = (st: typeof init, n: number) => {
      for (let k = 0; k < n; k++) st = reducer(st, { type: 'NAV_BACK' });
      return st;
    };
    // jump to crumb 1 (b): depth - i = 2 - 1 = 1 back
    const atB = back(s, 1);
    expect(selectTrail(atB)).toEqual([
      { factPath: 'kb/a.md', asOf: { mode: 'live' } },
      { factPath: 'kb/b.md', asOf: { mode: 'history', commit: 'bbb1111' } },
    ]);
    // jump to crumb 0 (live root a): depth - i = 2 - 0 = 2 backs
    const atA = back(s, 2);
    expect(selectTrail(atA)).toEqual([{ factPath: 'kb/a.md', asOf: { mode: 'live' } }]);
  });
});
