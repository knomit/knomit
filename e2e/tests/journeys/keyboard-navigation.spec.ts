import { test, expect } from '../../fixtures/knomit.js';
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

  test('history view: selecting a commit shows files in right panel, clicking a file shows fact', async ({ page }) => {
    // Enter history view
    await page.keyboard.press('3');
    const timeline = page.getByTestId('history-timeline');
    await expect(timeline).toBeVisible();

    // Wait for commits to load
    const commits = page.getByTestId('history-commit');
    await commits.first().waitFor({ timeout: 10_000 });
    const count = await commits.count();
    expect(count).toBeGreaterThan(1);

    // Select the second commit (first is auto-selected on load; clicking it
    // again clears rightSelection without re-triggering auto-select)
    await commits.nth(1).click();
    await page.waitForTimeout(1000);

    // Files are listed in the right panel as commit-file entries
    const commitFiles = page.getByTestId('commit-file');
    await commitFiles.first().waitFor({ timeout: 10_000 });
    const fileCount = await commitFiles.count();
    expect(fileCount).toBeGreaterThan(0);

    // Click the first file to load the fact
    await commitFiles.first().click();

    // Right panel should show the fact
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    const firstTitle = await factPanel.getTitle();
    expect(firstTitle.length).toBeGreaterThan(0);
  });

  test('chrono view: down key selects next item and right panel updates', async ({ page }) => {
    // Switch to chrono view
    await page.keyboard.press('2');
    const chronoList = page.getByTestId('chrono-list');
    await expect(chronoList).toBeVisible();

    // Wait for chrono items to load
    const items = page.getByTestId('chrono-item');
    await items.first().waitFor({ timeout: 10_000 });
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
