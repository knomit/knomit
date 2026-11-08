import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, KNOMIT_CLIENT_HEADER, KNOMIT_CLIENT_WEB } from './api';

// The OAuth approval calls (F19 phase 3b). The server's approval gate admits
// the browser only with the custom header (internal/edge/web/oauth_pending_api.go
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

  it('returns whether the approval left the subject\'s grants unchanged (F19 3c R5)', async () => {
    mockFetch(200, { id: 'p1', decision: 'approved', subject: 'laptop', grants_unchanged: true });
    await expect(api.approveOAuthPending('p1', 'laptop', ['read'])).resolves.toEqual({ subject: 'laptop', grantsUnchanged: true });
    mockFetch(200, { id: 'p1', decision: 'approved', subject: 'laptop' });
    await expect(api.approveOAuthPending('p1', 'laptop', ['read'])).resolves.toEqual({ subject: 'laptop', grantsUnchanged: false });
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

// The approved logins (GET /api/v1/oauth/grants, POST .../{id}/revoke): the
// same gate as the queue, so the same header, and the same 404 rule.
describe('OAuth approved logins api', () => {
  it('lists the logins with the header and unwraps them', async () => {
    const f = mockFetch(200, { grants: [{ id: 'g1', client_name: 'Claude Code', last_used_at: null }] });
    await expect(api.listOAuthGrants()).resolves.toEqual([{ id: 'g1', client_name: 'Claude Code', last_used_at: null }]);
    const [url, init] = f.mock.calls[0];
    expect(url).toBe('/api/v1/oauth/grants');
    expect(new Headers(init.headers).get('X-Knomit-Client')).toBe('web');
  });

  it('reads 404 as "no issuer here" (null), which hides the Approved group', async () => {
    mockFetch(404, { title: 'Not Found' });
    await expect(api.listOAuthGrants()).resolves.toBeNull();
  });

  it('reads a body with no grants as an empty list', async () => {
    mockFetch(200, {});
    await expect(api.listOAuthGrants()).resolves.toEqual([]);
  });

  it('surfaces a 403 with the server\'s reason', async () => {
    mockFetch(403, { title: 'Permission denied', detail: 'the anonymous loopback principal does not hold admin' });
    await expect(api.listOAuthGrants()).rejects.toThrow(/does not hold admin/);
  });

  it('revokes with POST, the JSON type and the header, escaping the id', async () => {
    const f = mockFetch(204, null);
    await api.revokeOAuthGrant('g/1');
    const [url, init] = f.mock.calls[0];
    expect(url).toBe('/api/v1/oauth/grants/g%2F1/revoke');
    expect(init.method).toBe('POST');
    const h = new Headers(init.headers);
    expect(h.get('Content-Type')).toBe('application/json');
    expect(h.get('X-Knomit-Client')).toBe('web');
  });

  it('surfaces a refused revoke', async () => {
    mockFetch(404, { title: 'Not Found', detail: 'oauth: no such approved login' });
    await expect(api.revokeOAuthGrant('gone')).rejects.toThrow(/no such approved login/);
  });
});
