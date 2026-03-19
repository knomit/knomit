import { test, expect } from '../../fixtures/knomit.js';

test.describe('Origin Modal', () => {
  test('open origin modal, set URL, save, and verify via API', async ({ freshKnomit, page }) => {
    // Seed a fact so the repo exists
    const seedRes = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/origin-test.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [origin-test]
refs: []
---
# Origin Test Fact

Fact created to ensure the repo exists.`,
      },
    });
    expect(seedRes.ok()).toBeTruthy();

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    // Open the menu
    const menuBtn = page.getByTestId('toknomitr-menu-btn');
    await menuBtn.click();

    // Click Origin
    const originItem = page.getByTestId('menu-origin');
    await expect(originItem).toBeVisible();
    await originItem.click();

    // Origin modal appears
    const modal = page.getByTestId('origin-modal');
    await expect(modal).toBeVisible();

    // Enter a URL
    const urlInput = page.getByTestId('origin-url-input');
    await urlInput.fill('git@github.com:test/origin-test.git');

    // Type "yes" to confirm the URL change
    const confirmInput = modal.locator('input[placeholder="yes"]');
    await confirmInput.fill('yes');

    // Click save
    const saveBtn = page.getByTestId('origin-save-btn');
    await expect(saveBtn).toBeEnabled();
    await saveBtn.click();

    // Modal should close after save
    await expect(modal).not.toBeVisible({ timeout: 10_000 });

    // Verify the origin was set via API
    const originRes = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/knomit/origin`);
    expect(originRes.ok()).toBeTruthy();
    const originData = await originRes.json();
    expect(originData.url).toBe('git@github.com:test/origin-test.git');
  });

  test('close button dismisses the origin modal', async ({ freshKnomit, page }) => {
    // Seed a fact so the repo exists
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/close-test.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.5
sources: 1
entities: [close-test]
refs: []
---
# Close Test

Seed fact.`,
      },
    });

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    // Open menu -> Origin
    await page.getByTestId('toknomitr-menu-btn').click();
    await page.getByTestId('menu-origin').click();

    const modal = page.getByTestId('origin-modal');
    await expect(modal).toBeVisible();

    // Close via close button
    await page.getByTestId('origin-close-btn').click();
    await expect(modal).not.toBeVisible();
  });
});
