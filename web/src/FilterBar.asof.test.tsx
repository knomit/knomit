import { describe, it, expect } from 'vitest';
import { parseFilterQuery } from './api';

describe('parseFilterQuery — at:/vs: anchor controls', () => {
  it('parses at:<7-char-sha> as history', () => {
    const result = parseFilterQuery('at:b812d40');
    expect(result.asOf).toEqual({ mode: 'history', commit: 'b812d40' });
    expect(result.chips).toEqual([]);
  });

  it('parses at:HEAD as live', () => {
    const result = parseFilterQuery('at:HEAD');
    expect(result.asOf).toEqual({ mode: 'live' });
  });

  it('rejects malformed at: hash', () => {
    expect(parseFilterQuery('at:b812d4').asOf).toBeUndefined();
    expect(parseFilterQuery('at:b812d40zzz').asOf).toBeUndefined();
    expect(parseFilterQuery('at:not-a-hash').asOf).toBeUndefined();
    expect(parseFilterQuery('at:').asOf).toBeUndefined();
  });

  it('parses vs:<from>..<to> as diff', () => {
    const result = parseFilterQuery('vs:c4f1abc..c9a7def');
    expect(result.asOf).toEqual({ mode: 'diff', from: 'c4f1abc', to: 'c9a7def' });
  });

  it('rejects malformed vs: range', () => {
    expect(parseFilterQuery('vs:c4f1abc').asOf).toBeUndefined();
    expect(parseFilterQuery('vs:c4f1abc..').asOf).toBeUndefined();
    expect(parseFilterQuery('vs:..c9a7def').asOf).toBeUndefined();
  });

  it('parses normal filters without affecting asOf', () => {
    const result = parseFilterQuery('domain:ai entity:Foo');
    expect(result.asOf).toBeUndefined();
    expect(result.chips.length).toBe(2);
  });

  it('parses combined: at:<sha> + filter chips', () => {
    const result = parseFilterQuery('at:b812d40 domain:ai');
    expect(result.asOf).toEqual({ mode: 'history', commit: 'b812d40' });
    expect(result.chips).toEqual([{ category: 'domain', value: 'ai' }]);
  });
});
