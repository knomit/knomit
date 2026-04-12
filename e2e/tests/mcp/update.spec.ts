import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

/** Learn a fact and return its file path. */
async function learnFact(client: McpClient, category = 'software'): Promise<string> {
  const result = await client.callTool('knomit_learn', {
    moment_name: 'test-update-seed',
    facts: [
      {
        topic: 'technology',
        category,
        title: 'Updatable Fact',
        body: 'This fact will be updated.',
        domain: ['testing'],
        confidence: 0.5,
        entities: ['original-entity'],
      },
    ],
  });
  const parsed = JSON.parse(result.content[0].text || '{}');
  return parsed.commits[0].file;
}

/** Wait for a fact to appear in query results. */
async function waitForIndex(client: McpClient, text: string, maxWait = 5000): Promise<void> {
  const deadline = Date.now() + maxWait;
  while (Date.now() < deadline) {
    const result = await client.callTool('knomit_query', { text });
    const parsed = JSON.parse(result.content[0].text || '{}');
    if (parsed.facts && parsed.facts.length > 0) return;
    await new Promise((r) => setTimeout(r, 500));
  }
}

/** Query by path and return the first matching fact. */
async function queryByPath(client: McpClient, path: string): Promise<Record<string, unknown> | null> {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    const result = await client.callTool('knomit_query', { path });
    const parsed = JSON.parse(result.content[0].text || '{}');
    if (parsed.facts && parsed.facts.length > 0) {
      // Find exact match
      const match = parsed.facts.find((f: { file: string }) => f.file === path);
      if (match) return match;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  return null;
}

test.describe('knomit_update', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code', freshKnomit.branch);
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('update confidence', async () => {
    const path = await learnFact(client);

    const upd = await client.callTool('knomit_update', {
      file: path,
      moment_name: 'test-update-conf',
      updates: { confidence: 0.95 },
    });
    expect(upd.isError).toBeFalsy();

    // Verify via query — frontmatter includes confidence
    const fact = await queryByPath(client, path);
    expect(fact).toBeTruthy();
    const fm = fact!.frontmatter as { confidence: number };
    expect(fm.confidence).toBe(0.95);
  });

  test('update domain tags', async () => {
    const path = await learnFact(client, 'hardware');

    const upd = await client.callTool('knomit_update', {
      file: path,
      moment_name: 'test-update-dom',
      updates: { domain: ['infrastructure', 'devops'] },
    });
    expect(upd.isError).toBeFalsy();

    const fact = await queryByPath(client, path);
    expect(fact).toBeTruthy();
    const fm = fact!.frontmatter as { domain: string[] };
    expect(fm.domain).toContain('infrastructure');
    expect(fm.domain).toContain('devops');
  });

  test('update entities', async () => {
    const path = await learnFact(client, 'networking');

    const upd = await client.callTool('knomit_update', {
      file: path,
      moment_name: 'test-update-ent',
      updates: { entities: ['new-entity-a', 'new-entity-b'] },
    });
    expect(upd.isError).toBeFalsy();

    const fact = await queryByPath(client, path);
    expect(fact).toBeTruthy();
    const fm = fact!.frontmatter as { entities: string[] };
    expect(fm.entities).toContain('new-entity-a');
    expect(fm.entities).toContain('new-entity-b');
  });
});
