import { test, expect } from '../../fixtures/knomit.js';

test.describe('Reference Navigation', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('clicking local ref opens referenced fact in history mode', async ({ page }) => {
    // Step 1: Search for the fact with local refs
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('query planner');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(1000);

    // Click the query-planning fact
    const factEntry = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
    if (!await factEntry.isVisible({ timeout: 5_000 }).catch(() => false)) {
      test.skip(true, 'query-planning fact not found');
      return;
    }
    await factEntry.click();
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
    const titleBefore = await page.getByTestId('fact-title').textContent();
    console.log(`Step 1 - fact loaded: "${titleBefore}"`);
    expect(titleBefore).toContain('Query Planning');

    // Step 2: Click local ref → should switch to history
    const localRef = page.locator('span').filter({ hasText: /^→/ }).first();
    await expect(localRef).toBeVisible({ timeout: 5_000 });
    const refText = await localRef.textContent();
    console.log(`Step 2 - clicking ref: ${refText}`);
    await localRef.click();

    // Step 3: Verify history mode and correct fact loaded
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 10_000 });
    // Wait for the fact title to change from "Query Planning" to the referenced fact
    await expect(page.getByTestId('fact-title')).not.toContainText('Query Planning', { timeout: 10_000 });
    const titleInHistory = await page.getByTestId('fact-title').textContent();
    console.log(`Step 3 - fact in history: "${titleInHistory}"`);

    // Must NOT be "Query Planning" — should be the referenced fact
    expect(titleInHistory).not.toContain('Query Planning');

    // Step 4: Navigate back — blur any focused input first
    await page.locator('body').click(); // ensure no input focused
    await page.waitForTimeout(200);

    // Check what's focused
    const focusedTag = await page.evaluate(() => document.activeElement?.tagName);
    console.log(`Step 4 - focused element before Backspace: ${focusedTag}`);

    await page.keyboard.press('Backspace');
    await page.waitForTimeout(1000);

    // Step 5: Verify we're back to tree mode with Query Planning
    const viewAfterBack = await page.evaluate(() => {
      // Check if history timeline is visible (still in history mode)
      return document.querySelector('[data-testid="history-timeline"]') ? 'history' : 'tree/chrono';
    });
    console.log(`Step 5 - view after back: ${viewAfterBack}`);

    const titleAfterBack = await page.getByTestId('fact-title').textContent({ timeout: 5_000 }).catch(() => 'NOT_VISIBLE');
    console.log(`Step 5 - fact after back: "${titleAfterBack}"`);

    // Should NOT be in history mode
    expect(viewAfterBack).not.toBe('history');
    // Should show Query Planning again
    expect(titleAfterBack).toContain('Query Planning');
  });
});
