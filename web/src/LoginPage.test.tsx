import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { LoginPage } from './LoginPage';

type Reply = { status: number; body?: unknown };

// fakeServer answers POST /auth/pair once and then each GET of the wait URL
// with the next reply in waits.
function fakeServer(waits: Reply[]) {
  const calls: { url: string; method: string }[] = [];
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    calls.push({ url, method });
    if (url.endsWith('/auth/pair') && method === 'POST') {
      return { ok: true, status: 201, json: async () => ({ id: 'P'.repeat(43), code: 'KQ7-M2X', wait: `/auth/pair/${'P'.repeat(43)}/wait` }) };
    }
    const next = waits.shift() ?? { status: 202 };
    return { ok: next.status >= 200 && next.status < 300, status: next.status, json: async () => next.body ?? {} };
  });
  vi.stubGlobal('fetch', fetchMock);
  return { calls, fetchMock };
}

describe('LoginPage', () => {
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

  it('starts a pairing, shows the code and how to approve it, and logs in when wait returns 200', async () => {
    const { calls } = fakeServer([{ status: 202 }, { status: 200, body: { status: 'approved' } }]);
    const onLoggedIn = vi.fn();
    render(<LoginPage onLoggedIn={onLoggedIn} minPollMs={5} />);

    expect(await screen.findByTestId('pair-code')).toHaveTextContent(/^[A-Z2-9]{3}-[A-Z2-9]{3}$/);
    expect(screen.getByTestId('pair-code')).toHaveTextContent('KQ7-M2X');
    expect(screen.getByTestId('pair-help')).toHaveTextContent(
      'Approve this browser from a signed-in browser or run knomit auth approve KQ7-M2X');

    await waitFor(() => expect(onLoggedIn).toHaveBeenCalledTimes(1));
    expect(calls[0]).toEqual({ url: '/auth/pair', method: 'POST' });
    const waits = calls.filter(c => c.url.endsWith('/wait'));
    expect(waits.length).toBe(2);
    expect(waits.every(c => c.method === 'GET')).toBe(true);
  });

  it('says so when the pairing is denied and does not log in', async () => {
    fakeServer([{ status: 403 }]);
    const onLoggedIn = vi.fn();
    render(<LoginPage onLoggedIn={onLoggedIn} minPollMs={5} />);
    expect(await screen.findByTestId('pair-denied')).toBeInTheDocument();
    expect(onLoggedIn).not.toHaveBeenCalled();
  });

  it('is what App renders once an API call answered 401', async () => {
    fakeServer([]);
    const { default: App } = await import('./App');
    const { authState } = await import('./authState');
    authState.reset();
    authState.requireLogin();
    render(<App />);
    expect(await screen.findByTestId('login-page')).toBeInTheDocument();
    authState.reset();
  });

  it('starts a new pairing when the old one expired', async () => {
    const { calls } = fakeServer([{ status: 410 }, { status: 200 }]);
    const onLoggedIn = vi.fn();
    render(<LoginPage onLoggedIn={onLoggedIn} minPollMs={5} />);
    await waitFor(() => expect(onLoggedIn).toHaveBeenCalled());
    expect(calls.filter(c => c.url.endsWith('/auth/pair')).length).toBe(2);
  });
});
