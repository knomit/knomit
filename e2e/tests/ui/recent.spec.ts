import { test, expect } from '../../fixtures/knomit.js';

test.describe('Recent Mode', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('networkidle');
  });

  test('pressing "r" enters recent mode and shows recently modified facts', async ({ page }) => {
    // Press 'r' to enter recent mode (keyboard shortcut from LeftPanel.tsx)
    await page.keyboard.press('r');
    const recentList = page.getByTestId('recent-list');
    await expect(recentList).toBeVisible();

    // Should show recent items with data-testid="recent-item"
    const items = page.getByTestId('recent-item');
    await items.first().waitFor({ timeout: 10_000 });
    const count = await items.count();
    expect(count).toBeGreaterThan(0);
  });

  test('recent items have data-path attribute', async ({ page }) => {
    await page.keyboard.press('r');
    const items = page.getByTestId('recent-item');
    await items.first().waitFor({ timeout: 10_000 });

    const firstPath = await items.first().getAttribute('data-path');
    expect(firstPath).toBeTruthy();
    expect(firstPath).toContain('/');
  });

  test('search within recent filters the list', async ({ page }) => {
    await page.keyboard.press('r');
    const items = page.getByTestId('recent-item');
    await items.first().waitFor({ timeout: 10_000 });
    const initialCount = await items.count();

    // Type in the recent search input
    const searchInput = page.getByTestId('recent-search-input');
    await searchInput.fill('postgresql');
    // Wait for debounce (300ms) + response
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    // Wait for DOM to settle
    await page.waitForTimeout(500);

    const filteredCount = await items.count();
    expect(filteredCount).toBeLessThan(initialCount);
    expect(filteredCount).toBeGreaterThan(0);
  });

  test('clear button resets the recent search filter', async ({ page }) => {
    await page.keyboard.press('r');
    const items = page.getByTestId('recent-item');
    await items.first().waitFor({ timeout: 10_000 });

    // Type a filter
    const searchInput = page.getByTestId('recent-search-input');
    await searchInput.fill('postgresql');
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);
    const filteredCount = await items.count();

    // Click clear button
    const clearBtn = page.getByTestId('recent-search-clear');
    await expect(clearBtn).toBeVisible();
    await clearBtn.click();

    // Wait for unfiltered results
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);

    const resetCount = await items.count();
    expect(resetCount).toBeGreaterThan(filteredCount);
  });

  test('pressing Escape exits recent mode back to browse', async ({ page }) => {
    await page.keyboard.press('r');
    await expect(page.getByTestId('recent-list')).toBeVisible();

    await page.keyboard.press('Escape');
    await expect(page.getByTestId('left-panel')).toBeVisible();
  });
});
