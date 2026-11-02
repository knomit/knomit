import { afterEach, describe, expect, it } from 'vitest';
import { factsetView, parseFactSetParams, writeFactSetParams } from './factsetOptions';
import { parseMaterializeParams, writeMaterializeParams } from './materializeOptions';
import { DEFAULT_OPTIONS } from './materializeApi';
import type { FsSearch } from './factsetApi';

afterEach(() => { factsetView.close(); window.history.replaceState(null, '', '/'); });

// The FactSet's URL keys (Q-UI6): their own keys, clamped like the single-fact
// view's, never touching materialize=1 / mat_*.
//
// Sabotage: reuse the materialize=1 key for open (the single-fact view opens
// too), or skip the clamp (fs_depth=99 reads as 99).
describe('factset URL state', () => {
  it('round-trips open + options under fs_* keys, keeping every other key, clamped', () => {
    const q = writeFactSetParams('?x=1&materialize=1&mat_depth=6', { open: true, options: { depth: 7, references: 49, crossKb: true, unpin: true } });
    const p = new URLSearchParams(q);
    expect(Object.fromEntries(p)).toEqual({ x: '1', materialize: '1', mat_depth: '6', factset: '1', fs_depth: '7', fs_refs: '49', fs_cross_kb: '1', fs_unpin: '1' });
    expect(parseFactSetParams(q)).toEqual({ open: true, options: { depth: 7, references: 49, crossKb: true, unpin: true } });
    // The single-fact view reads its own keys only.
    expect(parseMaterializeParams(q).options.depth).toBe(6);
    expect(new URLSearchParams(writeMaterializeParams(q, { open: false, options: DEFAULT_OPTIONS })).get('factset')).toBe('1');
    expect(parseFactSetParams('?factset=1&fs_depth=99&fs_refs=-3').options).toEqual({ ...DEFAULT_OPTIONS, depth: 10, references: 0 });
    expect(writeFactSetParams('?a=b&factset=1&fs_depth=7', { open: false, options: DEFAULT_OPTIONS })).toBe('?a=b');
  });

  it('the store writes the flag with replaceState and drops it on close; the search stays in memory', () => {
    const s: FsSearch = { target: { kind: 'lens', lens: 'dev' }, sort: 'relevance', params: 'q=x' };
    const before = window.history.length;
    factsetView.open(s);
    expect(window.location.search).toBe('?factset=1');
    expect(factsetView.get().search).toBe(s);
    factsetView.setOptions({ ...DEFAULT_OPTIONS, unpin: true });
    expect(window.location.search).toBe('?factset=1&fs_unpin=1');
    expect(window.location.search).not.toContain('q=x');
    factsetView.close();
    expect(window.location.search).toBe(''); // closed: no fs_* key at all
    expect(window.history.length).toBe(before);
  });
});
