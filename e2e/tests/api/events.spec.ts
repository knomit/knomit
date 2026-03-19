import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Events (SSE)', () => {
  test('connects and returns text/event-stream content type', async ({ freshKnomit }) => {
    const res = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/knomit/events`);
    const contentType = res.headers()['content-type'] ?? '';
    expect(contentType).toContain('text/event-stream');
  });
});
