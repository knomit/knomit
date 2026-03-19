import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Fact Edit', () => {
  test('create a fact via API and verify it appears in UI', async ({ freshKnomit, page }) => {
    // Create a fact via API
    const res = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/test-fact.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [playwright]
refs: []
---
# Test Fact

Created by e2e test.`,
      },
    });
    expect(res.ok()).toBeTruthy();

    // Navigate to it in the UI
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');
    const browse = new BrowsePage(page);
    await browse.clickEntry('test-fact.md');
    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Test Fact');
  });

  test('edit an existing fact via the editor', async ({ freshKnomit, page }) => {
    // Seed a fact
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/editable.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.5
sources: 1
entities: [edit-test]
refs: []
---
# Editable Fact

Original content.`,
      },
    });

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');
    const browse = new BrowsePage(page);
    await browse.clickEntry('editable.md');

    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Editable Fact');

    // Edit the fact via the editor textarea
    await factPanel.editContent(`---
type: observation
domain: [testing]
confidence: 0.8
sources: 2
entities: [edit-test]
refs: []
---
# Editable Fact

Updated content via e2e test.`);
    await factPanel.save();

    // Verify the update took effect
    await expect(factPanel.body).toContainText('Updated content via e2e test');
  });
});
