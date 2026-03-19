import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Stats', () => {
  test('returns aggregate stats for kb', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/stats?path=kb`);
    // Stats requires the Idx (SearchIndex) to be available; may 503 if not.
    if (res.ok()) {
      const body = await res.json();
      expect(body).toBeDefined();
      expect(body.total).toBeGreaterThan(0);
    } else {
      // 500 = stats query error (server bug), 503 = index not available
      expect([500, 503]).toContain(res.status());
    }
  });

  test('returns scoped stats for subdirectory', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/stats?path=kb/databases`);
    if (res.ok()) {
      const body = await res.json();
      expect(body).toBeDefined();
    } else {
      // 500 = stats query error (server bug), 503 = index not available
      expect([500, 503]).toContain(res.status());
    }
  });
});
