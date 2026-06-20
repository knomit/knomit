import { describe, it, expect, vi } from 'vitest';
import { resolveHopAnchor, computeReturnToNow } from './useTimeTravel';
import type { Fact } from './api';

const mkFact = (commit_hash: string): Fact => ({
  path: 'kb/b.md', title: 'B', body: '', domain: [], confidence: 0, sources: 0,
  entities: [], refs: [], commit_hash,
});

describe('resolveHopAnchor', () => {
  it('target current -> live', async () => {
    const fact = vi.fn(async (_r, _b, _p, commit?: string) => mkFact(commit ?? 'head777'));
    const r = await resolveHopAnchor('r', 'b', 'kb/b.md', 'head777', { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'live' });
  });
  it('target superseded -> scrubbed at pinned', async () => {
    const fact = vi.fn(async (_r, _b, _p, commit?: string) =>
      mkFact(commit ? 'pin111' : 'head777')); // HEAD read returns head777
    const r = await resolveHopAnchor('r', 'b', 'kb/b.md', 'pin111', { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'scrubbed', commit: 'pin111' });
    expect(r.fact?.commit_hash).toBe('pin111');
  });
  it('target retracted (HEAD 404) -> scrubbed at pinned via fallback', async () => {
    const fact = vi.fn(async (_r, _b, _p, commit?: string) => {
      if (!commit) throw new Error('404'); // HEAD read 404s
      return mkFact('pin111');
    });
    const r = await resolveHopAnchor('r', 'b', 'kb/b.md', 'pin111', { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'scrubbed', commit: 'pin111' });
  });
});

describe('computeReturnToNow', () => {
  it('subject present at HEAD -> stays subject, live', async () => {
    const fact = vi.fn(async () => mkFact('head1'));
    const r = await computeReturnToNow('r', 'b', 'kb/x/y.md', { fact: fact as any });
    expect(r).toEqual({ kind: 'subject', factPath: 'kb/x/y.md' });
  });
  it('subject retracted -> parent folder + notice', async () => {
    const fact = vi.fn(async () => { throw new Error('404'); });
    const r = await computeReturnToNow('r', 'b', 'kb/x/y.md', { fact: fact as any });
    expect(r).toEqual({
      kind: 'parent',
      parentPath: 'kb/x',
      notice: '"kb/x/y.md" was retracted — no live version. Returned to now.',
    });
  });
});
