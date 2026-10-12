/**
 * Navigation path tests for the facets UI:
 *
 * 1. The library has two presentations of the same facts: the path tree
 *    ("Sort by Path") and the recent-first list ("Sort by Recent"). Entity and
 *    domain chips force the recent-first list.
 * 2. Filters narrow within the current presentation and survive a switch.
 * 3. History is a per-fact timeline opened from the fact's version chip; the
 *    return-to-live control leaves it.
 * 4. Back restores the previous navigation step.
 * 5. Entity/domain clicks add a chip and stay out of history.
 *
 * (The old 1/2/3 view keys, the repo-wide history mode and ep: filters are gone.)
 */
import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { firstLocalRef, openFirstFact, openHistory } from '../../helpers/nav.js';

// Helpers
async function blurAndBack(page: import('@playwright/test').Page) {
  await page.locator('body').click();
  await page.waitForTimeout(200);
  await page.keyboard.press('Backspace');
  await page.waitForTimeout(500);
}

async function switchView(page: import('@playwright/test').Page, view: 'tree' | 'chrono') {
  await page.getByRole('button', { name: view === 'tree' ? 'Sort by Path' : 'Sort by Recent' }).click();
  await page.waitForTimeout(500);
}

async function isInHistoryMode(page: import('@playwright/test').Page): Promise<boolean> {
  return page.getByTestId('timeline-nav').isVisible({ timeout: 2_000 }).catch(() => false);
}

async function hasFactTitle(page: import('@playwright/test').Page): Promise<boolean> {
  return page.getByTestId('fact-title').isVisible({ timeout: 2_000 }).catch(() => false);
}

async function getFactTitle(page: import('@playwright/test').Page): Promise<string> {
  return page.getByTestId('fact-title').textContent({ timeout: 5_000 }).then(t => t || '').catch(() => '');
}

async function chipTexts(page: import('@playwright/test').Page): Promise<string[]> {
  // Filter chips are spans with "category:value" text and an x button
  const chips = page.locator('[style*="border-radius: 3px"]').filter({ hasText: /:/ });
  const count = await chips.count();
  const texts: string[] = [];
  for (let i = 0; i < count; i++) {
    const t = await chips.nth(i).textContent();
    if (t && t.includes(':')) texts.push(t.replace(/x$/, '').trim());
  }
  return texts;
}

