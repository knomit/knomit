// Resolving a fact's motif names into clusters.
//
// The names are free — they arrive on the fact itself — but the count beside
// each one is a request per motif. So the cell has three answers to tell apart,
// and the one that matters is the third: a count that could not be fetched must
// never render as `0`. Zero would say "nothing else in this corpus has this
// shape", which is both false and unfalsifiable from the reader's chair. This
// is the same rule ConnectionsCell already follows for edge counts, recorded in
// kb/decisions/ui/connections/header-cells.

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, waitFor } from '@testing-library/react';
import { useMotifClusters } from './useMotifClusters';
import { api } from './api';

vi.mock('./api', () => ({ api: { motifCluster: vi.fn(), lensMotifCluster: vi.fn() } }));

// The endpoint the hook reads from. A repo branch here; the lens twin below
// asserts the same hook against /lenses/{lens}/motifs/{key}.
const REPO = { kind: 'repo', repo: 'r', branch: 'b' } as const;

const cluster = (canonical: string, carrier_count: number) => ({
  cluster_key: `key-${canonical}`, canonical, members: [canonical],
  // Distinct, so a consumer reading df where it means carrier_count fails.
  df: carrier_count + 100, carrier_count, carriers: [], aliases: [],
});

describe('useMotifClusters', () => {
  beforeEach(() => vi.clearAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it('reports every name as loading before any response lands', () => {
    (api.motifCluster as ReturnType<typeof vi.fn>).mockReturnValue(new Promise(() => {}));
    const { result } = renderHook(() =>
      useMotifClusters(REPO, ['failure-presents-as-success', 'absence-encodes-value']));
    // The names are already known, so the caller can draw them immediately —
    // the whole reason the count is allowed to arrive late.
    expect(result.current.map(m => m.motif))
      .toEqual(['failure-presents-as-success', 'absence-encodes-value']);
    expect(result.current.every(m => m.status === 'loading')).toBe(true);
  });

  it('resolves each name to its own cluster', async () => {
    (api.motifCluster as ReturnType<typeof vi.fn>).mockImplementation(async (_r, _b, key: string) =>
      cluster(key, key === 'failure-presents-as-success' ? 26 : 7));
    const { result } = renderHook(() =>
      useMotifClusters(REPO, ['failure-presents-as-success', 'absence-encodes-value']));
    await waitFor(() => expect(result.current.every(m => m.status === 'ok')).toBe(true));
    // Distinct counts, matched by name: a hook that resolved both entries from
    // one response would pass with equal fixtures and fail here.
    const byName = Object.fromEntries(result.current.map(m => [m.motif, m]));
    expect(byName['failure-presents-as-success'].cluster?.carrier_count).toBe(26);
    expect(byName['absence-encodes-value'].cluster?.carrier_count).toBe(7);
  });

  it('marks a failed fetch as an error, never as a zero', async () => {
    (api.motifCluster as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('502'));
    const { result } = renderHook(() => useMotifClusters(REPO, ['handle-outlives-target']));
    await waitFor(() => expect(result.current[0].status).toBe('error'));
    // The name survives the failure — it came from the fact, not the request —
    // and there is no count at all, rather than a zero standing in for one.
    expect(result.current[0].motif).toBe('handle-outlives-target');
    expect(result.current[0].cluster).toBeUndefined();
  });

  it('keeps one failure from hiding its neighbour', async () => {
    (api.motifCluster as ReturnType<typeof vi.fn>).mockImplementation(async (_r, _b, key: string) => {
      if (key === 'bad-one') throw new Error('boom');
      return cluster(key, 9);
    });
    const { result } = renderHook(() => useMotifClusters(REPO, ['bad-one', 'good-one']));
    await waitFor(() => expect(result.current.every(m => m.status !== 'loading')).toBe(true));
    expect(result.current[0].status).toBe('error');
    expect(result.current[1].status).toBe('ok');
    expect(result.current[1].cluster?.carrier_count).toBe(9);
  });

  it('asks once per motif, and not again on re-render', async () => {
    (api.motifCluster as ReturnType<typeof vi.fn>).mockImplementation(async (_r, _b, k: string) => cluster(k, 3));
    const motifs = ['a', 'b'];
    const { result, rerender } = renderHook(() => useMotifClusters(REPO, motifs));
    await waitFor(() => expect(result.current.every(m => m.status === 'ok')).toBe(true));
    rerender();
    rerender();
    // Three per fact is the ceiling the API allows; three per RENDER would be a
    // request storm behind a header that looks perfectly still.
    expect(api.motifCluster).toHaveBeenCalledTimes(2);
  });

  it('refetches when the fact changes, and drops the previous fact’s answers', async () => {
    (api.motifCluster as ReturnType<typeof vi.fn>).mockImplementation(async (_r, _b, k: string) =>
      cluster(k, k === 'first' ? 11 : 22));
    let motifs = ['first'];
    const { result, rerender } = renderHook(() => useMotifClusters(REPO, motifs));
    await waitFor(() => expect(result.current[0].status).toBe('ok'));

    motifs = ['second'];
    rerender();
    await waitFor(() => expect(result.current[0].status).toBe('ok'));
    expect(result.current).toHaveLength(1);
    expect(result.current[0].motif).toBe('second');
    expect(result.current[0].cluster?.carrier_count).toBe(22);
  });

  it('asks for nothing when the fact carries no motifs', () => {
    const { result } = renderHook(() => useMotifClusters(REPO, undefined));
    expect(result.current).toEqual([]);
    expect(api.motifCluster).not.toHaveBeenCalled();
  });

  it('does not fetch before there is an endpoint to ask', () => {
    const { result } = renderHook(() =>
      useMotifClusters({ kind: 'repo', repo: '', branch: '' }, ['a']));
    // A request against an empty repo is a guaranteed 404 that would render as
    // a failed count on a fact whose motifs are perfectly fine.
    expect(api.motifCluster).not.toHaveBeenCalled();
    expect(result.current[0].status).toBe('loading');
  });

  // In a lens the count must come from the LENS, not from whichever mount the
  // fact lives on: the count sits beside a pivot and the pivot lists the lens.
  // The repo endpoint must not be touched at all — a fallback that silently
  // asked the write repo would answer with a smaller, plausible number.
  it('resolves through the lens endpoint in a lens context', async () => {
    (api.lensMotifCluster as ReturnType<typeof vi.fn>)
      .mockImplementation(async (_lens: string, key: string) => cluster(key, 26));
    const { result } = renderHook(() =>
      useMotifClusters({ kind: 'lens', lens: 'eng' }, ['failure-presents-as-success']));
    await waitFor(() => expect(result.current[0].status).toBe('ok'));
    expect(api.lensMotifCluster).toHaveBeenCalledWith('eng', 'failure-presents-as-success');
    expect(api.motifCluster).not.toHaveBeenCalled();
    expect(result.current[0].cluster?.carrier_count).toBe(26);
  });

  it('does not fetch before a lens name is known', () => {
    renderHook(() => useMotifClusters({ kind: 'lens', lens: '' }, ['a']));
    expect(api.lensMotifCluster).not.toHaveBeenCalled();
  });

  // Switching context re-resolves: the same names against a different corpus
  // are different counts, and holding the previous ones would attribute one
  // corpus's numbers to another.
  it('refetches when the endpoint changes under the same names', async () => {
    (api.motifCluster as ReturnType<typeof vi.fn>)
      .mockImplementation(async (_r: string, _b: string, k: string) => cluster(k, 3));
    (api.lensMotifCluster as ReturnType<typeof vi.fn>)
      .mockImplementation(async (_l: string, k: string) => cluster(k, 9));
    let endpoint: Parameters<typeof useMotifClusters>[0] = REPO;
    const { result, rerender } = renderHook(() => useMotifClusters(endpoint, ['shared-name']));
    await waitFor(() => expect(result.current[0].cluster?.carrier_count).toBe(3));
    endpoint = { kind: 'lens', lens: 'eng' };
    rerender();
    await waitFor(() => expect(result.current[0].cluster?.carrier_count).toBe(9));
  });
});
