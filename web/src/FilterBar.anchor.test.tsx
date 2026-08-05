// Task 17: `at:`/`vs:` anchor tokens are per-fact in a lens. In a lens context
// with NO open fact there is no mount to anchor against, so dropping into
// history would strand the left panel on nothing — the FilterBar warns (existing
// warnings channel) and does NOT dispatch SET_AS_OF. With an open fact the
// anchor resolves as usual. Repo context is byte-identical to before.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/react';
import { FilterBar } from './FilterBar';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';

vi.mock('./api', async () => {
  const actual = await vi.importActual<typeof import('./api')>('./api');
  return {
    ...actual,
    api: {
      completions: vi.fn().mockResolvedValue({ values: [] }),
      lensCompletions: vi.fn().mockResolvedValue({ values: [] }),
    },
  };
});

const lens: Lens = { name: 'eng', write: 'core', reads: [{ repo: 'core' }, { repo: 'docs' }] };

function lensState(overrides: Partial<AppState> = {}): AppState {
  return {
    ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa',
    context: { kind: 'lens', name: 'eng' }, lens, ...overrides,
  };
}
function repoState(overrides: Partial<AppState> = {}): AppState {
  return { ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa', ...overrides };
}

function typeAndEnter(value: string) {
  const input = document.getElementById('filter-input') as HTMLInputElement;
  fireEvent.change(input, { target: { value } });
  fireEvent.keyDown(input, { key: 'Enter' });
}

describe('FilterBar — at:/vs: anchor is per-fact in a lens', () => {
  beforeEach(() => vi.clearAllMocks());

  it('lens context, NO open fact: warns and does NOT dispatch SET_AS_OF', () => {
    const dispatch = vi.fn();
    render(<FilterBar state={lensState({ factPath: null })} dispatch={dispatch} />);
    typeAndEnter('at:b812d40');
    const actions = dispatch.mock.calls.map(c => c[0]);
    expect(actions.some((a: any) => a.type === 'SET_AS_OF')).toBe(false);
    // The warning is addressed to the person who just typed the anchor, so it
    // goes to the notice banner — the app's one user-facing message surface.
    expect(actions.some((a: any) =>
      a.type === 'SET_NOTICE' && /Open a fact first — time anchors are per-fact in a lens/.test(a.text))).toBe(true);
  });

  it('lens context, open fact: dispatches SET_AS_OF (per-fact anchor resolves)', () => {
    const dispatch = vi.fn();
    render(<FilterBar state={lensState({ factPath: 'kb://docsid123456/kb/a.md' })} dispatch={dispatch} />);
    typeAndEnter('at:b812d40');
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'b812d40' } });
  });

  it('repo context: at: dispatches SET_AS_OF even with no open fact (byte-identical)', () => {
    const dispatch = vi.fn();
    render(<FilterBar state={repoState({ factPath: null })} dispatch={dispatch} />);
    typeAndEnter('at:b812d40');
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'b812d40' } });
  });
});
