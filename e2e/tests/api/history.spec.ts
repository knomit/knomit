import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: History', () => {
  test('returns entries for kb path', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/history?path=kb`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.entries).toBeDefined();
    expect(Array.isArray(body.entries)).toBeTruthy();
    expect(body.entries.length).toBeGreaterThan(0);
  });

  test('respects limit parameter', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/history?path=kb&limit=2`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.entries.length).toBeLessThanOrEqual(2);
  });

  test('response has cursor field for pagination', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/history?path=kb`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect('cursor' in body).toBeTruthy();
  });
});
