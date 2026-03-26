import { test, expect } from '../../fixtures/knomit.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

/** Navigate into the first directory until a non-directory entry appears. */
async function navigateToFirstFact(page: Parameters<typeof test>[1]['page']) {
  const factEntries = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]'));
  const dirEntries = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]'));

  // Try up to 3 levels of directory descent to find a fact file
  for (let depth = 0; depth < 3; depth++) {
    const factCount = await factEntries.count();
    if (factCount > 0) break;
    // Click the first directory entry to descend; wait for browse response
    const browseResponse = page.waitForResponse(r => r.url().includes('/browse'), { timeout: 5000 });
    await dirEntries.first().click({ timeout: 10_000 });
    await browseResponse;
  }
  await factEntries.first().waitFor({ timeout: 10_000 });
}

test.describe('Navigation Paths', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
    await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
  });

  test('tree → history: same fact shown immediately, no flash to summary', async ({ page }) => {
    const factPanel = new FactPanel(page);

    // Navigate into subdirectories until we find a fact file
    await navigateToFirstFact(page);
    const factEntries = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]'));
    await factEntries.first().click();

    // Wait for the fact to load in tree mode
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    const treeFactTitle = await factPanel.getTitle();
    expect(treeFactTitle.length).toBeGreaterThan(0);

    // Switch to history mode
    await page.keyboard.press('3');

    // Immediately after keypress: stats-view must never appear
    // (APPLY_NAV sets factPath atomically — no intermediate summary state)
    await expect(page.getByTestId('stats-view')).not.toBeVisible({ timeout: 1000 });

    // History timeline should appear
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 5_000 });

    // Right panel should show a fact (not the summary/stats view)
    // The fact title should appear and the stats view should NOT be visible
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('stats-view')).not.toBeVisible();

    // The commit panel (with commit files) should be visible
    const commitFiles = page.getByTestId('commit-file');
    await commitFiles.first().waitFor({ timeout: 10_000 });
    expect(await commitFiles.count()).toBeGreaterThan(0);
  });

  test('tree → history → NAV_BACK returns to tree with same fact', async ({ page }) => {
    const factPanel = new FactPanel(page);

    // Navigate into subdirectories until we find a fact file, then open it
    await navigateToFirstFact(page);
    const factEntries = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]'));
    await factEntries.first().click();
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    const treeFactTitle = await factPanel.getTitle();

    // Switch to history
    await page.keyboard.press('3');
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 5_000 });

    // Navigate back
    await page.keyboard.press('Backspace');

    // Should return to tree view showing the same fact
    await expect(page.getByTestId('left-panel')).toBeVisible({ timeout: 5_000 });
    await expect(factPanel.title).toBeVisible({ timeout: 5_000 });
    const backFactTitle = await factPanel.getTitle();
    expect(backFactTitle).toBe(treeFactTitle);
  });
});
