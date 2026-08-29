// Motif chips become the `motifs` opt on the list request.
//
// The pivot is a filtered fact list: the chip in the bar and the rows on screen
// are the same query. So the one thing that must not go wrong here is a chip
// that renders but never reaches the wire — the reader would see the pivot,
// see a list, and be looking at an unfiltered corpus.
//
// Two motif chips WIDEN (the server reads one CSV with splitCSV). That makes
// the two-chip case the interesting one twice over: it is both the semantics
// and the shape a multi-value param has collapsed in before.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, waitFor } from '@testing-library/react';
import { Library } from './Library';
import { api } from './api';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    browse: vi.fn(async () => ({ children: [] })),
    recent: vi.fn(async () => ({
      facts: [
        { path: 'kb/gotchas/store/testing/z.md', title: 'A', type: 'observation', committed_at: 2 },
        { path: 'kb/gotchas/build/web/y.md', title: 'B', type: 'policy', committed_at: 1 },
      ],
      total: 26,
    })),
    search: vi.fn(async () => []),
    // A single motif chip makes the list a PIVOT, and the header then resolves
    // that motif to name it. Two chips is a union of two shapes with no single
    // name, so it stays an ordinary filtered list and never asks.
    motifCluster: vi.fn(async (_r: string, _b: string, key: string) => ({
      cluster_key: `key-${key}`, canonical: key, members: [key],
      df: 26, carrier_count: 26, carriers: [], aliases: [],
      definition: 'An operation that did not achieve its effect returns the same signals a successful one would.',
      definition_state: 'current',
    })),
    lensBrowse: vi.fn(async () => ({ children: [] })),
    listLensFacts: vi.fn(async () => ({ facts: [], total: 0 })),
    lensSearch: vi.fn(async () => []),
    listLensFactsSearch: vi.fn(async () => ({ facts: [], total: 0 })),
  },
}));

const repoState = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test', librarySort: 'recent',
  context: { kind: 'repo', repo: 'knomit-kb' }, ...over,
});

const lensState = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test', librarySort: 'recent',
  lens: { name: 'dev', write: { uid: 'u', name: 'knomit-kb' }, reads: [] } as AppState['lens'],
  context: { kind: 'lens', name: 'dev' }, ...over,
});

const optsOf = (fn: unknown, argIndex: number) =>
  (fn as ReturnType<typeof vi.fn>).mock.calls[0][argIndex] as Record<string, unknown>;

describe('motif chips reach the list request', () => {
  beforeEach(() => vi.clearAllMocks());

  it('sends both motif chips, and does not disturb the other categories', async () => {
    render(<Library dispatch={vi.fn()} navigate={vi.fn()} state={repoState({
      filters: [
        { category: 'motif', value: 'bypass-defeats-guarantee' },
        { category: 'domain', value: 'store' },
        { category: 'motif', value: 'handle-outlives-target' },
      ],
    })} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalled());
    const opts = optsOf(api.recent, 6);
    // Pairwise-distinct buckets: a motif landing in `domains` (or vice versa)
    // fails rather than passing on a shared value.
    expect(opts.motifs).toEqual(['bypass-defeats-guarantee', 'handle-outlives-target']);
    expect(opts.domains).toEqual(['store']);
  });

  it('omits motifs entirely when no motif chip is set', async () => {
    render(<Library dispatch={vi.fn()} navigate={vi.fn()} state={repoState({
      filters: [{ category: 'domain', value: 'store' }],
    })} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalled());
    const opts = optsOf(api.recent, 6);
    expect(opts.motifs).toBeUndefined();
    expect(opts.domains).toEqual(['store']);
  });

  it('carries the match tier from state, not from the chips', async () => {
    render(<Library dispatch={vi.fn()} navigate={vi.fn()} state={repoState({
      filters: [{ category: 'motif', value: 'failure-presents-as-success' }],
      motifMatch: 'token-2',
    })} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalled());
    expect(optsOf(api.recent, 6).motifMatch).toBe('token-2');
  });

  it('reaches the lens list endpoint too', async () => {
    render(<Library dispatch={vi.fn()} navigate={vi.fn()} state={lensState({
      filters: [{ category: 'motif', value: 'absence-encodes-value' }],
    })} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalled());
    const opts = optsOf(api.listLensFacts, 1);
    expect(opts.motifs).toEqual(['absence-encodes-value']);
  });

  it('two renders against one mock build independent arrays', async () => {
    // The two-requests-one-stub shape: the memo derives arrays from state, and
    // an implementation that filtered a shared array in place would leave the
    // second render reading the first one's leftovers.
    const filters: AppState['filters'] = [
      { category: 'motif', value: 'check-then-act-race' },
      { category: 'motif', value: 'point-in-time-resolution' },
    ];
    const first = render(<Library dispatch={vi.fn()} navigate={vi.fn()} state={repoState({ filters })} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalledTimes(1));
    first.unmount();
    render(<Library dispatch={vi.fn()} navigate={vi.fn()} state={repoState({ filters })} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalledTimes(2));

    const a = (api.recent as ReturnType<typeof vi.fn>).mock.calls[0][6] as Record<string, unknown>;
    const b = (api.recent as ReturnType<typeof vi.fn>).mock.calls[1][6] as Record<string, unknown>;
    expect(a.motifs).toEqual(['check-then-act-race', 'point-in-time-resolution']);
    expect(b.motifs).toEqual(['check-then-act-race', 'point-in-time-resolution']);
    expect(a.motifs).not.toBe(b.motifs);
    // And the caller's own array is untouched — the memo reads state.filters,
    // it does not get to rewrite it.
    expect(filters).toHaveLength(2);
  });
});
