import { test, expect } from '../../fixtures/knomit.js';
import { clearFilters, enterChrono, exitChrono, filterByEntity } from '../../helpers/nav.js';

test.describe('Chrono View (Recent)', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('Sort by Recent enters chrono view and shows recently modified facts', async ({ page }) => {
    // Sort by Recent switches the library to the chrono list
    await enterChrono(page);

    // Should show chrono items with data-testid="chrono-item"
    const items = page.getByTestId('chrono-item');
    const count = await items.count();
    expect(count).toBeGreaterThan(0);
  });

  test('chrono items have data-path attribute', async ({ page }) => {
    await enterChrono(page);
    const items = page.getByTestId('chrono-item');

    const firstPath = await items.first().getAttribute('data-path');
    expect(firstPath).toBeTruthy();
    expect(firstPath).toContain('/');
  });

  test('filter via filter bar narrows the chrono list', async ({ page }) => {
    await enterChrono(page);
    const items = page.getByTestId('chrono-item');
    const initialCount = await items.count();

    // Narrow with an entity chip (free text switches to a relevance search)
    await filterByEntity(page, 'PostgreSQL');

    const filteredCount = await items.count();
    expect(filteredCount).toBeLessThan(initialCount);
    // the seed has exactly four PostgreSQL facts
    expect(filteredCount).toBe(4);
  });

  test('clearing filter resets the chrono list', async ({ page }) => {
    await enterChrono(page);
    const items = page.getByTestId('chrono-item');

    await filterByEntity(page, 'PostgreSQL');
    const filteredCount = await items.count();

    // Clear the chip
    await clearFilters(page);
    const resetCount = await items.count();
    expect(resetCount).toBeGreaterThan(filteredCount);
  });

  test('Sort by Path exits chrono view back to tree', async ({ page }) => {
    await enterChrono(page);

    await exitChrono(page);
    await expect(page.getByTestId('left-panel')).toBeVisible();
  });

  test('type filter chip narrows chrono results', async ({ page }) => {
    await enterChrono(page);
    const items = page.getByTestId('chrono-item');

    // Register response listener before the action that triggers the API call
    const responsePromise = page.waitForResponse(resp =>
      resp.url().includes('/facts') && resp.url().includes('sort=recent') && resp.url().includes('type=observation')
    );

    // Type a type filter prefix in the filter bar
    const filterInput = page.locator('#filter-input');
    await filterInput.fill('type:observation');
    await filterInput.press('Enter');

    await responsePromise;
  });
});
