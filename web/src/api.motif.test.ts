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

describe('the /motifs collection and cluster clients', () => {
  function jsonOnce(body: unknown) {
    const f = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => body });
    globalThis.fetch = f as unknown as typeof fetch;
    return f;
  }

  const COLLECTION = {
    count: 73,
    health: {
      authored_clusters: 73, authored_recurring: 37, authored_mints: 73,
      authored_links: 183, authored_epistemic_recurring: 29,
      recurrence_rate: 0.51, mint_to_link_ratio: 0.4,
    },
    _embedded: {
      motifs: [{
        cluster_key: 'as-failure-present-success',
        canonical: 'failure-presents-as-success',
        members: ['failure-presents-as-success'],
        df: 26,
        definition: 'An operation that did not achieve its effect returns the same signals a successful one would.',
        definition_state: 'current',
      }],
    },
  };

  it('lists with the default df order and unwraps count, health and entries', async () => {
    const f = jsonOnce(COLLECTION);
    const r = await api.motifs('knomit-kb', 'agent/test');
    // Branch names go through the house encoding (slash → colon), not percent
    // encoding — the same shape every other branch-scoped endpoint uses.
    expect(urlOf(f).pathname).toBe('/api/v1/repos/knomit-kb/branches/agent:test/motifs');
    expect(r.count).toBe(73);
    // Distinct values throughout, so a field read from the wrong key fails.
    expect(r.health.authored_recurring).toBe(37);
    expect(r.health.authored_links).toBe(183);
    expect(r.health.recurrence_rate).toBe(0.51);
    expect(r.motifs[0].canonical).toBe('failure-presents-as-success');
    expect(r.motifs[0].df).toBe(26);
    expect(r.motifs[0].definition_state).toBe('current');
  });

  it('passes q, sort and paging through', async () => {
    const f = jsonOnce(COLLECTION);
    await api.motifs('r', 'b', { q: 'signal', sort: 'name', limit: 50, offset: 100 });
    const p = urlOf(f).searchParams;
    expect(p.get('q')).toBe('signal');
    expect(p.get('sort')).toBe('name');
    expect(p.get('limit')).toBe('50');
    expect(p.get('offset')).toBe('100');
  });

  // `path` is scope: it decides which corpus the vocabulary is of, and the
  // server narrows the health block along with the list. An absent path sends
  // no param at all rather than an empty one — "the whole branch" is the
  // server's own default, not a value the client invents.
  it('sends the path scope, and omits it when there is none', async () => {
    const f = jsonOnce(COLLECTION);
    await api.motifs('r', 'b', { path: 'kb/decisions' });
    expect(urlOf(f).searchParams.get('path')).toBe('kb/decisions');

    const g = jsonOnce(COLLECTION);
    await api.motifs('r', 'b', {});
    expect(urlOf(g).searchParams.has('path')).toBe(false);
  });

  it('caps limit at the server maximum rather than sending a value it rejects', async () => {
    const f = jsonOnce(COLLECTION);
    await api.motifs('r', 'b', { limit: 5000 });
    expect(urlOf(f).searchParams.get('limit')).toBe('200');
  });

  it('falls back to an empty list when the embedded shape is missing', async () => {
    jsonOnce({ count: 0 });
    const r = await api.motifs('r', 'b');
    expect(r.motifs).toEqual([]);
  });

  it('reads one cluster by key, percent-encoding it', async () => {
    const f = jsonOnce({
      cluster_key: 'collapse-defection-restraint',
      canonical: 'defection-collapses-restraint',
      members: ['defection-collapses-restraint', 'restraint-without-reciprocity'],
      df: 6,
      carrier_count: 6,
      definition: 'A restraint only prevents the outcome if all parties keep it.',
      definition_state: 'stale',
      carriers: [{ path: 'kb/x/y/z.md', title: 'T', type: 'observation', committed_at: 7 }],
      aliases: [
        { motif: 'defection-collapses-restraint', method: 'canonical' },
        { motif: 'restraint-without-reciprocity', method: 'judge', rationale: 'Both name the same collapse.' },
      ],
    });
    const c = await api.motifCluster('r', 'b', 'a/b key');
    expect(urlOf(f).pathname).toBe('/api/v1/repos/r/branches/b/motifs/a%2Fb%20key');
    // carrier_count and df are different numbers with different meanings; the
    // fixture keeps them equal in the wild but the client must not conflate the
    // fields, so the assertions name each one.
    expect(c.carrier_count).toBe(6);
    expect(c.df).toBe(6);
    expect(c.definition_state).toBe('stale');
    expect(c.aliases).toHaveLength(2);
    expect(c.aliases[1].method).toBe('judge');
    expect(c.aliases[1].rationale).toBe('Both name the same collapse.');
    expect(c.carriers[0].title).toBe('T');
  });

  it('tolerates a cluster with no carriers or aliases', async () => {
    jsonOnce({ cluster_key: 'k', canonical: 'c', members: ['c'], df: 1, carrier_count: 1 });
    const c = await api.motifCluster('r', 'b', 'k');
    expect(c.carriers).toEqual([]);
    expect(c.aliases).toEqual([]);
    expect(c.definition).toBeUndefined();
  });
});
