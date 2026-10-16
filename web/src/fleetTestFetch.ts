import { vi } from 'vitest';

// A fetch stub for the fleet tests, at the FETCH seam (not api.*): a route
// is `METHOD /api/v1/...` and answers a status and a JSON body; every call is
// recorded with its parsed body, so a test asserts the route called and what
// it sent. An unrouted call fails loudly (404 with the route in the detail).

export interface FleetCall { route: string; body: unknown }
export type FleetReply = { status?: number; body?: unknown } | ((body: unknown) => { status?: number; body?: unknown });

export function stubFleetFetch(routes: Record<string, FleetReply>): { calls: FleetCall[]; set: (route: string, reply: FleetReply) => void } {
  const calls: FleetCall[] = [];
  const table = { ...routes };
  vi.stubGlobal('fetch', vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, 'http://localhost');
    const route = `${(init?.method ?? 'GET').toUpperCase()} ${url.pathname}`;
    let body: unknown = undefined;
    if (typeof init?.body === 'string') {
      try { body = JSON.parse(init.body); } catch { body = init.body; }
    }
    calls.push({ route, body });
    const r = table[route];
    const reply = typeof r === 'function' ? r(body) : r;
    if (!reply) {
      return new Response(JSON.stringify({ detail: `unrouted ${route}` }), { status: 404, headers: { 'Content-Type': 'application/problem+json' } });
    }
    const status = reply.status ?? 200;
    return new Response(status === 204 ? null : JSON.stringify(reply.body ?? {}), { status, headers: { 'Content-Type': 'application/json' } });
  }));
  return { calls, set: (route, reply) => { table[route] = reply; } };
}

/** problem is a refusal body as the server sends it. */
export const problem = (status: number, code: string, detail: string, extra: Record<string, unknown> = {}) =>
  ({ status, body: { type: 'about:blank', title: 'refused', status, detail, code, ...extra } });

/** Every URL-looking string a container renders: what the no-knomit+ checks read. */
export function renderedText(el: HTMLElement): string {
  return el.textContent ?? '';
}
