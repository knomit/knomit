import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

test.describe('knomit_learn', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('learns a new fact without error', async () => {
    const result = await client.callTool('knomit_learn', {
      moment_name: 'test-learn',
      facts: [
        {
          topic: 'technology',
          category: 'software',
          title: 'Learn Test',
          body: 'Test body for the learn spec.',
          domain: ['testing'],
          confidence: 0.9,
          entities: ['playwright'],
        },
      ],
    });
    expect(result.isError).toBeFalsy();
    expect(result.content.length).toBeGreaterThan(0);
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed.commits).toBeDefined();
    expect(parsed.commits.length).toBe(1);
    expect(parsed.commits[0].file).toMatch(/^kb\/technology\/software\/[a-f0-9]+\.md$/);
  });

  test('returns error for missing required fields', async () => {
    const result = await client.callTool('knomit_learn', {
      moment_name: 'test-bad',
      facts: [
        {
          topic: 'technology',
          // missing category, title, body
        },
      ],
    });
    expect(result.isError).toBe(true);
  });

  test('learned fact is queryable', async () => {
    const learnResult = await client.callTool('knomit_learn', {
      moment_name: 'test-learn-query',
      facts: [
        {
          topic: 'technology',
          category: 'software',
          title: 'Queryable Fact From Learn Spec',
          body: 'This unique fact should be queryable after learning.',
          domain: ['testing'],
          confidence: 0.9,
          entities: ['playwright'],
        },
      ],
    });
    expect(learnResult.isError).toBeFalsy();

    // Wait for index sync
    const deadline = Date.now() + 5000;
    let found = false;
    while (Date.now() < deadline) {
      const qr = await client.callTool('knomit_query', { text: 'Queryable Fact From Learn Spec' });
      const parsed = JSON.parse(qr.content[0].text || '{}');
      if (parsed.facts && parsed.facts.length > 0) {
        found = true;
        break;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
    expect(found).toBe(true);
  });
});
