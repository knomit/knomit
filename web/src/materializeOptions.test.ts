import { describe, it, expect } from 'vitest';
import { DEFAULT_OPTIONS } from './materializeApi';
import type { MatOptions } from './materializeApi';
import {
  exportFilename, headingLevel, id8, optionChange, parseMaterializeParams, slugTitle, writeMaterializeParams,
} from './materializeOptions';

describe('materialize options in the URL', () => {
  it('round-trips every option through the query string', () => {
    const options: MatOptions = { depth: 7, maxFacts: 350, crossKb: true, unpin: true };
    const search = writeMaterializeParams('', { open: true, options });
    expect(parseMaterializeParams(search)).toEqual({ open: true, options });
  });

  it('writes only what differs from the defaults, and nothing when closed', () => {
    expect(writeMaterializeParams('', { open: true, options: DEFAULT_OPTIONS })).toBe('?materialize=1');
    expect(writeMaterializeParams('?materialize=1&mat_depth=9', { open: false, options: DEFAULT_OPTIONS })).toBe('');
  });

  it('keeps parameters that are not its own', () => {
    const search = writeMaterializeParams('?foo=bar', { open: true, options: { ...DEFAULT_OPTIONS, depth: 2 } });
    expect(new URLSearchParams(search).get('foo')).toBe('bar');
    expect(parseMaterializeParams(search).options.depth).toBe(2);
  });

  it('clamps a hand-edited URL to the limits (depth 0..10, facts 1..500) and falls back on junk', () => {
    const p = parseMaterializeParams('?materialize=1&mat_depth=99&mat_facts=100000');
    expect(p.options).toMatchObject({ depth: 10, maxFacts: 500 });
    const q = parseMaterializeParams('?materialize=1&mat_depth=-3&mat_facts=0');
    expect(q.options).toMatchObject({ depth: 0, maxFacts: 1 });
    const r = parseMaterializeParams('?materialize=1&mat_depth=abc&mat_facts=');
    expect(r.options).toMatchObject({ depth: 4, maxFacts: 200 });
  });

  it('defaults when the URL says nothing', () => {
    expect(parseMaterializeParams('')).toEqual({ open: false, options: DEFAULT_OPTIONS });
  });
});

describe('what an option change means for a stream', () => {
  const base = DEFAULT_OPTIONS;
  it('raising ONLY the count continues in place', () => {
    expect(optionChange(base, { ...base, maxFacts: 300 })).toBe('continue');
  });
  it('lowering the count, or changing anything else, restarts', () => {
    expect(optionChange(base, { ...base, maxFacts: 100 })).toBe('restart');
    expect(optionChange(base, { ...base, depth: 5 })).toBe('restart');
    expect(optionChange(base, { ...base, crossKb: true })).toBe('restart');
    expect(optionChange(base, { ...base, unpin: true })).toBe('restart');
    expect(optionChange(base, { ...base, maxFacts: 300, depth: 5 })).toBe('restart');
  });
  it('is a no-op when nothing changed', () => {
    expect(optionChange(base, { ...base })).toBe('same');
  });
});

describe('export filename', () => {
  it('is <slugged title, <=48 chars, cut at a word>-<id8>.md', () => {
    const title = 'materialize rulings (2026-10-28): pinned by default, unpin silently drops gone targets';
    expect(exportFilename(title, 'kb/decisions/materialize/rulings/16ccefe6.md'))
      .toBe('materialize-rulings-2026-10-28-pinned-by-default-16ccefe6.md');
  });
  it('never exceeds 48 characters of slug, never ends a slug on a dash', () => {
    const slug = slugTitle('A '.repeat(40) + 'supercalifragilisticexpialidocious and more');
    expect(slug.length).toBeLessThanOrEqual(48);
    expect(slug.endsWith('-')).toBe(false);
    expect(slugTitle('x'.repeat(100))).toHaveLength(48);
  });
  it('strips accents and punctuation, and falls back when nothing is left', () => {
    expect(slugTitle('Café — “Résumé”!')).toBe('cafe-resume');
    expect(exportFilename('???', 'kb/a/b/abcdef0123456789.md')).toBe('materialized-abcdef01.md');
  });
  it('id8 is the first 8 characters of the file name', () => {
    expect(id8('kb/decisions/x/16ccefe6.md')).toBe('16ccefe6');
    expect(id8('kb/decisions/x/0123456789abcdef.md')).toBe('01234567');
  });
});

describe('heading level by walk depth', () => {
  it('steps down one level per hop and stops at h6', () => {
    expect([0, 1, 2, 3, 4, 9].map(headingLevel)).toEqual([2, 3, 4, 5, 6, 6]);
  });
});
