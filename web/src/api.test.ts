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

  it('extracts ep and domain chips with free text', () => {
    const r = parseFilterQuery('ep:learn domain:go goroutine scheduling');
    expect(r).toEqual({ chips: [{ category: 'ep', value: 'learn' }, { category: 'domain', value: 'go' }], text: 'goroutine scheduling' });
  });
});
