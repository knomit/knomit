import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

const fact = `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [retract-entity]
refs: []
---
# Retractable Fact

This fact will be retracted.
`;

test.describe('knomit_retract', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('retracted fact is no longer queryable', async () => {
    await client.callTool('knomit_learn', {
      path: 'retract/gone',
      content: fact,
    });

    // Verify it exists first
    const before = await client.callTool('knomit_query', { text: 'Retractable Fact' });
    expect(before.isError).toBeFalsy();
    const beforeText = before.content.map((c) => c.text ?? '').join('');
    expect(beforeText).toContain('retract/gone');

    // Retract it
    const retractResult = await client.callTool('knomit_retract', {
      path: 'retract/gone',
    });
    expect(retractResult.isError).toBeFalsy();

    // Verify it's gone
    const after = await client.callTool('knomit_query', { text: 'Retractable Fact' });
    const afterText = after.content.map((c) => c.text ?? '').join('');
    expect(afterText).not.toContain('retract/gone');
  });
});
