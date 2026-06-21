import { describe, it, expect, vi } from 'vitest';
import { resolveHopAnchor, computeReturnToNow } from './useTimeTravel';
import { planTrailHop } from './state';
import type { Fact } from './api';
import type { TrailCrumb } from './state';

const crumb = (factPath: string): TrailCrumb => ({ factPath, asOf: { mode: 'live' } });

describe('planTrailHop', () => {
  it('pushes when the target is not already in the trail', () => {
    const trail = [crumb('kb/a.md'), crumb('kb/b.md')];
    expect(planTrailHop(trail, 'kb/c.md')).toEqual({ kind: 'push' });
  });
  it('unwinds to an earlier crumb instead of re-pushing (collapses A>B>A cycles)', () => {
    // Viewing B (depth 1) and hopping back to A (index 0) unwinds one step.
    const trail = [crumb('kb/a.md'), crumb('kb/b.md')];
    expect(planTrailHop(trail, 'kb/a.md')).toEqual({ kind: 'unwind', steps: 1 });
  });
  it('unwinds the full distance for a deeper revisit', () => {
    const trail = [crumb('kb/a.md'), crumb('kb/b.md'), crumb('kb/c.md')];
    expect(planTrailHop(trail, 'kb/a.md')).toEqual({ kind: 'unwind', steps: 2 });
  });
  it('is a no-op (unwind 0) when the target is already the current crumb', () => {
    const trail = [crumb('kb/a.md'), crumb('kb/b.md')];
    expect(planTrailHop(trail, 'kb/b.md')).toEqual({ kind: 'unwind', steps: 0 });
  });
});

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
  it('target superseded -> history at pinned', async () => {
    const fact = vi.fn(async (_r, _b, _p, commit?: string) =>
      mkFact(commit ? 'pin111' : 'head777')); // HEAD read returns head777
    const r = await resolveHopAnchor('r', 'b', 'kb/b.md', 'pin111', { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'history', commit: 'pin111' });
    expect(r.fact?.commit_hash).toBe('pin111');
  });
  it('target retracted (HEAD 404) -> history at pinned via fallback', async () => {
    const fact = vi.fn(async (_r, _b, _p, commit?: string) => {
      if (!commit) throw new Error('404'); // HEAD read 404s
      return mkFact('pin111');
    });
    const r = await resolveHopAnchor('r', 'b', 'kb/b.md', 'pin111', { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'history', commit: 'pin111' });
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
