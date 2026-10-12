import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Origin', () => {
  test('returns origin config', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/repos/knomit/origin`);
    // Returns 204 (no content) when no origin is configured, or 200 with origin data
    expect([200, 204]).toContain(res.status());
  });

  test('an unreachable origin is refused and nothing is persisted', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/repos/knomit/origin`, {
      data: { url: 'git@github.com:test/origin-e2e.git' },
    });
    // The attach probes the remote BEFORE writing anything (handlers_origin_hal.go,
    // lifecycle event AttachOrigin): an unreachable remote or a refused credential
    // is a 502 "Origin not attached" and the origin row is NOT persisted.
    expect(res.status()).toBe(502);
    const problem = await res.json();
    expect(problem.title).toBe('Origin not attached');

    // Nothing was persisted: no origin is configured.
    const getRes = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/repos/knomit/origin`);
    expect(getRes.status()).toBe(204);
  });
});
