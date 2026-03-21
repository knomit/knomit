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

  test('recent facts include type field', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/recent`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.facts.length).toBeGreaterThan(0);
    for (const fact of body.facts) {
      expect(typeof fact.type).toBe('string');
      expect(fact.type.length).toBeGreaterThan(0);
    }
  });

  test('type filter returns only matching types', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/recent?type=observation`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    for (const fact of body.facts) {
      expect(fact.type).toBe('observation');
    }
  });

  test('exclude_type filter excludes specified types', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/recent?exclude_type=observation`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    for (const fact of body.facts) {
      expect(fact.type).not.toBe('observation');
    }
  });
});
