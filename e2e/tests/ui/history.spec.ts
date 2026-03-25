import { test, expect } from '../../fixtures/knomit.js';
import { HistoryPage } from '../../pages/history.page.js';

test.describe('History Mode', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('pressing "3" enters history view and shows timeline', async ({ page }) => {
    // Press '3' to switch to history view (keyboard shortcut from App.tsx)
    await page.keyboard.press('3');
    const timeline = page.getByTestId('history-timeline');
    await expect(timeline).toBeVisible();
  });

  test('timeline shows commits with data-hash attribute', async ({ page }) => {
    await page.keyboard.press('3');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });

    const commits = await history.getCommits();
    expect(commits.length).toBeGreaterThan(0);
    // Each commit should have a non-empty hash
    for (const c of commits) {
      expect(c.hash).toBeTruthy();
      expect(c.hash.length).toBeGreaterThanOrEqual(7);
    }
  });

  test('multiple commits exist from seed batches', async ({ page }) => {
    await page.keyboard.press('3');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });

    const commits = await history.getCommits();
    // Seed data is written in multiple batches, so there should be multiple commits
    expect(commits.length).toBeGreaterThanOrEqual(2);
  });

  test('expanding a commit and clicking a file shows fact in right panel', async ({ page }) => {
    await page.keyboard.press('3');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });

    const commits = await history.getCommits();
    expect(commits.length).toBeGreaterThan(0);

    // Click the first commit to select it
    await history.clickCommit(commits[0].hash);

    // Press Enter to expand the commit (shows file list)
    await page.keyboard.press('Enter');
    await page.waitForTimeout(1000);

    // Click a file within the expanded commit
    const expandedFiles = page.locator('[data-testid="history-commit"] + div div[style*="cursor: pointer"]');
    const fileCount = await expandedFiles.count();
    if (fileCount > 0) {
      await expandedFiles.first().click();
      // The right panel should show the fact
      const factTitle = page.getByTestId('fact-title');
      await expect(factTitle).toBeVisible({ timeout: 10_000 });
    }
  });

  test('pressing "1" exits history view back to tree', async ({ page }) => {
    await page.keyboard.press('3');
    await expect(page.getByTestId('history-timeline')).toBeVisible();

    await page.keyboard.press('1');
    await expect(page.getByTestId('left-panel')).toBeVisible();
  });
});
