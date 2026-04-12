import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: History (commits)', () => {
  test('returns commit entries for branch', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/commits`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    // HAL CollectionView: {count, _links, _embedded: {commits: [...]}}
    expect(Array.isArray(body._embedded?.commits)).toBeTruthy();
    expect(body._embedded.commits.length).toBeGreaterThan(0);
  });

  test('respects limit parameter', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/commits?limit=2`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body._embedded.commits.length).toBeLessThanOrEqual(2);
  });

  test('response has pagination links', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/commits`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toHaveProperty('_links');
    expect(Array.isArray(body._embedded?.commits)).toBeTruthy();
  });
});
