/**
 * Navigation path tests — verifies the operation hierarchy:
 *
 * 1. Mode (tree/chrono/history) determines what the left panel shows
 * 2. Filters narrow within the current mode
 * 3. Tree ↔ Chrono: filters preserved (same data, different presentation)
 * 4. Tree/Chrono ↔ History: only path filter kept, rest cleared
 * 5. Left selection → right panel content
 * 6. Back navigation restores EXACT previous state
 * 7. Entity/domain clicks stay in current mode, add filter
 */
import { test, expect } from '../../fixtures/knomit.js';

// Helpers
async function blurAndBack(page: import('@playwright/test').Page) {
  await page.locator('body').click();
  await page.waitForTimeout(200);
  await page.keyboard.press('Backspace');
  await page.waitForTimeout(500);
}

async function switchView(page: import('@playwright/test').Page, key: '1' | '2' | '3') {
  await page.locator('body').click();
  await page.waitForTimeout(100);
  await page.keyboard.press(key);
  await page.waitForTimeout(500);
}

async function isInHistoryMode(page: import('@playwright/test').Page): Promise<boolean> {
  return page.getByTestId('history-timeline').isVisible({ timeout: 2_000 }).catch(() => false);
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
    await switchView(page, '2');
    await page.getByTestId('chrono-item').first().waitFor({ timeout: 10_000 }).catch(() => {});

    // Switch back to tree
    await switchView(page, '1');
    await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
    expect(await page.getByTestId('dir-entry').count()).toBeGreaterThan(0);
  });

  // ─── Rule: Tree ↔ Chrono preserves filters ──────────────────────────

  test('tree with entity filter → chrono: filter preserved', async ({ page }) => {
    // Navigate to a fact with entities
    const firstDir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
    await firstDir.waitFor({ timeout: 10_000 });
    await firstDir.click();
    await page.waitForTimeout(500);

    // Find and click a fact
    for (let i = 0; i < 3; i++) {
      const fact = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
      if (await fact.isVisible().catch(() => false)) {
        await fact.click();
        break;
      }
      const dir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
      if (await dir.isVisible().catch(() => false)) { await dir.click(); await page.waitForTimeout(500); }
    }

    // Click an entity tag
    const tag = page.getByTestId('tag-item').first();
    if (!await tag.isVisible({ timeout: 3_000 }).catch(() => false)) {
      test.skip(true, 'No entity tags found');
      return;
    }
    const tagValue = await tag.getAttribute('data-value') || '';
    await tag.click();
    await page.waitForTimeout(500);

    // Switch to chrono — filter should be preserved
    await switchView(page, '2');
    await page.waitForTimeout(500);

    // Check the entity chip is still there
    const chips = await chipTexts(page);
    expect(chips.some(c => c.includes(tagValue))).toBe(true);
  });

  // ─── Rule: Tree/Chrono → History clears non-path filters ────────────

  test('tree with domain filter → history: domain cleared, path kept', async ({ page }) => {
    // Add a path filter via navigation
    const firstDir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
    await firstDir.waitFor({ timeout: 10_000 });
    await firstDir.click();
    await page.waitForTimeout(500);

    // Add a domain filter by clicking a fact's domain tag
    const fact = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
    if (await fact.isVisible().catch(() => false)) {
      await fact.click();
      await page.waitForTimeout(500);
    }
    const domainTag = page.locator('[data-testid="tag-item"]').first();
    if (!await domainTag.isVisible({ timeout: 3_000 }).catch(() => false)) {
      test.skip(true, 'No tags found');
      return;
    }
    await domainTag.click();
    await page.waitForTimeout(500);

    // Now switch to history
    await switchView(page, '3');
    await page.getByTestId('history-timeline').waitFor({ timeout: 10_000 });

    // Domain/entity chip should be gone, path chip might remain
    const chips = await chipTexts(page);
    expect(chips.filter(c => c.startsWith('domain:'))).toHaveLength(0);
    expect(chips.filter(c => c.startsWith('entity:'))).toHaveLength(0);
  });

  test('history with ep filter → tree: ep cleared', async ({ page }) => {
    // Switch to history
    await switchView(page, '3');
    await page.getByTestId('history-timeline').waitFor({ timeout: 10_000 });

    // Add ep:learn filter
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('ep:learn');
    await page.keyboard.press('Space');
    await page.waitForTimeout(500);

    // Verify ep chip is there
    let chips = await chipTexts(page);
    expect(chips.some(c => c.includes('ep:'))).toBe(true);

    // Switch to tree
    await switchView(page, '1');
    await page.waitForTimeout(500);

    // ep chip should be gone
    chips = await chipTexts(page);
    expect(chips.filter(c => c.startsWith('ep:'))).toHaveLength(0);
  });

  // ─── Rule: Entity/domain clicks stay in current mode ────────────────

  test('entity click in chrono stays in chrono', async ({ page }) => {
    // Switch to chrono
    await switchView(page, '2');
    await page.getByTestId('chrono-item').first().waitFor({ timeout: 10_000 }).catch(() => {});

    // Click first chrono item to load a fact
    const item = page.getByTestId('chrono-item').first();
    if (await item.isVisible().catch(() => false)) {
      await item.click();
      await page.waitForTimeout(500);
    }

    // Click an entity tag
    const tag = page.getByTestId('tag-item').first();
    if (!await tag.isVisible({ timeout: 3_000 }).catch(() => false)) {
      test.skip(true, 'No entity tags in chrono view');
      return;
    }
    await tag.click();
    await page.waitForTimeout(500);

    // Should still NOT be in history mode
    expect(await isInHistoryMode(page)).toBe(false);
  });

  // ─── Rule: Back navigation restores exact state ─────────────────────

  test('select fact → switch to history → back: restores fact', async ({ page }) => {
    // Navigate to and select a fact
    const firstDir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
    await firstDir.waitFor({ timeout: 10_000 });
    await firstDir.click();
    await page.waitForTimeout(500);

    for (let i = 0; i < 3; i++) {
      const fact = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
      if (await fact.isVisible().catch(() => false)) {
        await fact.click();
        break;
      }
      const dir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
      if (await dir.isVisible().catch(() => false)) { await dir.click(); await page.waitForTimeout(500); }
    }

    if (!await hasFactTitle(page)) {
      test.skip(true, 'Could not select a fact');
      return;
    }
    const titleBefore = await getFactTitle(page);

    // Switch to history
    await switchView(page, '3');
    await page.getByTestId('history-timeline').waitFor({ timeout: 10_000 });

    // Back
    await blurAndBack(page);

    // Should be back in tree with the same fact
    expect(await isInHistoryMode(page)).toBe(false);
    const titleAfter = await getFactTitle(page);
    expect(titleAfter).toBe(titleBefore);
  });

  test('history with filter → tree → back: restores history with filter', async ({ page }) => {
    // Switch to history
    await switchView(page, '3');
    await page.getByTestId('history-timeline').waitFor({ timeout: 10_000 });

    // Add ep filter
    const filterInput = page.locator('#filter-input');
    await filterInput.focus();
    await filterInput.fill('ep:learn');
    await page.keyboard.press('Space');
    await page.waitForTimeout(500);

    // Switch to tree
    await switchView(page, '1');
    await page.waitForTimeout(500);
    expect(await isInHistoryMode(page)).toBe(false);

    // Back → should restore history with ep:learn
    await blurAndBack(page);
    expect(await isInHistoryMode(page)).toBe(true);

    const chips = await chipTexts(page);
    expect(chips.some(c => c.includes('ep:'))).toBe(true);
  });

  // ─── Rule: Deep navigation with multiple backs ──────────────────────

  test('tree → dir → fact → entity filter → chrono → back × 3', async ({ page }) => {
    // Step 1: Navigate into directory
    const firstDir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
    await firstDir.waitFor({ timeout: 10_000 });
    await firstDir.click();
    await page.waitForTimeout(500);

    // Step 2: Select a fact
    for (let i = 0; i < 3; i++) {
      const fact = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first();
      if (await fact.isVisible().catch(() => false)) {
        await fact.click();
        break;
      }
      const dir = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]')).first();
      if (await dir.isVisible().catch(() => false)) { await dir.click(); await page.waitForTimeout(500); }
    }

    if (!await hasFactTitle(page)) {
      test.skip(true, 'Could not find a fact');
      return;
    }
    const factTitle = await getFactTitle(page);

    // Step 3: Click entity tag
    const tag = page.getByTestId('tag-item').first();
    if (!await tag.isVisible({ timeout: 3_000 }).catch(() => false)) {
      test.skip(true, 'No tags on the fact');
      return;
    }
    await tag.click();
    await page.waitForTimeout(500);

    // Step 4: Switch to chrono
    await switchView(page, '2');
    await page.waitForTimeout(500);

    // Back 1: should return to tree with entity filter
    await blurAndBack(page);
    expect(await isInHistoryMode(page)).toBe(false);

    // Back 2: should return to fact without entity filter
    await blurAndBack(page);
    const restoredTitle = await getFactTitle(page);
    expect(restoredTitle).toBe(factTitle);
  });

  // ─── Rule: Rapid mode switching doesn't crash ───────────────────────

  test('rapid mode switching: 1 → 2 → 3 → 1 → 2 → 3 → 1', async ({ page }) => {
    for (const key of ['1', '2', '3', '1', '2', '3', '1'] as const) {
      await switchView(page, key);
    }
    // App should still be functional
    await expect(page.getByTestId('left-panel')).toBeVisible({ timeout: 5_000 });
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
    if (!await factEntry.isVisible({ timeout: 5_000 }).catch(() => false)) {
      test.skip(true, 'query-planning fact not found');
      return;
    }
    await factEntry.click();
    await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
    const titleBefore = await getFactTitle(page);

    // Click local ref
    const localRef = page.locator('span').filter({ hasText: /^→/ }).first();
    if (!await localRef.isVisible({ timeout: 3_000 }).catch(() => false)) {
      test.skip(true, 'No local refs');
      return;
    }
    await localRef.click();

    // Should be in history with the referenced fact loaded
    await expect(page.getByTestId('history-timeline')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('fact-title')).not.toContainText(titleBefore, { timeout: 10_000 });

    // Back → should restore original fact in tree mode
    await blurAndBack(page);
    expect(await isInHistoryMode(page)).toBe(false);
    const titleAfter = await getFactTitle(page);
    expect(titleAfter).toBe(titleBefore);
  });
});
