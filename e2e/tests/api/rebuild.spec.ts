import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Rebuild', () => {
  test('triggers async rebuild', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.post(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/index-rebuilds`,
    );
    // 201: job started (a running heal is replaced); 200: an identical
    // rebuild was already running and absorbed this one. There is no 409.
    expect([200, 201]).toContain(res.status());
    const job = await res.json();
    expect(job.kind).toBe('index-rebuild');
    expect(job.state).toBe('running');
  });
});
