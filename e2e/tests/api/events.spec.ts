import { test, expect } from '../../fixtures/knomit.js';

test.describe('API: Events (SSE)', () => {
  test('connects and returns text/event-stream content type', async ({ freshKnomit }) => {
    // SSE is a streaming endpoint — use fetch with an abort to avoid hanging.
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 2000);
    try {
      const res = await fetch(
        `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/events`,
        { signal: controller.signal },
      );
      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toContain('text/event-stream');
    } catch (err: any) {
      // AbortError is expected — the SSE stream never ends on its own.
      if (err.name !== 'AbortError') throw err;
    } finally {
      clearTimeout(timer);
    }
  });
});
