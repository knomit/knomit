import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

const validFact = `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [test-entity]
refs: []
---
# Learn Test Fact

This is a fact created by the learn spec.
`;

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
      path: 'testing/learn-basic',
      content: validFact,
    });
    expect(result.isError).toBeFalsy();
    expect(result.content.length).toBeGreaterThan(0);
  });

  test('returns error for invalid frontmatter', async () => {
    const badContent = `---
not_a_valid_field: ???
---
# Bad Fact
`;
    const result = await client.callTool('knomit_learn', {
      path: 'testing/learn-bad',
      content: badContent,
    });
    expect(result.isError).toBe(true);
  });

  test('learned fact is queryable', async () => {
    await client.callTool('knomit_learn', {
      path: 'testing/learn-query',
      content: validFact,
    });

    const result = await client.callTool('knomit_query', {
      text: 'Learn Test Fact',
    });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('learn-query');
  });
});
