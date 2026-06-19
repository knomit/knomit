import { describe, it, expect, afterEach } from 'vitest';
import { apiUrl } from './api';

describe('apiUrl', () => {
  afterEach(() => { delete (window as unknown as { __KNOMIT_API_BASE__?: string }).__KNOMIT_API_BASE__; });

  it('returns a relative path when no base is set (cloud, same-origin)', () => {
    expect(apiUrl('/api/v1/repos')).toBe('/api/v1/repos');
  });

  it('prefixes the absolute base when set (desktop, cross-origin)', () => {
    (window as unknown as { __KNOMIT_API_BASE__?: string }).__KNOMIT_API_BASE__ = 'http://127.0.0.1:19278';
    expect(apiUrl('/api/v1/repos')).toBe('http://127.0.0.1:19278/api/v1/repos');
  });
});
