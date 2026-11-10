import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { watchUnauthorized } from './eventStream';
import type { EventSourceLike } from './eventStream';
import { authState } from './authState';

// A minimal stream whose error events the test fires by hand.
function fakeStream(readyState: number) {
  const listeners: ((e: unknown) => void)[] = [];
  const s = {
    url: '/api/v1/repo-events', readyState,
    onopen: null, onmessage: null, onerror: null,
    addEventListener: (type: string, fn: (e: unknown) => void) => { if (type === 'error') listeners.push(fn); },
    removeEventListener: () => {},
    close: () => {},
  } as unknown as EventSourceLike;
  return { s, fire: (e: unknown) => listeners.forEach(l => l(e)) };
}

describe('a stream refused with 401 routes to login', () => {
  beforeEach(() => authState.reset());
  afterEach(() => { vi.unstubAllGlobals(); authState.reset(); });

  it('reads the status the desktop stream carries', () => {
    const { s, fire } = fakeStream(2);
    watchUnauthorized(s, s.url);
    fire({ type: 'error', status: 403 });
    expect(authState.isLoginRequired()).toBe(false);
    fire({ type: 'error', status: 401 });
    expect(authState.isLoginRequired()).toBe(true);
  });

  it('probes a CLOSED native stream once and asks for login on 401', async () => {
    const fetchMock = vi.fn().mockResolvedValue({ status: 401 });
    vi.stubGlobal('fetch', fetchMock);
    const { s, fire } = fakeStream(2);
    watchUnauthorized(s, s.url);
    fire({ type: 'error' });
    fire({ type: 'error' });
    await vi.waitFor(() => expect(authState.isLoginRequired()).toBe(true));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('does not probe a stream that is only reconnecting', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const { s, fire } = fakeStream(0); // CONNECTING
    watchUnauthorized(s, s.url);
    fire({ type: 'error' });
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
