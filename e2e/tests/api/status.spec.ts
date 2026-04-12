import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Status (branch root)', () => {
  test('returns branch root with expected fields', async ({ request, sharedBaseURL, sharedBranch }) => {
    const res = await request.get(`${sharedBaseURL}/api/v1/repos/knomit/branches/${sharedBranch}`);
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body).toHaveProperty('head');
    expect(body).toHaveProperty('name');
    expect(body).toHaveProperty('embeddings_enabled');
    expect(body).toHaveProperty('is_agent_branch');
  });
});
