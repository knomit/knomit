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

  test('type filter restricts results to specified type', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/search?q=PostgreSQL&type=observation`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.results)).toBeTruthy();
    for (const r of body.results) {
      expect(r.type).toBe('observation');
    }
  });

  test('exclude_type filter excludes specified type', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/search?q=PostgreSQL&exclude_type=hypothesis`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body.results)).toBeTruthy();
    for (const r of body.results) {
      expect(r.type).not.toBe('hypothesis');
    }
  });
});
