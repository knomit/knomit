import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Recent', () => {
  test('returns recently modified facts', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/recent`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.facts).toBeDefined();
    expect(Array.isArray(body.facts)).toBeTruthy();
    expect(body.facts.length).toBeGreaterThan(0);
    expect(typeof body.total).toBe('number');
  });
});
