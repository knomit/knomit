import { describe, it, expect, afterEach, vi } from 'vitest';
import { fetchVersion } from './api';

describe('fetchVersion', () => {
  afterEach(() => { vi.restoreAllMocks(); });

  it('GETs /api/v1/version and returns the version fields', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        version: '0.5.0',
        commit: '2a7ae9d',
        full: '0.5.0.2a7ae9d',
        _links: { self: { href: '/api/v1/version' } },
      }),
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const v = await fetchVersion();

    expect(fetchMock).toHaveBeenCalledWith('/api/v1/version', undefined);
    expect(v).toEqual({ version: '0.5.0', commit: '2a7ae9d', full: '0.5.0.2a7ae9d', readOnly: false });
  });

  it('throws on a non-2xx response', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false, status: 500, statusText: 'Internal Server Error', json: async () => ({}),
    }) as unknown as typeof fetch;

    await expect(fetchVersion()).rejects.toThrow();
  });
});
