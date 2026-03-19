import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

const baseFact = `---
type: observation
domain: [testing]
confidence: 0.5
sources: 1
entities: [original-entity]
refs: []
---
# Updatable Fact

This fact will be updated.
`;

test.describe('knomit_update', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('update confidence', async () => {
    await client.callTool('knomit_learn', { path: 'upd/conf', content: baseFact });

    const upd = await client.callTool('knomit_update', {
      path: 'upd/conf',
      confidence: 0.95,
    });
    expect(upd.isError).toBeFalsy();

    const result = await client.callTool('knomit_explain', { path: 'upd/conf' });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('0.95');
  });

  test('update domain tags', async () => {
    await client.callTool('knomit_learn', { path: 'upd/dom', content: baseFact });

    const upd = await client.callTool('knomit_update', {
      path: 'upd/dom',
      domain: ['infrastructure', 'devops'],
    });
    expect(upd.isError).toBeFalsy();

    const result = await client.callTool('knomit_explain', { path: 'upd/dom' });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('infrastructure');
  });

  test('update entities', async () => {
    await client.callTool('knomit_learn', { path: 'upd/ent', content: baseFact });

    const upd = await client.callTool('knomit_update', {
      path: 'upd/ent',
      entities: ['new-entity-a', 'new-entity-b'],
    });
    expect(upd.isError).toBeFalsy();

    const result = await client.callTool('knomit_explain', { path: 'upd/ent' });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('new-entity-a');
  });
});
