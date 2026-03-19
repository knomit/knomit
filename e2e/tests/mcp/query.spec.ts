import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

/** Helper: learn a fact and return its file path. */
async function learnFact(
  client: McpClient,
  title: string,
  body: string,
  opts: { domain?: string[]; entities?: string[]; category?: string } = {},
): Promise<string> {
  const result = await client.callTool('knomit_learn', {
    moment_name: 'test-query-seed',
    facts: [
      {
        topic: 'technology',
        category: opts.category ?? 'software',
        title,
        body,
        domain: opts.domain ?? ['testing'],
        confidence: 0.9,
        entities: opts.entities ?? ['test-entity'],
      },
    ],
  });
  const parsed = JSON.parse(result.content[0].text || '{}');
  return parsed.commits[0].file;
}

/** Wait for a query to return at least one result. */
async function waitForQuery(
  client: McpClient,
  params: Record<string, unknown>,
  maxWait = 5000,
): Promise<{ facts: Array<{ title: string; file: string; frontmatter: Record<string, unknown> }> }> {
  const deadline = Date.now() + maxWait;
  while (Date.now() < deadline) {
    const result = await client.callTool('knomit_query', params);
    const parsed = JSON.parse(result.content[0].text || '{}');
    if (parsed.facts && parsed.facts.length > 0) return parsed;
    await new Promise((r) => setTimeout(r, 500));
  }
  // Return empty on timeout
  return { facts: [] };
}

test.describe('knomit_query', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('query by text returns matching facts', async () => {
    await learnFact(
      client,
      'Kubernetes Pod Scheduling',
      'Kubernetes uses a scheduler to assign pods to nodes based on resource requests and constraints.',
      { category: 'software' },
    );
    await learnFact(
      client,
      'Ethernet Frame Structure',
      'An Ethernet frame consists of a preamble, destination MAC, source MAC, EtherType, payload, and FCS.',
      { category: 'networking' },
    );

    const parsed = await waitForQuery(client, { text: 'Kubernetes Pod Scheduling' });
    expect(parsed.facts.length).toBeGreaterThan(0);
    const titles = parsed.facts.map((f) => f.title);
    expect(titles.some((t) => t.includes('Kubernetes'))).toBe(true);
  });

  test('query with entity filter', async () => {
    await learnFact(
      client,
      'Widget Component Design',
      'Widgets are reusable UI components that encapsulate rendering logic.',
      { entities: ['widget-unique'], category: 'software' },
    );
    await learnFact(
      client,
      'Gadget Hardware Interface',
      'Gadgets communicate with the host via USB HID protocol.',
      { entities: ['gadget-unique'], category: 'hardware' },
    );

    // Wait for entity-based query to return results
    const parsed = await waitForQuery(client, { entities: ['widget-unique'] });
    expect(parsed.facts.length).toBeGreaterThan(0);
    const titles = parsed.facts.map((f) => f.title);
    expect(titles.some((t) => t.includes('Widget'))).toBe(true);
    expect(titles.some((t) => t.includes('Gadget'))).toBe(false);
  });

  test('query with domain filter', async () => {
    await learnFact(
      client,
      'React Component Lifecycle',
      'React components go through mounting, updating, and unmounting phases.',
      { domain: ['frontend-unique'], category: 'software' },
    );
    await learnFact(
      client,
      'PostgreSQL Query Optimization',
      'PostgreSQL uses cost-based optimization to choose execution plans.',
      { domain: ['backend-unique'], category: 'data' },
    );

    // Wait for domain-based query to return results
    const parsed = await waitForQuery(client, { domain: ['frontend-unique'] });
    expect(parsed.facts.length).toBeGreaterThan(0);
    const titles = parsed.facts.map((f) => f.title);
    expect(titles.some((t) => t.includes('React'))).toBe(true);
    expect(titles.some((t) => t.includes('PostgreSQL'))).toBe(false);
  });

  test('query with no params returns error', async () => {
    const result = await client.callTool('knomit_query', {});
    expect(result.isError).toBe(true);
  });
});
