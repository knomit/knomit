import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Rebuild', () => {
  test('triggers async rebuild', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.post(`${freshKnomit.baseURL}/api/v1/knomit/rebuild`);
    expect(res.status()).toBe(202);
  });
});
