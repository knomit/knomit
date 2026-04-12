import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Search', () => {
  test('returns results for a known term', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/search?q=PostgreSQL`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    // HAL CollectionView: {count, _links, _embedded: {results: [...]}}
    expect(Array.isArray(body._embedded?.results)).toBeTruthy();
    expect(body._embedded.results.length).toBeGreaterThan(0);
  });

  test('supports domain filter', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/search?q=domain:security`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body._embedded?.results)).toBeTruthy();
  });

  test('returns results array for unknown term', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/search?q=xyznonexistent`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body._embedded?.results)).toBeTruthy();
  });

  test('respects limit parameter', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/search?q=PostgreSQL&limit=1`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body._embedded?.results)).toBeTruthy();
    expect(body._embedded.results.length).toBeLessThanOrEqual(1);
  });

  test('type filter restricts results to specified type', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/search?q=PostgreSQL&type=observation`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body._embedded?.results)).toBeTruthy();
    for (const r of body._embedded.results) {
      expect(r.type).toBe('observation');
    }
  });

  test('exclude_type filter excludes specified type', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/search?q=PostgreSQL&exclude_type=hypothesis`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body._embedded?.results)).toBeTruthy();
    for (const r of body._embedded.results) {
      expect(r.type).not.toBe('hypothesis');
    }
  });
});
