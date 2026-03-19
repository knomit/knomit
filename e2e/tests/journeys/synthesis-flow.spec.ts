import { test, expect } from '../../fixtures/knomit.js';

test.describe.serial('Synthesis Flow', () => {
  test('create overlapping facts and trigger synthesize', async ({ freshKnomit }) => {
    // Create overlapping facts that could be candidates for synthesis
    const facts = [
      {
        path: 'kb/synth/caching-basics.md',
        content: `---
type: observation
domain: [caching]
confidence: 0.8
sources: 1
entities: [caching, redis]
refs: []
---
# Caching Basics

Caching stores frequently accessed data closer to the consumer to reduce latency and backend load.`,
      },
      {
        path: 'kb/synth/caching-strategies.md',
        content: `---
type: observation
domain: [caching]
confidence: 0.85
sources: 1
entities: [caching, cache-aside]
refs: []
---
# Caching Strategies

Common strategies include cache-aside, write-through, and write-behind. Cache-aside is the most popular pattern for read-heavy workloads.`,
      },
      {
        path: 'kb/synth/redis-caching.md',
        content: `---
type: observation
domain: [caching]
confidence: 0.75
sources: 1
entities: [redis, caching]
refs: []
---
# Redis as a Cache

Redis is often used as an in-memory cache layer. Its key-value model and TTL support make it well-suited for caching patterns.`,
      },
    ];

    for (const fact of facts) {
      const res = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
        data: { path: fact.path, content: fact.content },
      });
      expect(res.ok()).toBeTruthy();
    }

    // Trigger synthesize — expect 503 (no LLM configured) or 202 (accepted)
    const synthRes = await freshKnomit.api.post(`${freshKnomit.baseURL}/api/v1/knomit/synthesize`);
    expect([202, 503]).toContain(synthRes.status());

    if (synthRes.status() === 503) {
      // Gracefully handle no-LLM case — the endpoint is reachable but cannot synthesize
      const body = await synthRes.json();
      expect(body).toBeTruthy();
    }
  });
});
