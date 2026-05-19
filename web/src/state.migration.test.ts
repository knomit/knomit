import { describe, it, expect } from 'vitest';
import { init } from './state';

describe('view migration', () => {
  it('init.view is "library"', () => {
    expect(init.view).toBe('library');
  });
});
