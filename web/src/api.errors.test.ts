import { describe, it, expect, vi, beforeEach } from 'vitest';
import { api, parseFilterQuery } from './api';

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
