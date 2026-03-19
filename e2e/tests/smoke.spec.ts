import { test, expect } from '../fixtures/knomit.js';

test('shared instance is reachable', async ({ sharedBaseURL }) => {
  const res = await fetch(`${sharedBaseURL}/api/v1/repos`);
  expect(res.status).toBe(200);
});
