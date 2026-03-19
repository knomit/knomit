import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

function makeFact(title: string, opts: { domain?: string; entity?: string } = {}): string {
  const domain = opts.domain ?? 'testing';
  const entity = opts.entity ?? 'test-entity';
  return `---
type: observation
domain: [${domain}]
confidence: 0.9
sources: 1
entities: [${entity}]
refs: []
---
# ${title}

Body for ${title}.
`;
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
    await client.callTool('knomit_learn', { path: 'q/alpha', content: makeFact('Alpha Fact') });
    await client.callTool('knomit_learn', { path: 'q/beta', content: makeFact('Beta Fact') });
    await client.callTool('knomit_learn', { path: 'q/gamma', content: makeFact('Gamma Fact') });

    const result = await client.callTool('knomit_query', { text: 'Alpha' });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('alpha');
  });

  test('query with entity filter', async () => {
    await client.callTool('knomit_learn', { path: 'q/ent-a', content: makeFact('Ent A', { entity: 'widget' }) });
    await client.callTool('knomit_learn', { path: 'q/ent-b', content: makeFact('Ent B', { entity: 'gadget' }) });

    const result = await client.callTool('knomit_query', { entities: ['widget'] });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('ent-a');
    expect(text).not.toContain('ent-b');
  });

  test('query with domain filter', async () => {
    await client.callTool('knomit_learn', { path: 'q/dom-a', content: makeFact('Dom A', { domain: 'frontend' }) });
    await client.callTool('knomit_learn', { path: 'q/dom-b', content: makeFact('Dom B', { domain: 'backend' }) });

    const result = await client.callTool('knomit_query', { domain: 'frontend' });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('dom-a');
    expect(text).not.toContain('dom-b');
  });

  test('query with empty text returns no results', async () => {
    const result = await client.callTool('knomit_query', {});
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    // Empty query should return nothing or an empty set
    expect(text).not.toContain('q/');
  });
});
