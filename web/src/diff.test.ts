import { describe, it, expect } from 'vitest';
import { unifiedDiff } from './diff';

describe('unifiedDiff', () => {
  it('returns all-equal lines for identical inputs', () => {
    const r = unifiedDiff('a\nb\nc', 'a\nb\nc');
    expect(r.every(l => l.kind === 'eq')).toBe(true);
  });

  it('detects a single insertion', () => {
    const r = unifiedDiff('a\nc', 'a\nb\nc');
    expect(r).toEqual([
      { kind: 'eq',  text: 'a' },
      { kind: 'add', text: 'b' },
      { kind: 'eq',  text: 'c' },
    ]);
  });

  it('detects a single deletion', () => {
    const r = unifiedDiff('a\nb\nc', 'a\nc');
    expect(r).toEqual([
      { kind: 'eq',  text: 'a' },
      { kind: 'del', text: 'b' },
      { kind: 'eq',  text: 'c' },
    ]);
  });

  it('detects a replacement (del + add)', () => {
    const r = unifiedDiff('a\nold\nc', 'a\nnew\nc');
    expect(r).toEqual([
      { kind: 'eq',  text: 'a' },
      { kind: 'del', text: 'old' },
      { kind: 'add', text: 'new' },
      { kind: 'eq',  text: 'c' },
    ]);
  });

  it('handles empty from (all-add)', () => {
    const r = unifiedDiff('', 'a\nb');
    expect(r).toEqual([
      { kind: 'add', text: 'a' },
      { kind: 'add', text: 'b' },
    ]);
  });

  it('handles empty to (all-del)', () => {
    const r = unifiedDiff('a\nb', '');
    expect(r).toEqual([
      { kind: 'del', text: 'a' },
      { kind: 'del', text: 'b' },
    ]);
  });

  it('returns empty for two empty inputs', () => {
    expect(unifiedDiff('', '')).toEqual([]);
  });
});
