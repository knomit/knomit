import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Search', () => {
  test('returns results for a known term', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/search?q=PostgreSQL`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.results)).toBeTruthy();
    expect(body.results.length).toBeGreaterThan(0);
  });

  test('supports domain filter', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/search?q=domain:security`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.results)).toBeTruthy();
  });

  test('returns results array for unknown term', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/search?q=xyznonexistent`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.results)).toBeTruthy();
  });

  test('respects limit parameter', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/search?q=PostgreSQL&limit=1`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.results)).toBeTruthy();
    expect(body.results.length).toBeLessThanOrEqual(1);
  });
});
