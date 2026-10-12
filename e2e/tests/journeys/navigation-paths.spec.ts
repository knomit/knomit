import type { Page } from '@playwright/test';
import { test, expect } from '../../fixtures/knomit.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

/** Navigate into the first directory until a non-directory entry appears. */
async function navigateToFirstFact(page: Page) {
  const factEntries = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]'));
  const dirEntries = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]'));

  // Try up to 3 levels of directory descent to find a fact file
  for (let depth = 0; depth < 3; depth++) {
    const factCount = await factEntries.count();
    if (factCount > 0) break;
    // Click the first directory entry to descend; wait for browse response
    const browseResponse = page.waitForResponse(r => r.url().includes('/topics'), { timeout: 5000 });
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

    // Open history from the fact's version chip
    await page.getByTestId('version-walker').click();

    // The stats view must never appear (same fact, no intermediate summary state)
    await expect(page.getByTestId('stats-view')).not.toBeVisible({ timeout: 1000 });

    // The timeline appears and the fact stays on screen
    await expect(page.getByTestId('timeline-nav')).toBeVisible({ timeout: 5_000 });
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    expect(await factPanel.getTitle()).toBe(treeFactTitle);
    await expect(page.getByTestId('stats-view')).not.toBeVisible();

    // The timeline lists at least the fact's own version
    await expect(page.getByTestId('timeline-row').first()).toBeVisible({ timeout: 10_000 });
  });

  test('tree → history → return to live: back in the library with the same fact', async ({ page }) => {
    const factPanel = new FactPanel(page);

    await navigateToFirstFact(page);
    const factEntries = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]'));
    await factEntries.first().click();
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    const treeFactTitle = await factPanel.getTitle();

    await page.getByTestId('version-walker').click();
    await expect(page.getByTestId('timeline-nav')).toBeVisible({ timeout: 5_000 });

    await page.getByTestId('timeline-return-live').click();

    await expect(page.getByTestId('timeline-nav')).toHaveCount(0, { timeout: 5_000 });
    await expect(page.getByTestId('left-panel')).toBeVisible({ timeout: 5_000 });
    await expect(factPanel.title).toBeVisible({ timeout: 5_000 });
    expect(await factPanel.getTitle()).toBe(treeFactTitle);
  });
});
