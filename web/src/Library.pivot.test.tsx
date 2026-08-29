// The landed pivot: what a reader sees after opening a motif's carriers.
//
// The list must not read as a search or as a folder. It is every fact in the
// corpus with one shape, and the two things that say so are the heading — which
// names the shape and what it means instead of a location — and the rows, which
// name what each fact is ABOUT so the variety is readable as you scan.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';

const CARRIERS = [
  { path: 'kb/gotchas/store/testing/a.md', title: 'Limit 0 discards every row', type: 'observation', committed_at: 5 },
  { path: 'kb/invariants/build/b.md', title: 'Verify before the atomic rename', type: 'principle', committed_at: 4 },
  { path: 'kb/gotchas/desktop/c.md', title: 'Unsigned app refuses banners', type: 'observation', committed_at: 3 },
  { path: 'kb/meta/methodology/d.md', title: 'A regression test never seen red', type: 'methodology', committed_at: 2 },
  { path: 'kb/gotchas/store/e.md', title: 'Schema drift is invisible', type: 'observation', committed_at: 1 },
];

vi.mock('./api', () => ({
  api: {
    browse: vi.fn(async () => ({ children: [] })),
    recent: vi.fn(async () => ({ facts: CARRIERS, total: 26 })),
    search: vi.fn(async () => []),
    motifCluster: vi.fn(async (_r: string, _b: string, key: string) => ({
      cluster_key: 'as-failure-present-success',
      // The spelling the fact used is not always the one the corpus reads by.
      canonical: key === 'presents-as-success' ? 'failure-presents-as-success' : key,
      members: [key], df: 26, carrier_count: 26, carriers: [], aliases: [],
      definition: 'An operation that did not achieve its effect returns the same signals a successful one would.',
      definition_state: 'current',
    })),
    lensBrowse: vi.fn(async () => ({ children: [] })),
    listLensFacts: vi.fn(async () => ({ facts: [], total: 0 })),
    lensSearch: vi.fn(async () => []),
    listLensFactsSearch: vi.fn(async () => ({ facts: [], total: 0 })),
  },
}));

const pivoted = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test', librarySort: 'recent',
  context: { kind: 'repo', repo: 'knomit-kb' },
  filters: [{ category: 'motif', value: 'failure-presents-as-success' }],
  ...over,
});

const draw = (state: AppState) =>
  render(<Library state={state} dispatch={vi.fn()} navigate={vi.fn()} />);

describe('the landed pivot', () => {
  beforeEach(() => vi.clearAllMocks());

  it('names the shape where a folder would be, and says what it means', async () => {
    draw(pivoted());
    await waitFor(() => expect(screen.getByTestId('library-motif')).toBeTruthy());
    expect(screen.getByTestId('library-leaf').textContent).toBe('failure-presents-as-success');
    expect(screen.getByTestId('library-motif')).toHaveTextContent('same motif as');
    await waitFor(() => expect(screen.getByTestId('library-motif-definition').textContent)
      .toContain('same signals a successful one would'));
    // Not a location: the ancestors line is gone, because a motif cuts across
    // the ontology and there is no folder to be in.
    expect(screen.queryByTestId('library-ancestors')).toBeNull();
  });

  it('shows the corpus’s name for the motif, not the spelling the chip carries', async () => {
    draw(pivoted({ filters: [{ category: 'motif', value: 'presents-as-success' }] }));
    await waitFor(() => expect(screen.getByTestId('library-leaf').textContent)
      .toBe('failure-presents-as-success'));
  });

  it('counts carriers, not the page', async () => {
    draw(pivoted());
    await waitFor(() => expect(screen.getByTestId('library-count').textContent).toBe('26'));
  });

  it('names the areas the carriers are about, from the rows themselves', async () => {
    draw(pivoted());
    await waitFor(() => expect(screen.getByTestId('library-motif-subjects')).toBeTruthy());
    const line = screen.getByTestId('library-motif-subjects').textContent!;
    // store leads on two rows; build, desktop and methodology have one each.
    // Whole names, most-carried first, and a count for what was left out.
    expect(line).toContain('across store');
    expect(line).toContain('+1 more');
    expect(line).not.toContain('…');
  });

  it('gives every row its own subject, so the variety reads as you scan', async () => {
    draw(pivoted());
    await waitFor(() => expect(screen.getAllByTestId('chrono-subject').length).toBe(5));
    const subjects = screen.getAllByTestId('chrono-subject').map(e => e.textContent);
    // Path segment 3 — deliberately not the domain field.
    expect(subjects).toEqual(['store', 'build', 'desktop', 'methodology', 'store']);
  });

  it('stays an ordinary list when two motifs are combined', async () => {
    // Two chips is a union of two shapes and has no single name; there is
    // nothing honest for the heading to call it.
    draw(pivoted({ filters: [
      { category: 'motif', value: 'a' },
      { category: 'motif', value: 'b' },
    ] }));
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(5));
    expect(screen.queryByTestId('library-motif')).toBeNull();
    expect(screen.queryAllByTestId('chrono-subject')).toHaveLength(0);
  });

  it('stays an ordinary list when free text is also set', async () => {
    // Text makes it a search that happens to be motif-filtered — a different
    // question, and one the pivot heading would misdescribe.
    draw(pivoted({ freeText: 'sort' }));
    await waitFor(() => expect(screen.queryByTestId('library-motif')).toBeNull());
  });
});
