import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Fact Edit', () => {
  test('create a fact via API and verify it appears in UI', async ({ freshKnomit, page }) => {
    // Create a fact via API
    const res = await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/kb/test-fact.md`,
      {
        data: {
          content: `---
type: observation
confidence: 0.9
entities:
  - playwright
refs: []
---
# Test Fact

Created by e2e test.`,
        },
      },
    );
    expect(res.ok()).toBeTruthy();

    // Navigate to it in the UI
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');
    const browse = new BrowsePage(page);
    await browse.clickEntry('test-fact.md');
    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Test Fact');
  });

  test('edit an existing fact via API and verify update in UI', async ({ freshKnomit, page }) => {
    // Seed a fact
    await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/kb/editable.md`,
      {
        data: {
          content: `---
type: observation
confidence: 0.5
entities:
  - edit-test
refs: []
---
# Editable Fact

Original content.`,
        },
      },
    );

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');
    const browse = new BrowsePage(page);
    await browse.clickEntry('editable.md');

    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Editable Fact');
    await expect(factPanel.body).toContainText('Original content');

    // Update the fact via API (the editor textarea only appears for parse errors)
    const updateRes = await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/kb/editable.md`,
      {
        data: {
          content: `---
type: observation
confidence: 0.8
entities:
  - edit-test
refs: []
---
# Editable Fact

Updated content via e2e test.`,
        },
      },
    );
    expect(updateRes.ok()).toBeTruthy();

    // Reload page to see the updated fact
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');
    const browse2 = new BrowsePage(page);
    await browse2.clickEntry('editable.md');

    const factPanel2 = new FactPanel(page);
    await expect(factPanel2.body).toContainText('Updated content via e2e test');
  });
});
