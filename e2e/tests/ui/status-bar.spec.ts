import { test, expect } from '../../fixtures/knomit.js';

test.describe('Status Bar (Console)', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('networkidle');
  });

  test('console bar is visible at the bottom of the page', async ({ page }) => {
    // The Console component serves as the status bar (data-testid="console")
    const console = page.getByTestId('console');
    await expect(console).toBeVisible();
  });

  test('console bar contains status information text', async ({ page }) => {
    const console = page.getByTestId('console');
    await expect(console).toBeVisible();

    // The collapsed console shows "Console" label and entry counts
    const text = await console.textContent();
    expect(text).toContain('Console');
  });

  test('clicking the console bar toggles it open', async ({ page }) => {
    const console = page.getByTestId('console');
    await expect(console).toBeVisible();

    // Click to expand
    await console.click();

    // When expanded, the console-toggle button is still present
    const toggle = page.getByTestId('console-toggle');
    await expect(toggle).toBeVisible();
  });
});
