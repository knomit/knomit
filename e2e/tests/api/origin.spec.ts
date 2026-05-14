import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Origin', () => {
  test('returns origin config', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/repos/knomit/origin`);
    // Returns 204 (no content) when no origin is configured, or 200 with origin data
    expect([200, 204]).toContain(res.status());
  });

  test('sets origin URL', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/repos/knomit/origin`, {
      data: { url: 'git@github.com:test/origin-e2e.git' },
    });
    // The URL is unreachable from the e2e harness, so ActivateSync fails and
    // the handler returns 502. The contract (handlers_origin_hal.go) is that
    // the origin row IS persisted even when activation fails — the 502 only
    // signals the initial reconcile didn't succeed.
    expect(res.status()).toBe(502);

    // Verify the URL was persisted despite the 502.
    const getRes = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/repos/knomit/origin`);
    expect(getRes.ok()).toBeTruthy();
    const body = await getRes.json();
    expect(body.url).toBe('git@github.com:test/origin-e2e.git');
  });
});
