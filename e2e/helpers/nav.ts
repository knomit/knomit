/**
 * Navigation helpers for the facets UI.
 *
 * The old 1/2/3 view keys are gone. The three things they reached are now:
 *   - tree   : Browse facet, library sorted by Path  (button "Sort by Path")
 *   - chrono : Browse facet, library sorted by Recent (button "Sort by Recent")
 *   - history: the per-fact timeline, opened from the version walker chip in
 *              the fact header (data-testid="version-walker")
 * There is no repo-wide commit history view any more.
 */
import { expect, type Page } from '@playwright/test';

/** Switch the library to the recent-first (chrono) list and wait for items. */
export async function enterChrono(page: Page) {
  await page.getByRole('button', { name: 'Sort by Recent' }).click();
  await page.getByTestId('chrono-item').first().waitFor({ timeout: 10_000 });
}

/** Switch the library back to the path tree and wait for directory entries. */
export async function exitChrono(page: Page) {
  await page.getByRole('button', { name: 'Sort by Path' }).click();
  await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
  await expect(page.getByTestId('chrono-item')).toHaveCount(0);
}

/** With a fact open, open its version timeline. */
export async function openHistory(page: Page) {
  await page.getByTestId('version-walker').click();
  await expect(page.getByTestId('timeline-nav')).toBeVisible({ timeout: 10_000 });
  await page.getByTestId('timeline-row').first().waitFor({ timeout: 10_000 });
}

/** Descend from the library root until a fact row is clickable, then open it. */
export async function openFirstFact(page: Page) {
  const facts = page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]'));
  const dirs = page.getByTestId('dir-entry').and(page.locator('[data-isdir="true"]'));
  await page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 });
  for (let depth = 0; depth < 4 && (await facts.count()) === 0; depth++) {
    await dirs.first().click();
    await page.waitForTimeout(400);
  }
  await facts.first().waitFor({ timeout: 10_000 });
  await facts.first().click();
  await expect(page.getByTestId('fact-title')).toBeVisible({ timeout: 10_000 });
}

/**
 * The first clickable local-fact reference in the open fact's REFERENCES list
 * (a "→ kb/..." row that hops). Plain "→" rows (src://, urls) are inert and
 * carry no pointer cursor, so they are excluded.
 */
export function firstLocalRef(page: Page) {
  return page.locator('span[style*="cursor: pointer"]').filter({ hasText: /^→ kb\// }).first();
}

/**
 * Open the Connect remote wizard: the repo's Settings tab, then the Connect
 * button of its Remote block. Returns the wizard locator.
 */
export async function openConnectRemote(page: Page) {
  await page.getByRole('tab', { name: 'Settings' }).click();
  await page.getByTestId('remote-connect').click();
  const wizard = page.getByTestId('remote-connect-wizard');
  await expect(wizard).toBeVisible({ timeout: 10_000 });
  return wizard;
}

const isRecentRead = (resp: { url(): string }) =>
  resp.url().includes('/facts') && resp.url().includes('sort=recent');

/**
 * Narrow the chrono list with an entity chip. Free text no longer filters the
 * chrono list (it switches the library to a relevance search), but chips do.
 */
export async function filterByEntity(page: Page, entity: string) {
  const answered = page.waitForResponse(
    resp => isRecentRead(resp) && resp.url().includes('entities='),
  );
  const input = page.locator('#filter-input');
  await input.fill(`entity:${entity}`);
  await input.press('Enter');
  await answered;
  await page.waitForTimeout(500);
}

/** Drop every chip: blur the input, then Escape (CLEAR_FILTERS) and wait for the re-read. */
export async function clearFilters(page: Page) {
  await page.locator('#filter-input').blur();
  const answered = page.waitForResponse(isRecentRead);
  await page.keyboard.press('Escape');
  await answered;
  await page.waitForTimeout(500);
}
