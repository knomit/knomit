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
    const createRes = await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/${factPath}`,
      { data: { content: factContent } },
    );
    expect(createRes.ok()).toBeTruthy();

    // 2. Navigate to it in the UI
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');
    const browse = new BrowsePage(page);

    // Navigate into lifecycle directory then click the fact
    await browse.clickEntry('lifecycle');
    await browse.clickEntry('demo.md');

    // 3. Verify it displays
    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Lifecycle Demo');
    await expect(factPanel.body).toContainText('full lifecycle');

    // 4. Search for it (index is async — retry until it catches up)
    // In the new UI, search results show filenames (data-name), not full paths
    const factName = factPath.split('/').pop()!;
    let searchResults: string[] = [];
    for (let attempt = 0; attempt < 5; attempt++) {
      await browse.search('lifecycle');
      searchResults = await browse.getSearchResults();
      if (searchResults.some(r => r.includes(factName))) break;
      await browse.clearSearch();
      await page.waitForTimeout(1000);
    }
    expect(searchResults.some(r => r.includes(factName))).toBeTruthy();

    // 5. Edit via API (the UI only shows the editor for facts with parse errors)
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

    const editRes = await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/${factPath}`,
      { data: { content: updatedContent } },
    );
    expect(editRes.ok()).toBeTruthy();

    // 6. Verify the update in the UI
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');
    await browse.clickEntry('lifecycle');
    await browse.clickEntry('demo.md');
    await expect(factPanel.body).toContainText('Updated content after edit');

    // 7. Check history has multiple commits
    await page.keyboard.press('3');
    const timeline = page.getByTestId('history-timeline');
    await timeline.waitFor({ timeout: 10_000 });
    const commits = page.getByTestId('history-commit');
    const commitCount = await commits.count();
    expect(commitCount).toBeGreaterThanOrEqual(2);

    // Exit history view back to tree
    await page.keyboard.press('1');

    // 8. Retract via MCP
    const mcp = new McpClient(freshKnomit.baseURL, 'knomit', 'code', freshKnomit.branch);
    await mcp.initialize();
    try {
      const retractResult = await mcp.callTool('knomit_retract', { file: 'lifecycle/demo', moment_name: 'e2e-test' });
      expect(retractResult.isError).toBeFalsy();
    } finally {
      await mcp.close();
    }

    // 9. Verify it's gone from search (index is async — retry until it catches up)
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');
    let afterResults: string[] = [factName]; // seed so the loop runs
    for (let attempt = 0; attempt < 5; attempt++) {
      await page.waitForTimeout(1000);
      await browse.search('lifecycle');
      afterResults = await browse.getSearchResults();
      if (!afterResults.some(r => r.includes(factName))) break;
      await browse.clearSearch();
    }
    expect(afterResults.some(r => r.includes(factName))).toBeFalsy();
  });
});
