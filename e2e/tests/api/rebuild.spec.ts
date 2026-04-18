import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Rebuild', () => {
  test('triggers async rebuild', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.post(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/index-rebuilds`,
    );
    // 201: job started, 409: already running
    expect([201, 409]).toContain(res.status());
  });
});
