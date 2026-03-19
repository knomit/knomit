import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

test.describe('knomit_review', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('knomit_review is listed among tools', async () => {
    const tools = await client.listTools();
    const names = tools.map((t) => t.name);
    expect(names).toContain('knomit_review');
  });

  test('knomit_review with no dirty facts returns done', async () => {
    // Fresh KB has no dirty facts, so review should complete immediately
    const result = await client.callTool('knomit_review', {});
    expect(result.isError).toBeFalsy();
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed.session_id).toBeDefined();
    expect(parsed.done).toBe(true);
  });
});
