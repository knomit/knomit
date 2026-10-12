import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { firstLocalRef } from '../../helpers/nav.js';

// The repo-wide history view (keys 1/2/3, ep: filters, per-commit file lists)
// no longer exists; its data-flow checks are covered by ui/history.spec.ts.
// What remains here is the ref-hop journey, which ends in the version timeline.
test.describe('Ref hop data flow', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('browse → entity click → ref click → timeline loads fact → back exits cleanly', async ({ page }) => {
    const browse = new BrowsePage(page);

    // Step 1: tree → databases → postgresql → query-planning (it cites mvcc and btree-vs-hash)
    await browse.clickEntry('databases');
    await browse.clickEntry('postgresql');
    await browse.clickEntry('query-planning.md');
    await expect(page.getByTestId('fact-title')).toContainText('Query Planning', { timeout: 10_000 });

    // Step 2: click the PostgreSQL entity tag: a chip appears and the library
    // lists the matching facts (chips force the recent-first list).
    await page.getByTestId('tag-item').filter({ hasText: 'PostgreSQL' }).first().click();
    await expect(page.locator('span').filter({ hasText: /entity:PostgreSQL/ }).first()).toBeVisible({ timeout: 5_000 });

    // Step 3: select the query-planning result
    const result = page.locator('[data-testid="chrono-item"][data-path$="/query-planning.md"]');
    await result.waitFor({ timeout: 10_000 });
    await result.click();
    await expect(page.getByTestId('fact-title')).toContainText('Query Planning', { timeout: 10_000 });

    // Step 4: click a local reference: the referenced fact opens in the timeline
    await firstLocalRef(page).click();
    await expect(page.getByTestId('timeline-nav')).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId('fact-title')).not.toContainText('Query Planning', { timeout: 10_000 });

    // Step 5: back leaves history and shows the original fact
    await page.locator('body').click({ position: { x: 5, y: 5 } });
    await page.keyboard.press('Backspace');
    await expect(page.getByTestId('timeline-nav')).toHaveCount(0, { timeout: 10_000 });
    await expect(page.getByTestId('fact-title')).toContainText('Query Planning', { timeout: 10_000 });

    // Step 6: back again keeps unwinding: still a functioning library
    await page.keyboard.press('Backspace');
    await expect(page.getByTestId('left-panel')).toBeVisible({ timeout: 5_000 });
  });
});
