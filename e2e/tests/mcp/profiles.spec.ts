import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

const CORE_TOOLS = ['knomit_learn', 'knomit_query'];

test.describe('MCP profiles', () => {
  test('default profile returns core tools', async ({ freshKnomit }) => {
    const client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
    const tools = await client.listTools();
    const names = tools.map((t) => t.name);
    for (const tool of CORE_TOOLS) {
      expect(names).toContain(tool);
    }
    await client.close();
  });

  test('unknown profile falls back to code', async ({ freshKnomit }) => {
    const codeClient = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await codeClient.initialize();
    const codeTools = await codeClient.listTools();

    const unknownClient = new McpClient(freshKnomit.baseURL, 'knomit', 'nonexistent' as 'code');
    await unknownClient.initialize();
    const unknownTools = await unknownClient.listTools();

    const codeNames = codeTools.map((t) => t.name).sort();
    const unknownNames = unknownTools.map((t) => t.name).sort();
    expect(unknownNames).toEqual(codeNames);

    await codeClient.close();
    await unknownClient.close();
  });

  test('all three profiles return core tools', async ({ freshKnomit }) => {
    for (const profile of ['code', 'chat', 'generic'] as const) {
      const client = new McpClient(freshKnomit.baseURL, 'knomit', profile);
      await client.initialize();
      const tools = await client.listTools();
      const names = tools.map((t) => t.name);
      for (const tool of CORE_TOOLS) {
        expect(names, `profile '${profile}' should have ${tool}`).toContain(tool);
      }
      await client.close();
    }
  });

  test('profiles share the same tool set', async ({ freshKnomit }) => {
    const toolSets: string[][] = [];
    for (const profile of ['code', 'chat', 'generic'] as const) {
      const client = new McpClient(freshKnomit.baseURL, 'knomit', profile);
      await client.initialize();
      const tools = await client.listTools();
      toolSets.push(tools.map((t) => t.name).sort());
      await client.close();
    }
    // All profiles should expose the same tools
    expect(toolSets[0]).toEqual(toolSets[1]);
    expect(toolSets[1]).toEqual(toolSets[2]);
  });
});
