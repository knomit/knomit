import { test, expect } from '../../fixtures/knomit.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Search in Recent Mode', () => {
  let factPanel: FactPanel;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    factPanel = new FactPanel(page);
    await page.waitForLoadState('domcontentloaded');
    // Enter recent mode
    await page.keyboard.press('r');
    const recentList = page.getByTestId('recent-list');
    await expect(recentList).toBeVisible();
    // Wait for items to load
    await page.getByTestId('recent-item').first().waitFor({ timeout: 10_000 });
  });

  test('recent mode shows facts and selecting one opens it in the right panel', async ({ page }) => {
    const items = page.getByTestId('recent-item');
    const count = await items.count();
    expect(count).toBeGreaterThan(0);

    // Click the first recent item
    await items.first().click();

    // Right panel should show the selected fact
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
  });

  test('selecting different recent items updates the right panel', async ({ page }) => {
    const items = page.getByTestId('recent-item');
    const count = await items.count();
    if (count < 2) return;

    // Click first item
    await items.first().click();
    await expect(factPanel.title).toBeVisible();
    const firstTitle = await factPanel.getTitle();

    // Click second item
    await items.nth(1).click();
    await page.waitForTimeout(500);
    const secondTitle = await factPanel.getTitle();

    // Titles should differ (different facts)
    expect(secondTitle).not.toBe(firstTitle);
  });

  test('searching in recent mode filters the list', async ({ page }) => {
    const items = page.getByTestId('recent-item');
    const initialCount = await items.count();
    expect(initialCount).toBeGreaterThan(0);

    // Search for a specific term
    const searchInput = page.getByTestId('recent-search-input');
    await searchInput.fill('postgresql');
    // Wait for debounce + API response
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);

    const filteredCount = await items.count();
    expect(filteredCount).toBeLessThan(initialCount);
    expect(filteredCount).toBeGreaterThan(0);

    // The filtered results should relate to postgresql
    const firstPath = await items.first().getAttribute('data-path');
    expect(firstPath).toContain('postgresql');
  });

  test('selecting a filtered recent item opens the correct fact', async ({ page }) => {
    const searchInput = page.getByTestId('recent-search-input');
    await searchInput.fill('postgresql');
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);

    const items = page.getByTestId('recent-item');
    await items.first().waitFor({ timeout: 5_000 });
    const path = await items.first().getAttribute('data-path');
    expect(path).toBeTruthy();

    // Click it
    await items.first().click();

    // Right panel should show a fact related to the path
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
  });

  test('arrow keys navigate recent items and update right panel', async ({ page }) => {
    const items = page.getByTestId('recent-item');
    const count = await items.count();
    if (count < 2) return;

    // First item should auto-select and show in right panel
    await expect(factPanel.title).toBeVisible();
    const firstTitle = await factPanel.getTitle();

    // ArrowDown to move to second item
    await page.keyboard.press('ArrowDown');
    await page.waitForTimeout(500);
    const secondTitle = await factPanel.getTitle();
    expect(secondTitle).not.toBe(firstTitle);

    // ArrowUp to go back to first
    await page.keyboard.press('ArrowUp');
    await page.waitForTimeout(500);
    const backTitle = await factPanel.getTitle();
    expect(backTitle).toBe(firstTitle);
  });

  test('pressing Escape from recent then searching works end-to-end', async ({ page }) => {
    // From recent mode, exit to browse
    await page.keyboard.press('Escape');
    const searchInput = page.getByTestId('search-input');
    await expect(searchInput).toBeVisible();

    // Now search from browse mode
    await searchInput.fill('PostgreSQL');
    await page.waitForResponse(resp => resp.url().includes('/search'));

    // Results should appear and selecting one updates the right panel
    const results = page.getByTestId('search-result');
    await results.first().waitFor({ timeout: 10_000 });
    await results.first().click();
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
  });

  test('clearing recent search resets to full recent list', async ({ page }) => {
    const items = page.getByTestId('recent-item');
    const initialCount = await items.count();

    // Filter
    const searchInput = page.getByTestId('recent-search-input');
    await searchInput.fill('postgresql');
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);
    const filteredCount = await items.count();
    expect(filteredCount).toBeLessThan(initialCount);

    // Clear via the clear button
    const clearBtn = page.getByTestId('recent-search-clear');
    await clearBtn.click();
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);

    const resetCount = await items.count();
    expect(resetCount).toBeGreaterThan(filteredCount);
  });
});
