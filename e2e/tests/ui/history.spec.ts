import { test, expect } from '../../fixtures/knomit.js';
import { HistoryPage } from '../../pages/history.page.js';

test.describe('History Mode', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('pressing "h" enters history mode and shows timeline', async ({ page }) => {
    // Press 'h' to enter history mode (keyboard shortcut from LeftPanel.tsx)
    await page.keyboard.press('h');
    const timeline = page.getByTestId('history-timeline');
    await expect(timeline).toBeVisible();
  });

  test('timeline shows commits with data-hash attribute', async ({ page }) => {
    await page.keyboard.press('h');
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
    await page.keyboard.press('h');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });

    const commits = await history.getCommits();
    // Seed data is written in multiple batches, so there should be multiple commits
    expect(commits.length).toBeGreaterThanOrEqual(2);
  });

  test('clicking a commit shows fact in right panel', async ({ page }) => {
    await page.keyboard.press('h');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });

    const commits = await history.getCommits();
    expect(commits.length).toBeGreaterThan(0);

    // Click the first commit
    await history.clickCommit(commits[0].hash);

    // The right panel should show either commit-detail (multi-file) or fact-title (single-file).
    // Seed commits are typically single-file, so the fact renders directly.
    const factTitle = page.getByTestId('fact-title');
    const commitDetail = page.getByTestId('commit-detail');
    // Wait for either to appear
    await expect(factTitle.or(commitDetail)).toBeVisible({ timeout: 10_000 });
  });

  test('pressing Escape exits history mode back to browse', async ({ page }) => {
    await page.keyboard.press('h');
    await expect(page.getByTestId('history-timeline')).toBeVisible();

    await page.keyboard.press('Escape');
    await expect(page.getByTestId('left-panel')).toBeVisible();
  });
});
