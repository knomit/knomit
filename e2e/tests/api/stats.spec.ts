import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Stats', () => {
  test('returns aggregate stats for kb', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/stats?path=kb`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.total).toBeGreaterThan(0);
  });

  test('returns scoped stats for subdirectory', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/stats?path=kb/databases`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toBeDefined();
  });
});
