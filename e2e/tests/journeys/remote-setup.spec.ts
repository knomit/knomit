import { test, expect } from '../../fixtures/knomit.js';

test.describe.serial('Remote Setup', () => {
  test('open origin modal → set remote URL → save → verify via API', async ({ freshKnomit, page }) => {
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
    await page.waitForLoadState('networkidle');

    // Open the menu
    const menuBtn = page.getByTestId('toknomitr-menu-btn');
    await menuBtn.click();

    // Click Origin item
    const originItem = page.getByTestId('menu-origin');
    await expect(originItem).toBeVisible();
    await originItem.click();

    // Origin modal appears
    const modal = page.getByTestId('origin-modal');
    await expect(modal).toBeVisible();

    // Enter a remote URL
    const remoteUrl = 'git@github.com:test/remote-setup-test.git';
    const urlInput = page.getByTestId('origin-url-input');
    await urlInput.fill(remoteUrl);

    // Type "yes" to confirm
    const confirmInput = modal.locator('input[placeholder="yes"]');
    await confirmInput.fill('yes');

    // Click save
    const saveBtn = page.getByTestId('origin-save-btn');
    await expect(saveBtn).toBeEnabled();
    await saveBtn.click();

    // Modal should close after save
    await expect(modal).not.toBeVisible({ timeout: 10_000 });

    // Verify via API that origin is configured
    const originRes = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/knomit/origin`);
    expect(originRes.ok()).toBeTruthy();
    const originData = await originRes.json();
    expect(originData.url).toBe(remoteUrl);
  });
});
