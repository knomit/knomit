import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Commit', () => {
  test('returns commit detail for a known hash', async ({ request, sharedBaseURL }) => {
    // First get a known commit hash from history
    const histRes = await request.get(`${sharedBaseURL}/api/v1/knomit/history?path=kb&limit=1`);
    expect(histRes.ok()).toBeTruthy();
    const histBody = await histRes.json();
    expect(histBody.entries.length).toBeGreaterThan(0);
    const commit = histBody.entries[0].commit;

    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/commit?hash=${commit}`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toBeDefined();
    expect(body.commit).toBe(commit);
  });

  test('returns 404 for nonexistent hash', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/commit?hash=nonexistent`);
    expect(res.status()).toBe(404);
  });
});
