import { test, expect } from '../../fixtures/knomit.js';

test.describe('Remote Setup', () => {
  test('open connect remote modal → enter URL → verify UI flow', async ({ freshKnomit, page }) => {
    // Seed a fact so the repo exists
    const seedRes = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/remote-setup-seed.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [remote-setup]
refs: []
---
# Remote Setup Seed

Seed fact so the repo exists.`,
      },
    });
    expect(seedRes.ok()).toBeTruthy();

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    // Open the menu
    const menuBtn = page.getByTestId('toknomitr-menu-btn');
    await menuBtn.click();

    // Click Origin item
    const originItem = page.getByTestId('menu-origin');
    await expect(originItem).toBeVisible();
    await originItem.click();

    // Connect Remote modal appears
    const modal = page.getByTestId('connect-remote-modal');
    await expect(modal).toBeVisible();

    // Verify modal title
    await expect(modal.locator('h2')).toContainText('Connect Remote');

    // Enter a remote URL
    const urlInput = page.getByTestId('connect-remote-url-input');
    await urlInput.fill('git@github.com:test/remote-setup-test.git');

    // Auth method selector should be visible
    const authSelect = modal.locator('select');
    await expect(authSelect).toBeVisible();

    // Test Connection button should be enabled
    const testBtn = page.getByTestId('connect-remote-test-btn');
    await expect(testBtn).toBeEnabled();

    // Cancel button should be visible
    const cancelBtn = page.getByTestId('connect-remote-cancel-btn');
    await expect(cancelBtn).toBeVisible();

    // Close via cancel
    await cancelBtn.click();
    await expect(modal).not.toBeVisible();

    // No origin should be configured (we didn't complete the flow)
    const originRes = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/knomit/origin`);
    expect(originRes.status()).toBe(204); // no content = no origin configured
  });
});
