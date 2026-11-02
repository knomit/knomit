// Q-UI14: the FactSet view lives in the right panel, replacing the fact view
// as the single-fact view does, and Back returns to the fact.
//
// Sabotage: mount the FactSet view only when a fact's materialize view is open
// (the factset-view never appears over the fact).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';
import { factsetView } from './factsetOptions';

const { first, close } = vi.hoisted(() => ({ close: vi.fn(async () => {}), first: vi.fn(async () => ({
  mode: 'factset', title: 'Facts in U', search_line: 'path kb/u/ · sorted by recent', anchor: 'a', unpinned: false, cross_kb: false,
  depth: 4, max_nodes: 200, facts_limit: 500, units: 1, unit_from: 1, unit_to: 1, cursor: 'ID.2.0', has_more: false,
  facts: [{ n: 1, path: 'kb/u/a.md', repo: 'r', commit: 'c', depth: 0, role: 'root', title: 'A', type: 'observation', kind: 'epistemic',
    confidence: 0.7, anchor_id: 'f-a', unit: 1, number: '1', tree_depth: 0, written: true, body: 'a.' }],
})) }));
vi.mock('./factsetApi', async importOriginal => ({
  ...(await importOriginal<typeof import('./factsetApi')>()),
  factsetApi: { first, next: vi.fn(), seek: vi.fn(), exportMarkdown: vi.fn(), close },
}));

vi.mock('./api', () => ({
  api: {
    fact: vi.fn().mockResolvedValue({
      path: 'kb/test/foo.md', title: 'Foo', body: 'body', type: 'observation', confidence: 0.9, sources: 1,
      domain: [], entities: [], refs: [], commit_hash: 'aaa1111', commit_date: '2026-05-01T00:00:00Z',
    }),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    commitDetail: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
    explain: vi.fn().mockResolvedValue({ incoming: [], outgoing: [] }),
  },
}));

const state: AppState = { ...init, repo: 'knomit', branch: 'machine/test', factPath: 'kb/test/foo.md', asOf: { mode: 'live' } };

afterEach(() => { act(() => factsetView.close()); window.history.replaceState(null, '', '/'); delete document.documentElement.dataset.matFocus; });

describe('the FactSet view in the right panel', () => {
  it('replaces the fact view when a list opens it, and Back returns to the fact', async () => {
    render(<RightPanel state={state} dispatch={vi.fn()} />);
    expect(await screen.findByTestId('retract-btn')).toBeInTheDocument();
    act(() => factsetView.open({ target: { kind: 'repo', repo: 'knomit', branch: 'machine/test' }, sort: 'recent', params: 'path=kb%2Fu' }));
    expect(await screen.findByTestId('factset-view')).toBeInTheDocument();
    expect(screen.queryByTestId('retract-btn')).toBeNull();
    await waitFor(() => expect(screen.getAllByTestId('mat-section')).toHaveLength(1));
    expect(first).toHaveBeenCalledTimes(1);
    expect(window.location.search).toBe('?factset=1');
    fireEvent.click(screen.getByTestId('fs-back'));
    expect(await screen.findByTestId('retract-btn')).toBeInTheDocument();
    expect(screen.queryByTestId('factset-view')).toBeNull();
    expect(window.location.search).toBe('');
  });
});

// User ruling 2026-11-02 (#710 M2): the FactSet view exists only in Focus.
// Leaving Focus, by the toggle or by Esc, IS closing the view: back to the
// fact, the FactSet closed (DELETE with its cursor), the URL flag gone. Esc in
// a text or number field only leaves the field (3fedfce6).
//
// Sabotage, one run each: the toggle turns Focus off instead of closing (the
// view stays); Esc turns Focus off instead of closing (the view stays, no
// DELETE); a text field does not hold Esc (the view closes).
describe('leaving Focus closes the FactSet (#710 M2)', () => {
  const open = async () => {
    render(<RightPanel state={state} dispatch={vi.fn()} />);
    await screen.findByTestId('retract-btn');
    act(() => factsetView.open({ target: { kind: 'repo', repo: 'knomit', branch: 'machine/test' }, sort: 'recent', params: 'path=kb%2Fu' }));
    await waitFor(() => expect(screen.getAllByTestId('mat-section')).toHaveLength(1));
    close.mockClear();
  };

  it('the Exit focus toggle closes the view, closes the FactSet and clears the URL', async () => {
    await open();
    const toggle = screen.getByTestId('fs-focus');
    expect(toggle).toHaveAttribute('title', 'Exit focus');
    fireEvent.click(toggle);
    expect(await screen.findByTestId('retract-btn')).toBeInTheDocument();
    expect(screen.queryByTestId('factset-view')).toBeNull();
    expect(close).toHaveBeenCalledWith({ kind: 'repo', repo: 'knomit', branch: 'machine/test' }, 'ID.2.0');
    expect(window.location.search).toBe('');
    expect(document.documentElement.dataset.matFocus).toBeUndefined();
  });

  it('Esc closes the view and issues the DELETE', async () => {
    await open();
    (document.activeElement as HTMLElement | null)?.blur();
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(await screen.findByTestId('retract-btn')).toBeInTheDocument();
    expect(screen.queryByTestId('factset-view')).toBeNull();
    expect(close).toHaveBeenCalledTimes(1);
    expect(close).toHaveBeenCalledWith(expect.objectContaining({ kind: 'repo' }), 'ID.2.0');
    expect(window.location.search).toBe('');
  });

  it('Esc in a text field only leaves the field: the view stays open, nothing is closed', async () => {
    await open();
    const depth = screen.getByTestId('fs-depth');
    depth.focus();
    expect(document.activeElement).toBe(depth);
    fireEvent.keyDown(depth, { key: 'Escape' });
    expect(document.activeElement).not.toBe(depth);
    expect(screen.getByTestId('factset-view')).toBeInTheDocument();
    expect(close).not.toHaveBeenCalled();
    expect(window.location.search).toBe('?factset=1');
  });
});
