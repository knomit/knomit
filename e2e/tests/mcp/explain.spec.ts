import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

/** Learn a fact and return its file path. */
async function learnFact(client: McpClient): Promise<string> {
  const result = await client.callTool('knomit_learn', {
    moment_name: 'test-explain-seed',
    facts: [
      {
        topic: 'technology',
        category: 'software',
        title: 'Explainable Fact',
        body: 'This fact exists for testing the explain tool.',
        domain: ['testing'],
        confidence: 0.85,
        entities: ['explain-entity'],
      },
    ],
  });
  const parsed = JSON.parse(result.content[0].text || '{}');
  return parsed.commits[0].file;
}

test.describe('knomit_explain', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('explain returns fact with provenance structure', async () => {
    const path = await learnFact(client);

    const result = await client.callTool('knomit_explain', { file: path });
    expect(result.isError).toBeFalsy();
    expect(result.content.length).toBeGreaterThan(0);

    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed.facts).toBeDefined();
    expect(parsed.facts.length).toBe(1);
    expect(parsed.facts[0].path).toBe(path);
    expect(parsed.facts[0].title).toBe('Explainable Fact');
    expect(parsed.facts[0].depth).toBe(0);
    expect(parsed.facts[0]).toHaveProperty('commit');
    expect(parsed.facts[0]).toHaveProperty('refs');
    expect(parsed).toHaveProperty('cursor');
    expect(parsed).toHaveProperty('has_more');
  });
});
