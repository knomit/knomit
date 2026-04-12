import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Repos', () => {
  test('lists all repos', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    // HAL CollectionView: {count, _links, _embedded: {repos: [{name, _links}]}}
    expect(typeof body.count).toBe('number');
    expect(Array.isArray(body._embedded?.repos)).toBeTruthy();
    expect(body._embedded.repos.some((r: any) => r.name === 'knomit')).toBeTruthy();
  });
});
