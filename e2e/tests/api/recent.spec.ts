import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Recent (facts collection)', () => {
  test('returns recently modified facts', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/facts`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    // HAL CollectionView: {count, _links, _embedded: {facts: [...]}}
    expect(typeof body.count).toBe('number');
    expect(Array.isArray(body._embedded?.facts)).toBeTruthy();
    expect(body._embedded.facts.length).toBeGreaterThan(0);
  });

  test('recent facts include type field', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/facts`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body._embedded.facts.length).toBeGreaterThan(0);
    for (const fact of body._embedded.facts) {
      expect(typeof fact.type).toBe('string');
      expect(fact.type.length).toBeGreaterThan(0);
    }
  });

  test('type filter returns only matching types', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/facts?type=observation`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    for (const fact of body._embedded.facts) {
      expect(fact.type).toBe('observation');
    }
  });

  test('exclude_type filter excludes specified types', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}/facts?exclude_type=observation`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    for (const fact of body._embedded.facts) {
      expect(fact.type).not.toBe('observation');
    }
  });
});
