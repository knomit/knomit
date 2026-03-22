import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Repos', () => {
  test('lists all repos', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(Array.isArray(body)).toBeTruthy();
    expect(body.some((r: any) => r.name === 'knomit')).toBeTruthy();
  });
});
