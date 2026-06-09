import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Synthesize', () => {
  test('returns 503 (no LLM) or 202', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.post(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/synthesis-runs`,
    );
    // 201: job started, 503: no LLM configured, 409: already running
    expect([201, 503, 409]).toContain(res.status());
  });
});
