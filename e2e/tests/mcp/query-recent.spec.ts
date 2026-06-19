import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

/** Learn a fact and return its file path. */
async function learnFact(
  client: McpClient,
  title: string,
  category = 'software',
): Promise<string> {
  const result = await client.callTool('knomit_learn', {
    moment_name: 'test-recent-seed',
    facts: [
      {
        topic: 'technology',
        category,
        title,
        body: `Body for ${title}.`,
        domain: ['testing'],
        confidence: 0.9,
        entities: ['recent-entity'],
      },
    ],
  });
  const parsed = JSON.parse(result.content[0].text || '{}');
  return parsed.commits[0].file;
}

/** Wait for sort=recent to return facts. */
async function waitForRecent(client: McpClient, maxWait = 5000): Promise<void> {
  const deadline = Date.now() + maxWait;
  while (Date.now() < deadline) {
    const result = await client.callTool('knomit_query', { sort: 'recent' });
    const parsed = JSON.parse(result.content[0].text || '{}');
    if (parsed.facts && parsed.facts.length > 0) return;
    await new Promise((r) => setTimeout(r, 500));
  }
}

test.describe('knomit_query sort=recent', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code', freshKnomit.branch);
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('sort=recent returns facts after learning (no filter)', async () => {
    await learnFact(client, 'Recent Fact A');
    await learnFact(client, 'Recent Fact B');
    await waitForRecent(client);

    const result = await client.callTool('knomit_query', { sort: 'recent' });
    expect(result.isError).toBeFalsy();
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed.facts).toBeDefined();
    expect(parsed.facts.length).toBeGreaterThan(0);
    expect(parsed).toHaveProperty('has_more');
    expect(parsed.facts[0].frontmatter.committed_at).toBeGreaterThan(0);
  });

  test('sort=recent with path filter', async () => {
    await learnFact(client, 'Sub Fact A', 'networking');
    await learnFact(client, 'Other Fact B', 'hardware');
    await waitForRecent(client);

    const result = await client.callTool('knomit_query', {
      sort: 'recent',
      path: 'kb/technology/networking',
    });
    expect(result.isError).toBeFalsy();
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed.facts).toBeDefined();
    for (const f of parsed.facts) {
      expect(f.file).toContain('kb/technology/networking');
    }
  });

  test('sort=recent returns cursor-based pagination structure', async () => {
    await learnFact(client, 'Pagination Fact');
    await waitForRecent(client);

    const result = await client.callTool('knomit_query', { sort: 'recent' });
    expect(result.isError).toBeFalsy();
    const parsed = JSON.parse(result.content[0].text || '{}');
    expect(parsed).toHaveProperty('facts');
    expect(parsed).toHaveProperty('cursor');
    expect(parsed).toHaveProperty('has_more');
  });
});
