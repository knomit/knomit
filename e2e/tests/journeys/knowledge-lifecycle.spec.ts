import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';
import { McpClient } from '../../helpers/mcp-client.js';

test.describe.serial('Knowledge Lifecycle', () => {
  test('create → view → search → edit → history → retract', async ({ freshKnomit, page }) => {
    const factPath = 'kb/lifecycle/demo.md';
    const factContent = `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [lifecycle-entity]
refs: []
---
# Lifecycle Demo

This fact exercises the full lifecycle.`;

    // 1. Create fact via API
    const createRes = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: { path: factPath, content: factContent },
    });
    expect(createRes.ok()).toBeTruthy();

    // 2. Navigate to it in the UI
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');
    const browse = new BrowsePage(page);

    // Navigate into lifecycle directory then click the fact
    await browse.clickEntry('lifecycle');
    await browse.clickEntry('demo.md');

    // 3. Verify it displays
    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Lifecycle Demo');
    await expect(factPanel.body).toContainText('full lifecycle');

    // 4. Search for it
    await browse.search('lifecycle');
    const searchResults = await browse.getSearchResults();
    expect(searchResults).toContain(factPath);

    // Clear search to return to browse mode
    await browse.clearSearch();

    // 5. Edit via editor
    const updatedContent = `---
type: observation
domain: [testing]
confidence: 0.95
sources: 2
entities: [lifecycle-entity]
refs: []
---
# Lifecycle Demo

Updated content after edit.`;

    // Navigate back to the fact
    await browse.clickEntry('lifecycle');
    await browse.clickEntry('demo.md');
    await expect(factPanel.title).toContainText('Lifecycle Demo');

    await factPanel.editContent(updatedContent);
    await factPanel.save();

    // 6. Verify the update
    await expect(factPanel.body).toContainText('Updated content after edit');

    // 7. Check history has multiple commits
    await page.keyboard.press('h');
    const timeline = page.getByTestId('history-timeline');
    await timeline.waitFor({ timeout: 10_000 });
    const commits = page.getByTestId('history-commit');
    const commitCount = await commits.count();
    expect(commitCount).toBeGreaterThanOrEqual(2);

    // Exit history mode
    await page.keyboard.press('Escape');

    // 8. Retract via MCP
    const mcp = new McpClient(freshKnomit.baseURL, 'knomit', 'code');
    await mcp.initialize();
    try {
      const retractResult = await mcp.callTool('knomit_retract', { path: 'lifecycle/demo' });
      expect(retractResult.isError).toBeFalsy();
    } finally {
      await mcp.close();
    }

    // 9. Verify it's gone from search
    await browse.search('lifecycle');
    const afterResults = await browse.getSearchResults();
    expect(afterResults).not.toContain(factPath);
  });
});
