import { test, expect } from '../../fixtures/knomit.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Keyboard Navigation Across Modes', () => {
  let factPanel: FactPanel;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    factPanel = new FactPanel(page);
    await page.waitForLoadState('domcontentloaded');
    // Wait for initial browse to load
    await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
  });

  test('history mode: up/down keys load selected fact in right panel', async ({ page }) => {
    // Enter history mode
    await page.keyboard.press('h');
    const timeline = page.getByTestId('history-timeline');
    await expect(timeline).toBeVisible();

    // Wait for commits to load
    const commits = page.getByTestId('history-commit');
    await commits.first().waitFor({ timeout: 10_000 });
    const count = await commits.count();
    expect(count).toBeGreaterThan(1);

    // First commit should auto-load in the right panel
    await expect(factPanel.title).toBeVisible({ timeout: 10_000 });
    const firstTitle = await factPanel.getTitle();
    expect(firstTitle.length).toBeGreaterThan(0);

    // Press ArrowDown to select the next commit
    await page.keyboard.press('ArrowDown');
    await page.waitForTimeout(1000);

    // Right panel should update with the new commit's data
    await expect(factPanel.title).toBeVisible();
    const secondTitle = await factPanel.getTitle();
    // The title may or may not change (depends on whether the commit touches a different fact)
    // but the right panel should still be showing a fact
    expect(secondTitle.length).toBeGreaterThan(0);

    // Press ArrowUp to go back
    await page.keyboard.press('ArrowUp');
    await page.waitForTimeout(1000);
    const backTitle = await factPanel.getTitle();
    expect(backTitle).toBe(firstTitle);
  });

  test('recent mode after history: down key selects next item and right panel updates', async ({ page }) => {
    // First enter history mode — this sets historyCommit in state
    await page.keyboard.press('h');
    await expect(page.getByTestId('history-timeline')).toBeVisible();
    const commits = page.getByTestId('history-commit');
    await commits.first().waitFor({ timeout: 10_000 });
    // Select a commit so historyCommit is set
    await page.keyboard.press('ArrowDown');
    await page.waitForTimeout(500);

    // Now switch to recent mode — this must clear historyCommit
    // BUG FIX: ENTER_RECENT now clears historyCommit so the right panel
    // falls through to the fact-loading branch instead of commit-loading
    await page.keyboard.press('r');
    const recentList = page.getByTestId('recent-list');
    await expect(recentList).toBeVisible();

    // Wait for recent items to load
    const items = page.getByTestId('recent-item');
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

    // Press ArrowDown again — third fact
    await page.keyboard.press('ArrowDown');
    await page.waitForTimeout(1000);
    const thirdTitle = await factPanel.getTitle();
    expect(thirdTitle).not.toBe(secondTitle);
  });
});
