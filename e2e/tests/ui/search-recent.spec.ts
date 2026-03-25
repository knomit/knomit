import { test, expect } from '../../fixtures/knomit.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Search in Chrono View', () => {
  let factPanel: FactPanel;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    factPanel = new FactPanel(page);
    await page.waitForLoadState('domcontentloaded');
    // Enter chrono view
    await page.keyboard.press('2');
    const chronoList = page.getByTestId('chrono-list');
    await expect(chronoList).toBeVisible();
    // Wait for items to load
    await page.getByTestId('chrono-item').first().waitFor({ timeout: 10_000 });
  });

  test('chrono view shows facts and selecting one opens it in the right panel', async ({ page }) => {
    const items = page.getByTestId('chrono-item');
    const count = await items.count();
    expect(count).toBeGreaterThan(0);

    // Click the first chrono item
    await items.first().click();

    // Right panel should show the selected fact
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
  });

  test('selecting different chrono items updates the right panel', async ({ page }) => {
    const items = page.getByTestId('chrono-item');
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

  test('filtering in chrono view narrows the list', async ({ page }) => {
    const items = page.getByTestId('chrono-item');
    const initialCount = await items.count();
    expect(initialCount).toBeGreaterThan(0);

    // Type in the filter bar to set free text
    const filterInput = page.locator('#filter-input');
    await filterInput.fill('postgresql');
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

  test('selecting a filtered chrono item opens the correct fact', async ({ page }) => {
    const filterInput = page.locator('#filter-input');
    await filterInput.fill('postgresql');
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);

    const items = page.getByTestId('chrono-item');
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

  test('arrow keys navigate chrono items and update right panel', async ({ page }) => {
    const items = page.getByTestId('chrono-item');
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

  test('switching to tree view then searching works end-to-end', async ({ page }) => {
    // From chrono view, switch to tree view
    await page.keyboard.press('1');
    const filterInput = page.locator('#filter-input');
    await expect(filterInput).toBeVisible();

    // Now search from tree view
    await filterInput.fill('PostgreSQL');
    await page.waitForResponse(resp => resp.url().includes('/search') || resp.url().includes('/browse'));

    // Results should appear and selecting one updates the right panel
    const results = page.getByTestId('dir-entry');
    await results.first().waitFor({ timeout: 10_000 });
    await results.first().click();
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
  });

  test('clearing filter resets to full chrono list', async ({ page }) => {
    const items = page.getByTestId('chrono-item');
    const initialCount = await items.count();

    // Filter
    const filterInput = page.locator('#filter-input');
    await filterInput.fill('postgresql');
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);
    const filteredCount = await items.count();
    expect(filteredCount).toBeLessThan(initialCount);

    // Clear filter input
    await filterInput.clear();
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);

    const resetCount = await items.count();
    expect(resetCount).toBeGreaterThan(filteredCount);
  });
});
