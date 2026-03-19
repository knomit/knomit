import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Commit', () => {
  test('returns commit detail for a known hash', async ({ request, sharedBaseURL }) => {
    // First get a known commit hash from history
    const histRes = await request.get(`${sharedBaseURL}/api/v1/knomit/history?path=kb&limit=1`);
    expect(histRes.ok()).toBeTruthy();
    const histBody = await histRes.json();
    expect(histBody.entries.length).toBeGreaterThan(0);
    const hash = histBody.entries[0].hash;

    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/commit?hash=${hash}`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toBeDefined();
    expect(body.hash).toBe(hash);
  });

  test('returns 404 for nonexistent hash', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/commit?hash=nonexistent`);
    expect(res.status()).toBe(404);
  });
});
