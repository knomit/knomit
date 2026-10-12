import { test, expect } from '../../fixtures/knomit.js';
import { enterChrono, openFirstFact, openHistory } from '../../helpers/nav.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Keyboard Navigation Across Views', () => {
  let factPanel: FactPanel;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    factPanel = new FactPanel(page);
    await page.waitForLoadState('domcontentloaded');
    // Wait for initial browse to load
    await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
  });

  test('history: the active version expands to its files and the open fact is marked "here"', async ({ page }) => {
    await openFirstFact(page);
    const openTitle = await factPanel.getTitle();

    // Open the version timeline of the open fact
    await openHistory(page);
    await expect(page.getByTestId('timeline-row').first()).toBeVisible();

    // The active row discloses the commit's files when clicked
    await expect(page.getByTestId('timeline-file-row')).toHaveCount(0);
    await page.getByTestId('timeline-row').first().click();
    const files = page.getByTestId('timeline-file-row');
    await files.first().waitFor({ timeout: 10_000 });
    expect(await files.count()).toBeGreaterThan(0);

    // The fact being viewed is the one marked "here"
    await expect(page.locator('[data-testid="timeline-file-row"][data-self="true"]')).toHaveCount(1);
    await expect(page.getByTestId('timeline-here-marker')).toBeVisible();
    await expect(factPanel.title).toHaveText(openTitle);
  });

  test('chrono view: down key selects next item and right panel updates', async ({ page }) => {
    // Switch to chrono view
    await enterChrono(page);

    const items = page.getByTestId('chrono-item');
    const count = await items.count();
    expect(count).toBeGreaterThan(1);

    // First item should auto-select and load in right panel
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    const firstTitle = await factPanel.getTitle();
    expect(firstTitle.length).toBeGreaterThan(0);

    // Press ArrowDown to select the next item
    await page.keyboard.press('ArrowDown');
    await page.waitForTimeout(1000);

    // Right panel should update with the new fact
    const secondTitle = await factPanel.getTitle();
    expect(secondTitle).not.toBe(firstTitle);

    // Press ArrowDown again -- third fact
    await page.keyboard.press('ArrowDown');
    await page.waitForTimeout(1000);
    const thirdTitle = await factPanel.getTitle();
    expect(thirdTitle).not.toBe(secondTitle);
  });
});
