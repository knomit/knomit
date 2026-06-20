import { describe, it, expect } from 'vitest';
import { reducer, init, currentPath, selectAnchorCommit, selectTrail } from './state';
import type { AppState, FilterChip } from './state';

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

  it('CONSOLE_LOG adds entry and caps at 500', () => {
    const next = reducer(init, { type: 'CONSOLE_LOG', level: 'info', message: 'hello' });
    expect(next.consoleEntries).toHaveLength(1);
    expect(next.consoleEntries[0].message).toBe('hello');
    expect(next.consoleEntries[0].level).toBe('info');
  });

  it('CONSOLE_LOG trims entries beyond 500', () => {
    const entries = Array.from({ length: 500 }, (_, i) => ({
      id: i, time: i, level: 'info' as const, message: `msg${i}`,
    }));
    const s = { ...init, consoleEntries: entries };
    const next = reducer(s, { type: 'CONSOLE_LOG', level: 'error', message: 'overflow' });
    expect(next.consoleEntries).toHaveLength(500);
    expect(next.consoleEntries[499].message).toBe('overflow');
    expect(next.consoleEntries[0].message).toBe('msg1');
  });

  it('CONSOLE_TOGGLE flips consoleOpen', () => {
    expect(reducer(init, { type: 'CONSOLE_TOGGLE' }).consoleOpen).toBe(true);
    const open = { ...init, consoleOpen: true };
    expect(reducer(open, { type: 'CONSOLE_TOGGLE' }).consoleOpen).toBe(false);
  });

  it('CONSOLE_SET_HEIGHT clamps between 80 and 600', () => {
    expect(reducer(init, { type: 'CONSOLE_SET_HEIGHT', height: 50 }).consoleHeight).toBe(80);
    expect(reducer(init, { type: 'CONSOLE_SET_HEIGHT', height: 300 }).consoleHeight).toBe(300);
    expect(reducer(init, { type: 'CONSOLE_SET_HEIGHT', height: 900 }).consoleHeight).toBe(600);
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
