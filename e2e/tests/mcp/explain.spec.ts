import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

const fact = `---
type: observation
domain: [testing]
confidence: 0.85
sources: 1
entities: [explain-entity]
refs: []
---
# Explainable Fact

This fact exists for testing the explain tool.
`;

test.describe('knomit_explain', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('explain returns path and metadata for a learned fact', async () => {
    await client.callTool('knomit_learn', {
      path: 'explain/target',
      content: fact,
    });

    const result = await client.callTool('knomit_explain', {
      path: 'explain/target',
    });
    expect(result.isError).toBeFalsy();
    expect(result.content.length).toBeGreaterThan(0);
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('explain/target');
  });
});
