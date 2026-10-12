import { test, expect } from '../../fixtures/knomit.js';
import { openFirstFact, openHistory } from '../../helpers/nav.js';

// The status footer is the app's bottom rail: where you are in time, which
// build is serving. It replaced the old expandable console; it is not a button.
test.describe('Status footer', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('the footer is visible at the bottom of the page', async ({ page }) => {
    const footer = page.getByTestId('status-footer');
    await expect(footer).toBeVisible();

    const box = await footer.boundingBox();
    const viewport = page.viewportSize();
    expect(box).not.toBeNull();
    expect(viewport).not.toBeNull();
    expect(Math.abs(box!.y + box!.height - viewport!.height)).toBeLessThanOrEqual(2);
  });

  test('live: the footer shows the LIVE dot, HEAD and the build version', async ({ page }) => {
    const footer = page.getByTestId('status-footer');
    await expect(footer.getByRole('img', { name: 'LIVE' })).toBeVisible();
    await expect(footer).toContainText('HEAD');
    await expect(footer.getByTestId('version-badge')).toHaveText(/^v\S+/);
  });

  test('in history the footer shows the HISTORY dot instead of LIVE', async ({ page }) => {
    await openFirstFact(page);
    await openHistory(page);

    const footer = page.getByTestId('status-footer');
    await expect(footer.getByRole('img', { name: 'HISTORY' })).toBeVisible();
    await expect(footer.getByRole('img', { name: 'LIVE' })).toHaveCount(0);
  });
});
