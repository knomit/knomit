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
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code', freshKnomit.branch);
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

  // #715 made a filterless query legal: the defaults bound it to one page of
  // 20 snippet rows, with a cursor to the rest.
  test('query with no params returns the bounded first page and a cursor', async () => {
    const subjects = [
      'Volcanic basalt columns', 'Medieval falconry', 'Sourdough fermentation', 'Tidal power turbines',
      'Byzantine mosaics', 'Arctic tern migration', 'Origami tessellations', 'Jazz modal harmony',
      'Coral reef bleaching', 'Roman aqueduct gradients', 'Beekeeping swarm control', 'Glacier calving',
      'Typesetting ligatures', 'Chess endgame tablebases', 'Silk road caravanserais', 'Lichen symbiosis',
      'Clock escapement design', 'Desert dune migration', 'Morse code timing', 'Bonsai root pruning',
      'Lunar regolith sintering', 'Viking longship keels',
    ];
    const result = await client.callTool('knomit_learn', {
      moment_name: 'test-query-unfiltered',
      facts: subjects.map((s, i) => ({
        topic: 'technology',
        category: `unfiltered/q${String(i).padStart(2, '0')}`,
        title: s,
        body: `${s}: a fact about ${s.toLowerCase()} and nothing else, number ${i}.`,
        entities: [`unfiltered-${i}`],
      })),
    });
    expect(result.isError, result.content[0].text).toBeFalsy();

    // The index syncs after the commit: wait until the first page is full.
    let parsed: { facts?: unknown[]; cursor?: string | null; has_more?: boolean } = {};
    const deadline = Date.now() + 10_000;
    while (Date.now() < deadline) {
      const res = await client.callTool('knomit_query', {});
      expect(res.isError, res.content[0].text).toBeFalsy();
      parsed = JSON.parse(res.content[0].text || '{}');
      if ((parsed.facts?.length ?? 0) >= 20) break;
      await new Promise((r) => setTimeout(r, 500));
    }
    expect(parsed.facts?.length).toBe(20);
    expect(parsed.has_more).toBe(true);
    expect(typeof parsed.cursor).toBe('string');
    expect(parsed.cursor).not.toBe('');
  });
});
