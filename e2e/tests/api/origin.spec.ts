import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Origin', () => {
  test('returns origin config', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/knomit/origin`);
    // May be 200 with empty origin or 404 if no repo yet
    expect([200, 404]).toContain(res.status());
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
