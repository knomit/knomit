import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Status', () => {
  test('returns status with expected fields', async ({ request, sharedBaseURL }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/knomit/status`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toHaveProperty('head');
    expect(body).toHaveProperty('branch');
    expect(body).toHaveProperty('embeddings_enabled');
    expect(body).toHaveProperty('ontology_root');
  });
});
