import { describe, it, expect } from 'vitest';
import { parseSearchQuery, parseFilterQuery } from './api';

describe('parseSearchQuery', () => {
  it('parses plain text', () => {
    const r = parseSearchQuery('hello world');
    expect(r).toEqual({ text: 'hello world', domains: [], entities: [] });
  });

  it('extracts domain filter', () => {
    const r = parseSearchQuery('domain:science foo');
    expect(r).toEqual({ text: 'foo', domains: ['science'], entities: [] });
  });

  it('extracts entity filter', () => {
    const r = parseSearchQuery('entity:alice bar');
    expect(r).toEqual({ text: 'bar', domains: [], entities: ['alice'] });
  });

  it('handles multiple filters', () => {
    const r = parseSearchQuery('domain:a domain:b entity:x search text');
    expect(r).toEqual({ text: 'search text', domains: ['a', 'b'], entities: ['x'] });
  });

  it('extracts quoted phrases', () => {
    const r = parseSearchQuery('"exact phrase" other');
    expect(r).toEqual({ text: 'exact phrase other', domains: [], entities: [] });
  });

  it('handles empty string', () => {
    const r = parseSearchQuery('');
    expect(r).toEqual({ text: '', domains: [], entities: [] });
  });

  it('handles filter-only input with no free text', () => {
    const r = parseSearchQuery('domain:x entity:y');
    expect(r).toEqual({ text: '', domains: ['x'], entities: ['y'] });
  });

  it('ignores empty filter value after colon', () => {
    const r = parseSearchQuery('domain: text');
    expect(r).toEqual({ text: 'text', domains: [], entities: [] });
  });

  it('extracts quoted entity with spaces', () => {
    const r = parseSearchQuery('entity:"Composer 2"');
    expect(r).toEqual({ text: '', domains: [], entities: ['Composer 2'] });
  });

  it('extracts quoted domain with spaces', () => {
    const r = parseSearchQuery('domain:"machine learning" foo');
    expect(r).toEqual({ text: 'foo', domains: ['machine learning'], entities: [] });
  });

  it('mixes quoted and unquoted filters', () => {
    const r = parseSearchQuery('entity:"Composer 2" domain:php search text');
    expect(r).toEqual({ text: 'search text', domains: ['php'], entities: ['Composer 2'] });
  });

  it('handles bare quoted string alongside quoted filter', () => {
    const r = parseSearchQuery('entity:"Composer 2" "exact phrase"');
    expect(r).toEqual({ text: 'exact phrase', domains: [], entities: ['Composer 2'] });
  });
});

describe('parseFilterQuery', () => {
  it('extracts domain and type chips with free text', () => {
    const r = parseFilterQuery('domain:go type:concept free text');
    expect(r).toEqual({ chips: [{ category: 'domain', value: 'go' }, { category: 'type', value: 'concept' }], text: 'free text' });
  });

  it('extracts quoted entity and unquoted path chips', () => {
    const r = parseFilterQuery('entity:"supply chain" path:kb/go');
    expect(r).toEqual({ chips: [{ category: 'entity', value: 'supply chain' }, { category: 'path', value: 'kb/go' }], text: '' });
  });

  it('ep: prefix is treated as free text (not a filter)', () => {
    const r = parseFilterQuery('ep:learn domain:go goroutine scheduling');
    expect(r.chips).toEqual([{ category: 'domain', value: 'go' }]);
    expect(r.text).toContain('ep:learn');
    expect(r.text).toContain('goroutine scheduling');
  });

  it('multiple type chips from typed syntax', () => {
    const r = parseFilterQuery('type:concept type:principle');
    expect(r.chips).toHaveLength(2);
    expect(r.chips[0]).toEqual({ category: 'type', value: 'concept' });
    expect(r.chips[1]).toEqual({ category: 'type', value: 'principle' });
    expect(r.text).toBe('');
  });

  it('path chip with deep path', () => {
    const r = parseFilterQuery('path:kb/technology/ai/anthropic');
    expect(r.chips).toEqual([{ category: 'path', value: 'kb/technology/ai/anthropic' }]);
  });

  it('mixed domain entity type and free text', () => {
    const r = parseFilterQuery('domain:go entity:goroutine type:concept scheduling');
    expect(r.chips).toHaveLength(3);
    expect(r.text).toBe('scheduling');
  });

  it('quoted entity with spaces preserved', () => {
    const r = parseFilterQuery('entity:"supply chain security"');
    expect(r.chips).toEqual([{ category: 'entity', value: 'supply chain security' }]);
  });

  it('empty input returns no chips and empty text', () => {
    const r = parseFilterQuery('');
    expect(r.chips).toHaveLength(0);
    expect(r.text).toBe('');
  });
});
