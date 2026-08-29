import { describe, it, expect, vi } from 'vitest';
import { api } from './api';

// The motif filter on the wire.
//
// Every assertion here exists because of a failure mode the repo has already
// met once: `api.recent`'s own comment records two type chips collapsing to
// `undefined` and silently removing all type filtering. A motif CSV that
// collapses the same way would silently narrow a pivot from "every fact with
// this shape" to one motif, or drop the filter entirely — and the list would
// still render, still be plausible, and still be wrong.

function mockFetch() {
  const f = vi.fn().mockResolvedValue({
    ok: true, status: 200,
    json: async () => ({ count: 0, _embedded: { facts: [], results: [] }, facts: [], results: [] }),
  });
  globalThis.fetch = f as unknown as typeof fetch;
  return f;
}

const urlOf = (f: ReturnType<typeof vi.fn>, i = 0) => new URL(String(f.mock.calls[i][0]), 'http://x');
const paramOf = (f: ReturnType<typeof vi.fn>, name: string) => urlOf(f).searchParams.get(name);

describe('motif filter params', () => {
  describe('api.recent', () => {
    it('omits motifs entirely when none are set — not an empty string', () => {
      const f = mockFetch();
      api.recent('r', 'b', 'kb', '', 50, 0, { types: ['observation'] });
      expect(urlOf(f).searchParams.has('motifs')).toBe(false);
      expect(urlOf(f).searchParams.has('motif_match')).toBe(false);
      // The sibling filter still lands, so an assertion of absence cannot pass
      // by the whole opts object having been ignored.
      expect(paramOf(f, 'type')).toBe('observation');
    });

    it('joins two motifs into one comma-separated param', () => {
      const f = mockFetch();
      api.recent('r', 'b', 'kb', '', 50, 0, {
        motifs: ['bypass-defeats-guarantee', 'handle-outlives-target'],
      });
      expect(paramOf(f, 'motifs')).toBe('bypass-defeats-guarantee,handle-outlives-target');
    });

    it('sends motif_match only when the tier is not the default', () => {
      const exact = mockFetch();
      api.recent('r', 'b', 'kb', '', 50, 0, { motifs: ['m'], motifMatch: 'exact' });
      expect(urlOf(exact).searchParams.has('motif_match')).toBe(false);

      const wide = mockFetch();
      api.recent('r', 'b', 'kb', '', 50, 0, { motifs: ['m'], motifMatch: 'token-2' });
      expect(paramOf(wide, 'motif_match')).toBe('token-2');
    });
  });

  describe('api.search', () => {
    it('omits motifs when none are set, and keeps its siblings', () => {
      const f = mockFetch();
      api.search('r', 'b', 'query', '', 0, { types: ['policy'] });
      expect(urlOf(f).searchParams.has('motifs')).toBe(false);
      expect(paramOf(f, 'type')).toBe('policy');
    });

    it('joins two motifs into one comma-separated param', () => {
      const f = mockFetch();
      api.search('r', 'b', '', '', 0, { motifs: ['absence-encodes-value', 'check-then-act-race'] });
      expect(paramOf(f, 'motifs')).toBe('absence-encodes-value,check-then-act-race');
    });

    it('sends motif_match only when the tier is not the default', () => {
      const f = mockFetch();
      api.search('r', 'b', '', '', 0, { motifs: ['m'], motifMatch: 'stem' });
      expect(paramOf(f, 'motif_match')).toBe('stem');
    });
  });

  describe('lens twins', () => {
    it('listLensFacts joins two motifs and keeps the repo fan-out params', () => {
      const f = mockFetch();
      api.listLensFacts('dev', {
        repos: ['work', 'core'],
        motifs: ['failure-presents-as-success', 'absence-encodes-value'],
        motifMatch: 'token-2',
      });
      expect(paramOf(f, 'motifs')).toBe('failure-presents-as-success,absence-encodes-value');
      expect(paramOf(f, 'motif_match')).toBe('token-2');
      expect(urlOf(f).searchParams.getAll('repo')).toEqual(['work', 'core']);
    });

    it('lensSearch joins two motifs', () => {
      const f = mockFetch();
      api.lensSearch('dev', 'q', undefined, {
        motifs: ['point-in-time-resolution', 'handle-outlives-target'],
      });
      expect(paramOf(f, 'motifs')).toBe('point-in-time-resolution,handle-outlives-target');
      expect(urlOf(f).searchParams.has('motif_match')).toBe(false);
    });

    it('listLensFacts omits motifs when none are set', () => {
      const f = mockFetch();
      api.listLensFacts('dev', { path: 'kb' });
      expect(urlOf(f).searchParams.has('motifs')).toBe(false);
      expect(paramOf(f, 'path')).toBe('kb');
    });
  });
});
