import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { fetchVersion, fetchJSON } from './api';
import { authState } from './authState';

// Any API answer of 401 (a credential the server no longer accepts, or none
// where one is needed — only after the unified-auth flip) switches the app to
// the login page, once however many requests fail together, and the request
// that got the 401 still rejects for its caller.
describe('API 401 routes to login', () => {
  beforeEach(() => { authState.reset(); });
  afterEach(() => { vi.restoreAllMocks(); authState.reset(); });

  const unauthorized = () => vi.fn().mockResolvedValue({
    ok: false, status: 401, statusText: 'Unauthorized', json: async () => ({ title: 'Invalid credential' }),
  });

  it('calls requireLogin once for concurrent 401s and rejects each original promise', async () => {
    globalThis.fetch = unauthorized() as unknown as typeof fetch;
    const spy = vi.spyOn(authState, 'requireLogin');
    const results = await Promise.allSettled([fetchVersion(), fetchJSON('/api/v1/repos'), fetchJSON('/api/v1/lenses')]);
    expect(results.every(r => r.status === 'rejected')).toBe(true);
    expect(spy).toHaveBeenCalledTimes(1);
    expect(authState.isLoginRequired()).toBe(true);
  });

  it('does not ask for login on other errors', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: false, status: 403, statusText: 'Forbidden', json: async () => ({}),
    }) as unknown as typeof fetch;
    const spy = vi.spyOn(authState, 'requireLogin');
    await expect(fetchVersion()).rejects.toThrow();
    expect(spy).not.toHaveBeenCalled();
    expect(authState.isLoginRequired()).toBe(false);
  });

  it('notifies subscribers once and clears on loggedIn', () => {
    const seen: boolean[] = [];
    const unsub = authState.subscribe(() => seen.push(authState.isLoginRequired()));
    authState.requireLogin();
    authState.requireLogin();
    authState.loggedIn();
    unsub();
    authState.requireLogin();
    expect(seen).toEqual([true, false]);
  });
});
