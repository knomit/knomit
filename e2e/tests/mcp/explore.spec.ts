import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

/** Learn a fact and return its file path. */
async function learnFact(
  client: McpClient,
  title: string,
  category = 'software',
): Promise<string> {
  const result = await client.callTool('knomit_learn', {
    moment_name: 'test-explore-seed',
    facts: [
      {
        topic: 'technology',
        category,
        title,
        body: `Body for ${title}.`,
        domain: ['testing'],
        confidence: 0.9,
        entities: ['explore-entity'],
      },
    ],
  });
  const parsed = JSON.parse(result.content[0].text || '{}');
  return parsed.commits[0].file;
}

/** Wait for explore to return facts. */
async function waitForExplore(client: McpClient, maxWait = 5000): Promise<void> {
  const deadline = Date.now() + maxWait;
  while (Date.now() < deadline) {
    const result = await client.callTool('knomit_explore', {});
    const parsed = JSON.parse(result.content[0].text || '{}');
    if (parsed.facts && parsed.facts.length > 0) return;
    await new Promise((r) => setTimeout(r, 500));
  }
}

test.describe('knomit_explore', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code', freshKnomit.branch);
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('explore returns facts after learning', async () => {
    await learnFact(client, 'Explore Fact A');
    await learnFact(client, 'Explore Fact B');
    await waitForExplore(client);

    const result = await client.callTool('knomit_explore', {});
    expect(result.isError).toBeFalsy();
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed.facts).toBeDefined();
    expect(parsed.facts.length).toBeGreaterThan(0);
    expect(parsed).toHaveProperty('has_more');
  });

  test('explore with path filter', async () => {
    await learnFact(client, 'Sub Fact A', 'networking');
    await learnFact(client, 'Other Fact B', 'hardware');
    await waitForExplore(client);

    const result = await client.callTool('knomit_explore', { path: 'kb/technology/networking' });
    expect(result.isError).toBeFalsy();
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed.facts).toBeDefined();
    // All returned facts should be under kb/technology/networking
    for (const f of parsed.facts) {
      expect(f.path).toContain('kb/technology/networking');
    }
  });

  test('explore returns cursor-based pagination structure', async () => {
    await learnFact(client, 'Pagination Fact');
    await waitForExplore(client);

    const result = await client.callTool('knomit_explore', {});
    expect(result.isError).toBeFalsy();
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed).toHaveProperty('facts');
    expect(parsed).toHaveProperty('cursor');
    expect(parsed).toHaveProperty('has_more');
  });
});
