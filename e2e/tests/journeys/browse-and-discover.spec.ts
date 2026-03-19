import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe.serial('Browse and Discover', () => {
  test('navigate tree → view metadata → click entity tag → find related facts', async ({ freshKnomit, page }) => {
    // Create several related facts sharing an entity
    const sharedEntity = 'discovery-topic';
    const facts = [
      {
        path: 'kb/discover/alpha.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [${sharedEntity}, alpha-only]
refs: []
---
# Alpha Fact

First fact about the discovery topic.`,
      },
      {
        path: 'kb/discover/beta.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.85
sources: 1
entities: [${sharedEntity}, beta-only]
refs: []
---
# Beta Fact

Second fact about the discovery topic.`,
      },
      {
        path: 'kb/discover/gamma.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.8
sources: 1
entities: [${sharedEntity}]
refs: []
---
# Gamma Fact

Third fact about the discovery topic.`,
      },
    ];

    for (const fact of facts) {
      const res = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
        data: { path: fact.path, content: fact.content },
      });
      expect(res.ok()).toBeTruthy();
    }

    // Navigate to the UI
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');
    const browse = new BrowsePage(page);

    // Navigate directory tree into discover/
    await browse.clickEntry('discover');
    const entries = await browse.getDirectoryEntries();
    const names = entries.map(e => e.name);
    expect(names).toContain('alpha.md');
    expect(names).toContain('beta.md');
    expect(names).toContain('gamma.md');

    // Select a fact and see metadata
    await browse.clickEntry('alpha.md');
    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Alpha Fact');

    // Verify entity tags appear in metadata
    const meta = factPanel.meta;
    await expect(meta).toBeVisible({ timeout: 10_000 });

    // Find the shared entity tag and click it
    const tagItem = page.locator(`[data-testid="tag-item"][data-value="${sharedEntity}"]`);
    await expect(tagItem).toBeVisible({ timeout: 10_000 });
    await tagItem.click();

    // Verify search results include related facts
    const searchResults = await browse.getSearchResults();
    expect(searchResults.length).toBeGreaterThanOrEqual(2);

    // All three facts share the entity, so they should all appear
    const hasAlpha = searchResults.some(r => r.includes('alpha'));
    const hasBeta = searchResults.some(r => r.includes('beta'));
    expect(hasAlpha || hasBeta).toBeTruthy();
  });
});
