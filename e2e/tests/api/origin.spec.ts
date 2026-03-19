import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Origin', () => {
  test('returns origin config', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/knomit/origin`);
    // Returns 204 (no content) when no origin is configured, or 200 with origin data
    expect([200, 204]).toContain(res.status());
  });

  test('sets origin URL', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/origin`, {
      data: { url: 'git@github.com:test/origin-e2e.git' },
    });
    expect(res.ok()).toBeTruthy();

    // Verify it was set
    const getRes = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/knomit/origin`);
    expect(getRes.ok()).toBeTruthy();
    const body = await getRes.json();
    expect(body.url).toBe('git@github.com:test/origin-e2e.git');
  });
});
