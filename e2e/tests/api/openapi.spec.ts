import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: OpenAPI', () => {
  test('returns OpenAPI YAML spec', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/openapi.yaml`);
    expect(res.ok()).toBeTruthy();
    const text = await res.text();
    expect(text).toContain('openapi');
  });
});
