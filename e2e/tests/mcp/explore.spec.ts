import { test, expect } from '../../fixtures/knomit.js';
import { McpClient } from '../../helpers/mcp-client.js';

const fact = (title: string) => `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [explore-entity]
refs: []
---
# ${title}

Body.
`;

test.describe('knomit_explore', () => {
  let client: McpClient;

  test.beforeEach(async ({ freshKnomit }) => {
    client = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await client.initialize();

    // Seed some facts in a directory structure
    await client.callTool('knomit_learn', { path: 'explore/sub/fact-a', content: fact('Fact A') });
    await client.callTool('knomit_learn', { path: 'explore/sub/fact-b', content: fact('Fact B') });
    await client.callTool('knomit_learn', { path: 'explore/other/fact-c', content: fact('Fact C') });
  });

  test.afterEach(async () => {
    await client.close();
  });

  test('explore root returns directory listing', async () => {
    const result = await client.callTool('knomit_explore', {});
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('explore');
  });

  test('explore into a subdirectory', async () => {
    const result = await client.callTool('knomit_explore', { path: 'explore/sub' });
    expect(result.isError).toBeFalsy();
    const text = result.content.map((c) => c.text ?? '').join('');
    expect(text).toContain('fact-a');
  });

  test('session-based: multiple explore calls build up state', async () => {
    // First call — root
    const r1 = await client.callTool('knomit_explore', {});
    expect(r1.isError).toBeFalsy();

    // Second call — deeper
    const r2 = await client.callTool('knomit_explore', { path: 'explore' });
    expect(r2.isError).toBeFalsy();
    const text2 = r2.content.map((c) => c.text ?? '').join('');
    expect(text2).toContain('sub');
    expect(text2).toContain('other');

    // Third call — leaf directory
    const r3 = await client.callTool('knomit_explore', { path: 'explore/sub' });
    expect(r3.isError).toBeFalsy();
    const text3 = r3.content.map((c) => c.text ?? '').join('');
    expect(text3).toContain('fact-a');
    expect(text3).toContain('fact-b');
  });
});
