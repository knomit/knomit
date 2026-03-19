import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Activity', () => {
  test('returns activity data for a path', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/activity?path=kb/databases`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toBeDefined();
  });

  test('does not 500 without path param', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/activity`);
    expect(res.status()).not.toBe(500);
  });
});
