import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

/** Learn a fact and return its file path. */
async function learnFact(client: McpClient): Promise<string> {
  const result = await client.callTool('knomit_learn', {
    moment_name: 'test-retract-seed',
    facts: [
      {
        topic: 'technology',
        category: 'software',
        title: 'Retractable Fact',
        body: 'This fact will be retracted.',
        domain: ['testing'],
        confidence: 0.9,
        entities: ['retract-entity'],
      },
    ],
  });
  const parsed = JSON.parse(result.content[0].text || '{}');
  return parsed.commits[0].file;
}

/** Wait for index to pick up a text query. */
async function waitForIndex(client: McpClient, text: string, maxWait = 5000): Promise<void> {
  const deadline = Date.now() + maxWait;
  while (Date.now() < deadline) {
    const result = await client.callTool('knomit_query', { text });
    const parsed = JSON.parse(result.content[0].text || '{}');
    if (parsed.facts && parsed.facts.length > 0) return;
    await new Promise((r) => setTimeout(r, 500));
  }
}

test.describe('knomit_retract', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code', freshKnomit.branch);
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('retracted fact is no longer queryable', async () => {
    const path = await learnFact(client);

    // Wait for it to be indexed
    await waitForIndex(client, 'Retractable Fact');

    // Verify it exists
    const before = await client.callTool('knomit_query', { text: 'Retractable Fact' });
    const beforeParsed = JSON.parse(before.content[0].text || '{}');
    expect(beforeParsed.facts.length).toBeGreaterThan(0);

    // Retract it
    const retractResult = await client.callTool('knomit_retract', {
      file: path,
      moment_name: 'test-retract',
    });
    expect(retractResult.isError).toBeFalsy();

    // Wait for index to update, then verify it's gone
    const deadline = Date.now() + 5000;
    let gone = false;
    while (Date.now() < deadline) {
      const after = await client.callTool('knomit_query', { text: 'Retractable Fact' });
      const afterParsed = JSON.parse(after.content[0].text || '{}');
      if (afterParsed.facts.length === 0) {
        gone = true;
        break;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
    expect(gone).toBe(true);
  });
});
