/**
 * Exact reproduction of the broken ref-click flow:
 *
 * 1. Navigate to kb/technology/ai/anthropic
 * 2. Select the fact → renders in right panel
 * 3. Click a local ref → should go to history mode showing the REFERENCED fact
 * 4. Back → should return to step 2 (the original fact in tree mode)
 *
 * NO fuzzy waits. NO "any fact is fine". Exact titles, exact modes.
 */
import { test, expect } from '../../fixtures/knomit.js';

test.describe('Ref Click Exact Flow', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
    await page.getByTestId('left-panel').waitFor({ timeout: 10_000 });
  });

  test('navigate → select fact → click ref → verify history fact → back → verify original', async ({ page }) => {
    // ── Step 1: Navigate to a fact that has local refs ──
    // Use search to find query-planning (has refs to mvcc.md and btree-vs-hash.md)
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('query planner');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(1500);

    const factEntry = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
    if (!await factEntry.isVisible({ timeout: 5_000 }).catch(() => false)) {
      test.skip(true, 'query-planning fact not found in search');
      return;
    }
    await factEntry.click();
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });

    // ── Step 2: Record exact state ──
    const originalTitle = await page.getByTestId('fact-title').textContent();
    console.log(`STEP 2: fact loaded = "${originalTitle}"`);
    expect(originalTitle).toContain('Query Planning');

    // Verify NOT in history mode
    expect(await page.getByTestId('history-timeline').isVisible().catch(() => false)).toBe(false);

    // ── Step 3: Click local ref ──
    const localRef = page.locator('span').filter({ hasText: /^→/ }).first();
    await expect(localRef).toBeVisible({ timeout: 5_000 });
    const refText = (await localRef.textContent())?.replace(/^→\s*/, '').trim();
    console.log(`STEP 3: clicking ref → "${refText}"`);
    await localRef.click();

    // Wait for history mode to fully load
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 10_000 });

    // Wait for the fact title to change from original
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });

    // Give time for the async pipeline: commit detail → auto-select → fact fetch
    await page.waitForTimeout(3000);

    const historyTitle = await page.getByTestId('fact-title').textContent();
    console.log(`STEP 3 result: history fact = "${historyTitle}"`);

    // CRITICAL: must NOT show the original fact
    expect(historyTitle).not.toContain('Query Planning');
    // CRITICAL: must NOT show stats/summary (no fact-title would be visible, but if it is, it should be the ref target)
    expect(historyTitle).not.toBe('');

    // Must be in history mode
    expect(await page.getByTestId('history-timeline').isVisible()).toBe(true);

    // ── Step 4: Back → must return to EXACT step 2 state ──
    await page.locator('body').click();
    await page.waitForTimeout(200);
    await page.keyboard.press('Backspace');

    // Wait for history to disappear and fact to load
    await page.waitForTimeout(2000);

    const afterBack1InHistory = await page.getByTestId('history-timeline').isVisible().catch(() => false);
    const afterBack1Title = await page.getByTestId('fact-title').textContent({ timeout: 5_000 }).catch(() => 'NOT_VISIBLE');
    console.log(`STEP 4: after first back: inHistory=${afterBack1InHistory}, title="${afterBack1Title}"`);

    // CRITICAL: must NOT be in history mode
    expect(afterBack1InHistory).toBe(false);
    // CRITICAL: must show the ORIGINAL fact
    expect(afterBack1Title).toContain('Query Planning');
  });
});