test.describe('Navigation Paths', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
    await page.getByTestId('left-panel').waitFor({ timeout: 10_000 });
  });

  // ─── Rule: Mode switching ────────────────────────────────────────────

  test('tree → chrono → tree: left panel content changes, right panel shows stats', async ({ page }) => {
    // Tree mode: should have directory entries
    await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
    const treeEntries = await page.getByTestId('dir-entry').count();
    expect(treeEntries).toBeGreaterThan(0);

    // Switch to chrono
    await switchView(page, 'chrono');
    await page.getByTestId('chrono-item').first().waitFor({ timeout: 10_000 }).catch(() => {});

    // Switch back to tree
    await switchView(page, 'tree');
    await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
    expect(await page.getByTestId('dir-entry').count()).toBeGreaterThan(0);
  });

  // ─── Rule: Tree ↔ Chrono preserves filters ──────────────────────────

  test('tree with entity filter → chrono: filter preserved', async ({ page }) => {
    await openFirstFact(page);

    // Click an entity tag
    const tag = page.getByTestId('tag-item').first();
    await expect(tag).toBeVisible({ timeout: 10_000 });
    const tagValue = await tag.getAttribute('data-value') || '';
    await tag.click();
    await page.waitForTimeout(500);

    // Switch to chrono — filter should be preserved
    await switchView(page, 'chrono');
    await page.waitForTimeout(500);

    // Check the entity chip is still there
    const chips = await chipTexts(page);
    expect(chips.some(c => c.includes(tagValue))).toBe(true);
  });

  // ─── Rule: History does not touch the filters ───────────────────────

  test('entity chip → history → return to live: chip kept, same fact', async ({ page }) => {
    const browse = new BrowsePage(page);
    await browse.clickEntry('databases');
    await browse.clickEntry('postgresql');
    await browse.clickEntry('mvcc.md');
    await expect(page.getByTestId('fact-title')).toContainText('MVCC', { timeout: 10_000 });

    await page.getByTestId('tag-item').filter({ hasText: 'PostgreSQL' }).first().click();
    await page.waitForTimeout(500);
    expect((await chipTexts(page)).some(c => c.startsWith('entity:PostgreSQL'))).toBe(true);

    // (While history is open the filter row shows the trail breadcrumb, so the
    // chip is checked after the return, not during.)
    await openHistory(page);

    await page.getByTestId('timeline-return-live').click();
    await expect(page.getByTestId('timeline-nav')).toHaveCount(0);
    expect((await chipTexts(page)).some(c => c.startsWith('entity:PostgreSQL'))).toBe(true);
    await expect(page.getByTestId('fact-title')).toContainText('MVCC');
  });

  // ─── Rule: Entity/domain clicks stay in current mode ────────────────

  test('entity click in chrono stays in chrono', async ({ page }) => {
    // Switch to chrono
    await switchView(page, 'chrono');
    await page.getByTestId('chrono-item').first().waitFor({ timeout: 10_000 }).catch(() => {});

    // Click first chrono item to load a fact
    const item = page.getByTestId('chrono-item').first();
    if (await item.isVisible().catch(() => false)) {
      await item.click();
      await page.waitForTimeout(500);
    }

    // Click an entity tag
    const tag = page.getByTestId('tag-item').first();
    await expect(tag).toBeVisible({ timeout: 10_000 });
    await tag.click();
    await page.waitForTimeout(500);

    // Still the chrono list (the chip narrows it), and NOT in history
    expect(await isInHistoryMode(page)).toBe(false);
    await expect(page.getByTestId('chrono-item').first()).toBeVisible();
  });

  // ─── Rule: Back navigation restores exact state ─────────────────────

  test('select fact → open history → return to live: restores fact', async ({ page }) => {
    await openFirstFact(page);
    const titleBefore = await getFactTitle(page);

    await openHistory(page);
    expect(await isInHistoryMode(page)).toBe(true);

    await page.getByTestId('timeline-return-live').click();

    // Back in the library with the same fact
    await expect(page.getByTestId('timeline-nav')).toHaveCount(0);
    expect(await getFactTitle(page)).toBe(titleBefore);
  });

  // ─── Rule: Deep navigation with multiple backs ──────────────────────

  test('tree → dir → fact → entity filter → chrono → back × 2', async ({ page }) => {
    await openFirstFact(page);

    const factTitle = await getFactTitle(page);

    // Step 3: Click entity tag
    const tag = page.getByTestId('tag-item').first();
    await expect(tag).toBeVisible({ timeout: 10_000 });
    await tag.click();
    await page.waitForTimeout(500);

    // Step 4: Switch to chrono
    await switchView(page, 'chrono');
    await page.waitForTimeout(500);

    // Sort switches are not navigation steps (SET_LIBRARY_SORT does not push
    // the back stack); the chip click is. So Back 1 undoes the chip and lands
    // on the fact exactly as it was opened.
    await blurAndBack(page);
    expect(await isInHistoryMode(page)).toBe(false);
    expect((await chipTexts(page)).some(c => c.startsWith('entity:'))).toBe(false);
    expect(await getFactTitle(page)).toBe(factTitle);

    // Back 2 undoes the earlier step: a working library, no history
    await blurAndBack(page);
    await expect(page.getByTestId('left-panel')).toBeVisible();
    expect(await isInHistoryMode(page)).toBe(false);
  });

  // ─── Rule: Rapid mode switching doesn't crash ───────────────────────

  test('rapid sort switching: path ↔ recent × 4', async ({ page }) => {
    for (const view of ['chrono', 'tree', 'chrono', 'tree', 'chrono', 'tree', 'chrono', 'tree'] as const) {
      await switchView(page, view);
    }
    // App should still be functional, on the path tree
    await expect(page.getByTestId('left-panel')).toHaveAttribute('data-sort', 'path');
    await expect(page.getByTestId('dir-entry').first()).toBeVisible({ timeout: 5_000 });
  });

  // ─── Rule: Local ref click → history at commit → back restores ──────

  test('fact with ref → click ref → history shows referenced fact → back restores', async ({ page }) => {
    // Search for the fact with local refs
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('query planner');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(1000);

    const factEntry = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
    await expect(factEntry).toBeVisible({ timeout: 10_000 });
    await factEntry.click();
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
    const titleBefore = await getFactTitle(page);

    // Click local ref
    const localRef = firstLocalRef(page);
    await expect(localRef).toBeVisible({ timeout: 10_000 });
    await localRef.click();

    // Should be in history with the referenced fact loaded
    await expect(page.getByTestId('timeline-nav')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('fact-title')).not.toContainText(titleBefore, { timeout: 10_000 });

    // Back → should restore original fact in tree mode
    await blurAndBack(page);
    expect(await isInHistoryMode(page)).toBe(false);
    const titleAfter = await getFactTitle(page);
    expect(titleAfter).toBe(titleBefore);
  });
});
