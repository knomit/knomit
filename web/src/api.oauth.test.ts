import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, KNOMIT_CLIENT_HEADER, KNOMIT_CLIENT_WEB } from './api';

// The OAuth approval calls (F19 phase 3b). The server's approval gate admits
// the browser only with the custom header (internal/web/oauth_pending_api.go
// spells it identically; its own test pins the Go side), a JSON body type on
// mutations, and the Origin the browser adds by itself. Only these calls send
// the header: on the desktop every other cross-origin call would otherwise
// gain a preflight.

function mockFetch(status: number, body: unknown) {
  const f = vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: () => Promise.resolve(body),
  });
  globalThis.fetch = f as unknown as typeof fetch;
  return f;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('OAuth approval api', () => {
  it('spells the header exactly as the server does', () => {
    expect(KNOMIT_CLIENT_HEADER).toBe('X-Knomit-Client');
    expect(KNOMIT_CLIENT_WEB).toBe('web');
  });

  it('lists pending requests with the header and unwraps them', async () => {
    const f = mockFetch(200, { pending: [{ id: 'p1', client_name: 'Claude Code' }] });
    const got = await api.listOAuthPending();
    expect(got).toEqual([{ id: 'p1', client_name: 'Claude Code' }]);
    const [url, init] = f.mock.calls[0];
    expect(url).toBe('/api/v1/oauth/pending');
    expect(new Headers(init.headers).get('X-Knomit-Client')).toBe('web');
  });

  it('reads 404 as "not configured here" (null), not as an error', async () => {
    mockFetch(404, { title: 'Not Found' });
    await expect(api.listOAuthPending()).resolves.toBeNull();
  });

  it('surfaces a 403 with the server\'s reason', async () => {
    mockFetch(403, { title: 'Permission denied', detail: 'the anonymous loopback principal does not hold admin' });
    await expect(api.listOAuthPending()).rejects.toThrow(/does not hold admin/);
  });

  it('approves with a JSON body, the JSON type and the header', async () => {
    const f = mockFetch(200, { id: 'p1', decision: 'approved' });
    await api.approveOAuthPending('p1', 'laptop', ['read', 'write']);
    const [url, init] = f.mock.calls[0];
    expect(url).toBe('/api/v1/oauth/pending/p1/approve');
    expect(init.method).toBe('POST');
    const h = new Headers(init.headers);
    expect(h.get('Content-Type')).toBe('application/json');
    expect(h.get('X-Knomit-Client')).toBe('web');
    expect(JSON.parse(init.body)).toEqual({ subject: 'laptop', scopes: ['read', 'write'] });
  });

  it('denies with the JSON type and the header (the gate requires both on every mutation)', async () => {
    const f = mockFetch(204, null);
    await api.denyOAuthPending('p/1');
    const [url, init] = f.mock.calls[0];
    expect(url).toBe('/api/v1/oauth/pending/p%2F1/deny');
    const h = new Headers(init.headers);
    expect(h.get('Content-Type')).toBe('application/json');
    expect(h.get('X-Knomit-Client')).toBe('web');
  });

  it('surfaces a refused approval', async () => {
    mockFetch(400, { title: 'Bad Request', detail: 'oauth: subject must be 1-128 characters' });
    await expect(api.approveOAuthPending('p1', 'bad subject', ['read'])).rejects.toThrow(/subject must be/);
  });
});
