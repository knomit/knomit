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

  test('knomit_review fails without LLM configured', async () => {
    const result = await client.callTool('knomit_review', {});
    // Expect an error since no LLM provider is configured in test instances
    expect(result.isError).toBe(true);
  });
});
