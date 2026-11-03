import { describe, it, expect, vi, beforeEach } from 'vitest';
import { api, parseFilterQuery, isCommitNotInRepo, isNotFound } from './api';
import type { HttpError } from './api';

// Regression: most api.* functions used to call `.then(r => r.json())` without
// checking r.ok, silently parsing problem+json bodies as success and returning
// empty results from backend 500s. The centralized fetchJSON helper now throws
// on any non-2xx — these tests pin the new contract.
describe('api.* — non-2xx surfaces as a rejected promise', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  function mock500(detail = 'boom') {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
      json: async () => ({ detail }),
    } as unknown as Response);
  }

  it('factCommits rejects on 500 (not silently returns empty list)', async () => {
    mock500('history blew up');
    await expect(api.factCommits('r', 'b', 'kb/x.md')).rejects.toThrow(/500/);
  });

  it('commitDetail rejects on 500 (not silently returns null/empty)', async () => {
    mock500();
    await expect(api.commitDetail('r', 'b', 'abc1234')).rejects.toThrow(/500/);
  });

  it('stats rejects on 500', async () => {
    mock500();
    await expect(api.stats('r', 'b', 'kb')).rejects.toThrow(/500/);
  });

  it('activity rejects on 500', async () => {
    mock500();
    await expect(api.activity('r', 'b', 'kb')).rejects.toThrow(/500/);
  });

  it('status rejects on 500', async () => {
    mock500();
    await expect(api.status('r', 'b')).rejects.toThrow(/500/);
  });

  it('browse rejects on 500', async () => {
    mock500();
    await expect(api.browse('r', 'b', 'kb', 'kb')).rejects.toThrow(/500/);
  });

  it('search rejects on 500', async () => {
    mock500();
    await expect(api.search('r', 'b', 'q')).rejects.toThrow(/500/);
  });

  it('rebuild rejects on 500 (callers must handle the rejection)', async () => {
    mock500();
    await expect(api.rebuild('r', 'b')).rejects.toThrow(/500/);
  });

  it('error message includes the problem+json detail when present', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
      json: async () => ({ detail: 'specific reason from server' }),
    } as unknown as Response);
    await expect(api.stats('r', 'b', 'kb')).rejects.toThrow(/specific reason from server/);
  });
});

describe('parseFilterQuery — malformed at:/vs: tokens surface warnings', () => {
  it('invalid at: SHA produces a warning instead of silent drop', () => {
    const r = parseFilterQuery('at:notasha');
    expect(r.asOf).toBeUndefined();
    expect(r.warnings).toHaveLength(1);
    expect(r.warnings[0]).toMatch(/invalid at: token/);
  });

  it('invalid vs: range produces a warning', () => {
    const r = parseFilterQuery('vs:bad..range');
    expect(r.asOf).toBeUndefined();
    expect(r.warnings).toHaveLength(1);
    expect(r.warnings[0]).toMatch(/invalid vs: token/);
  });

  it('valid at: HEAD does not produce a warning', () => {
    const r = parseFilterQuery('at:HEAD');
    expect(r.asOf).toEqual({ mode: 'live' });
    expect(r.warnings).toEqual([]);
  });

  it('valid at: short SHA does not produce a warning', () => {
    const r = parseFilterQuery('at:abc1234');
    expect(r.asOf).toEqual({ mode: 'history', commit: 'abc1234' });
    expect(r.warnings).toEqual([]);
  });

  it('valid vs: range does not produce a warning', () => {
    const r = parseFilterQuery('vs:aaa1111..bbb2222');
    expect(r.asOf).toEqual({ mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
    expect(r.warnings).toEqual([]);
  });

  it('warnings are emitted independently for each malformed token', () => {
    const r = parseFilterQuery('at:bad1 vs:bad2..bad3');
    expect(r.warnings.length).toBeGreaterThanOrEqual(2);
  });
});

// A commit-anchored read at a commit the repo does not hold answers 404 with
// problem code commit_not_in_repo (user ruling 2026-11-03). The client must
// tell it apart from a 404 "Fact not found" at a commit the repo holds.
describe('api — 404 commit_not_in_repo is recognisable', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  function mock404(body: Record<string, unknown>) {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false, status: 404, statusText: 'Not Found', json: async () => body,
    } as unknown as Response);
  }
  const notInRepo = { type: 'about:blank', title: 'Commit not found', status: 404, detail: 'this repo has no commit "c0ffee"', code: 'commit_not_in_repo' };
  const factMissing = { type: 'about:blank', title: 'Fact not found', status: 404, detail: 'no fact at path' };

  it('fact at a commit: the rejection carries status and code, and the message is unchanged', async () => {
    mock404(notInRepo);
    const err = await api.fact('beta', 'main', 'kb/x.md', 'c0ffee', { fallback: 'before' }).catch(e => e);
    expect(isCommitNotInRepo(err)).toBe(true);
    expect(String(err)).toMatch(/→ 404 this repo has no commit "c0ffee"/);
  });

  it('a 404 without the code is NOT commit_not_in_repo', async () => {
    mock404(factMissing);
    const err = await api.fact('beta', 'main', 'kb/x.md', 'c0ffee').catch(e => e);
    expect(isNotFound(err)).toBe(true);
    expect(isCommitNotInRepo(err)).toBe(false);
  });

  it('factDiff: a side at an unknown commit rejects (naming that commit) instead of reading as an absent side', async () => {
    mock404(notInRepo);
    const err = await api.factDiff('beta', 'main', 'kb/x.md', 'c0ffee', 'c0ffee').catch(e => e);
    expect(isCommitNotInRepo(err)).toBe(true);
    expect((err as HttpError).commit).toBe('c0ffee');
  });

  it('factDiff: a plain 404 side is still an absent side (null)', async () => {
    mock404(factMissing);
    await expect(api.factDiff('beta', 'main', 'kb/x.md', 'a1', 'a2')).resolves.toEqual({ from: null, to: null });
  });
});
