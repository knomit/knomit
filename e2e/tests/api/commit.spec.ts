import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Commit', () => {
  test('returns commit detail for a known hash', async ({ request, sharedBaseURL, sharedBranch }) => {
    // First get a known commit hash from commits list
    const histRes = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/commits?limit=1`);
    expect(histRes.ok()).toBeTruthy();
    const histBody = await histRes.json();
    expect(histBody._embedded.commits.length).toBeGreaterThan(0);
    const commit = histBody._embedded.commits[0].commit;

    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/commits/${commit}`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toBeDefined();
    expect(body.commit).toBe(commit);
  });

  test('returns 404 for nonexistent hash', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/commits/nonexistent`);
    expect(res.status()).toBe(404);
  });
});
