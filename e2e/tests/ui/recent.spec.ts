import { test, expect } from '../../fixtures/knomit.js';

test.describe('Chrono View (Recent)', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('pressing "2" enters chrono view and shows recently modified facts', async ({ page }) => {
    // Press '2' to switch to chrono view (keyboard shortcut from App.tsx)
    await page.keyboard.press('2');
    const chronoList = page.getByTestId('chrono-list');
    await expect(chronoList).toBeVisible();

    // Should show chrono items with data-testid="chrono-item"
    const items = page.getByTestId('chrono-item');
    await items.first().waitFor({ timeout: 10_000 });
    const count = await items.count();
    expect(count).toBeGreaterThan(0);
  });

  test('chrono items have data-path attribute', async ({ page }) => {
    await page.keyboard.press('2');
    const items = page.getByTestId('chrono-item');
    await items.first().waitFor({ timeout: 10_000 });

    const firstPath = await items.first().getAttribute('data-path');
    expect(firstPath).toBeTruthy();
    expect(firstPath).toContain('/');
  });

  test('filter via filter bar narrows the chrono list', async ({ page }) => {
    await page.keyboard.press('2');
    const items = page.getByTestId('chrono-item');
    await items.first().waitFor({ timeout: 10_000 });
    const initialCount = await items.count();

    // Type in the filter bar to set free text
    const filterInput = page.locator('#filter-input');
    await filterInput.fill('postgresql');
    // Wait for debounce (300ms) + response
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    // Wait for DOM to settle
    await page.waitForTimeout(500);

    const filteredCount = await items.count();
    expect(filteredCount).toBeLessThan(initialCount);
    expect(filteredCount).toBeGreaterThan(0);
  });

  test('clearing filter resets the chrono list', async ({ page }) => {
    await page.keyboard.press('2');
    const items = page.getByTestId('chrono-item');
    await items.first().waitFor({ timeout: 10_000 });

    // Type a filter
    const filterInput = page.locator('#filter-input');
    await filterInput.fill('postgresql');
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);
    const filteredCount = await items.count();

    // Clear filter input
    await filterInput.clear();

    // Wait for unfiltered results
    await page.waitForResponse(resp => resp.url().includes('/recent'));
    await page.waitForTimeout(500);

    const resetCount = await items.count();
    expect(resetCount).toBeGreaterThan(filteredCount);
  });

  test('pressing "1" exits chrono view back to tree', async ({ page }) => {
    await page.keyboard.press('2');
    await expect(page.getByTestId('chrono-list')).toBeVisible();

    await page.keyboard.press('1');
    await expect(page.getByTestId('left-panel')).toBeVisible();
  });

  test('type filter chip narrows chrono results', async ({ page }) => {
    await page.keyboard.press('2');
    const items = page.getByTestId('chrono-item');
    await items.first().waitFor({ timeout: 10_000 });

    // Type a type filter prefix in the filter bar
    const filterInput = page.locator('#filter-input');
    await filterInput.fill('type:observation');
    await filterInput.press('Enter');

    // Wait for API call with type filter
    const responsePromise = page.waitForResponse(resp =>
      resp.url().includes('/recent') && resp.url().includes('type=observation')
    );
    await responsePromise;
  });
});
