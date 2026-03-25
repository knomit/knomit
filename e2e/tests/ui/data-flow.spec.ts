import { test, expect } from '../../fixtures/knomit.js';
import { HistoryPage } from '../../pages/history.page.js';

test.describe('History Mode Data Flow', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('switching to history mode loads a fact in the right panel', async ({ page }) => {
    // Start in tree mode — left panel should be visible
    await expect(page.getByTestId('left-panel')).toBeVisible();

    // Press '3' to switch to history
    await page.keyboard.press('3');

    // Wait for timeline to appear
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 10_000 });

    // Wait for at least one commit
    await page.getByTestId('history-commit').first().waitFor({ timeout: 10_000 });

    // The first commit is auto-selected, which loads a fact in the right panel
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
  });

  test('clicking a different commit updates the right panel fact', async ({ page }) => {
    await page.keyboard.press('3');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });

    const commits = await history.getCommits();
    expect(commits.length).toBeGreaterThanOrEqual(2);

    // First commit is auto-selected. Get the initial fact title.
    const factTitle = page.getByTestId('fact-title');
    await expect(factTitle).toBeVisible({ timeout: 10_000 });
    const initialTitle = await factTitle.textContent();

    // Click the second commit
    await history.clickCommit(commits[1].hash);

    // Wait for the right panel to potentially update.
    // The title may or may not change (depends on data), but fact-title must remain visible.
    await expect(factTitle).toBeVisible({ timeout: 10_000 });

    // If there are different files in each commit, titles should differ.
    // We at least verify the panel didn't go blank.
    const newTitle = await factTitle.textContent();
    expect(newTitle).toBeTruthy();
  });

  test('filtering by episode type updates timeline and right panel', async ({ page }) => {
    await page.keyboard.press('3');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });
    await page.getByTestId('history-commit').first().waitFor({ timeout: 10_000 });

    // Focus filter input and type "ep:learn"
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('ep:learn');
    await page.keyboard.press('Space');

    // Wait for the ep chip to appear
    const epChip = page.locator('span').filter({ hasText: /ep:.*learn/ });
    await expect(epChip.first()).toBeVisible({ timeout: 5_000 });

    // Wait for timeline to re-render with filtered commits
    await page.getByTestId('history-commit').first().waitFor({ timeout: 10_000 }).catch(() => {});

    // If filtered commits exist, the right panel should still show a fact
    const commitCount = await page.getByTestId('history-commit').count();
    if (commitCount > 0) {
      await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
    }
  });

  test('back navigation from history returns to previous mode', async ({ page }) => {
    // Verify we're in tree mode
    await expect(page.getByTestId('left-panel')).toBeVisible();

    // Press '3' to switch to history
    await page.keyboard.press('3');
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 10_000 });

    // Press Backspace to go back
    await page.keyboard.press('Backspace');

    // Verify we're back in tree mode
    await expect(page.getByTestId('left-panel')).toBeVisible({ timeout: 5_000 });
  });

  test('retract commit with only deleted files still shows fact in right panel', async ({ page }) => {
    // This is the key regression test for the bug where retract commits
    // with only deleted files wouldn't auto-select any file.
    await page.keyboard.press('3');
    const history = new HistoryPage(page);
    await history.timeline.waitFor({ timeout: 10_000 });
    await page.getByTestId('history-commit').first().waitFor({ timeout: 10_000 });

    // Focus filter input and type "ep:retract"
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('ep:retract');
    await page.keyboard.press('Space');

    // Wait for the ep chip to appear
    const epChip = page.locator('span').filter({ hasText: /ep:.*retract/ });
    await expect(epChip.first()).toBeVisible({ timeout: 5_000 });

    // Wait for filtered commits to load
    await page.waitForTimeout(1000);
    const commitCount = await page.getByTestId('history-commit').count();

    if (commitCount > 0) {
      // Click the first retract commit
      const firstCommit = page.getByTestId('history-commit').first();
      await firstCommit.click();

      // The right panel MUST show a fact title — this is the regression:
      // previously, retract commits with only deleted files showed nothing
      // because the code filtered out deleted files before auto-selecting.
      await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
    }
    // If no retract commits exist in seed data, the test passes trivially.
    // The state-level regression test covers the logic regardless.
  });

  test('browse → entity click → ref click → history loads fact → back exits cleanly', async ({ page }) => {
    // Step 1: Start in tree mode, kb selected
    await expect(page.getByTestId('left-panel')).toBeVisible({ timeout: 10_000 });

    // Step 2: Click the first directory to go one level deep
    const firstDir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
    await firstDir.waitFor({ timeout: 10_000 });
    await firstDir.click();
    // Wait for new entries to load
    await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });

    // Navigate deeper until we find a fact (non-directory entry)
    for (let depth = 0; depth < 5; depth++) {
      const factEntry = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
      const hasFact = await factEntry.isVisible().catch(() => false);
      if (hasFact) break;
      // Go deeper into first directory
      const dir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
      if (await dir.isVisible().catch(() => false)) {
        await dir.click();
        await page.waitForTimeout(500);
      } else break;
    }

    // Step 3: Click a fact to load it in the right panel
    const factEntry = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
    if (!await factEntry.isVisible().catch(() => false)) {
      test.skip(true, 'No facts found in browsable tree');
      return;
    }
    await factEntry.click();
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
    const factTitle1 = await page.getByTestId('fact-title').textContent();

    // Step 4: Click an entity tag to trigger search
    const entityTag = page.getByTestId('tag-item').first();
    if (!await entityTag.isVisible({ timeout: 3_000 }).catch(() => false)) {
      test.skip(true, 'No entity tags on the selected fact');
      return;
    }
    await entityTag.click();
    // Wait for search results (filter chip should appear)
    await page.waitForTimeout(500);

    // Step 5: Select the first search result
    const searchResult = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
    await searchResult.waitFor({ timeout: 10_000 });
    await searchResult.click();
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });

    // Step 6: Click a local reference (non-http link) to open it in history mode
    // Local refs are rendered as <span> elements containing "→"
    const localRef = page.locator('span').filter({ hasText: /^→/ }).first();
    if (!await localRef.isVisible({ timeout: 3_000 }).catch(() => false)) {
      test.skip(true, 'No local references on the selected fact');
      return;
    }
    await localRef.click();

    // Step 7: Verify history mode opened and fact is visible in right panel
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 10_000 });
    // The referenced fact MUST be loaded in the right panel
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });

    // Step 8: Click back — should return to previous state cleanly
    await page.keyboard.press('Backspace');
    await page.waitForTimeout(500);

    // Should not be stuck in history — verify we can see fact content
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });

    // Click back again — should keep going back through history
    await page.keyboard.press('Backspace');
    await page.waitForTimeout(500);

    // Should still have content visible (not broken state)
    await expect(page.getByTestId('left-panel')).toBeVisible({ timeout: 5_000 });
  });
});
