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

  // Regression: History button click with a fact open should (1) highlight that fact in
  // CommitPanel and (2) require only ONE back step to return to tree mode.
  test('History button with fact open: fact highlighted in CommitPanel, one back returns to tree', async ({ page }) => {
    // Use search to land on a specific fact at a known path.
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('mvcc');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(800);

    const factEntry = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
    if (!await factEntry.isVisible({ timeout: 5_000 }).catch(() => false)) {
      test.skip(true, 'mvcc fact not found in seed data');
      return;
    }
    await factEntry.click();
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
    const factPath = await factEntry.getAttribute('data-path');

    // Wait for FACT_LOADED to fire so factCommit is set in state.
    await page.waitForTimeout(800);

    // Click the History button (not keyboard shortcut) — the bug was this bypassed the manager.
    await page.getByTitle('History').click();
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 10_000 });

    // The fact should be highlighted in CommitPanel (same path, same commit).
    if (factPath) {
      await expect(page.locator(`[data-testid="commit-file"][data-path="${factPath}"]`)).toBeVisible({ timeout: 5_000 });
    }

    // ONE back step must return to tree mode (previously required two).
    await page.locator('body').click();
    await page.keyboard.press('Backspace');
    await page.waitForTimeout(500);
    await expect(page.getByTestId('history-timeline')).not.toBeVisible({ timeout: 3_000 });
    await expect(page.getByTestId('left-panel')).toBeVisible();
  });
});
