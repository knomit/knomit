import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Synthesize', () => {
  test('returns 503 (no LLM) or 202', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.post(`${freshKnomit.baseURL}/api/v1/knomit/synthesize`);
    expect([202, 503]).toContain(res.status());
  });
});
