import { test, expect } from '../../fixtures/knomit.js';

test.describe('Connect Remote Modal', () => {
  test('open modal, enter URL, and close via cancel', async ({ freshKnomit, page }) => {
    // Seed a fact so the repo exists
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
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

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    // Open the menu
    const menuBtn = page.getByTestId('toknomitr-menu-btn');
    await menuBtn.click();

    // Click Origin
    const originItem = page.getByTestId('menu-origin');
    await expect(originItem).toBeVisible();
    await originItem.click();

    // Connect Remote modal appears
    const modal = page.getByTestId('connect-remote-modal');
    await expect(modal).toBeVisible();

    // Enter a URL
    const urlInput = page.getByTestId('connect-remote-url-input');
    await urlInput.fill('git@github.com:test/origin-test.git');
    await expect(urlInput).toHaveValue('git@github.com:test/origin-test.git');

    // Test Connection button should be enabled
    const testBtn = page.getByTestId('connect-remote-test-btn');
    await expect(testBtn).toBeEnabled();

    // Cancel closes the modal
    const cancelBtn = page.getByTestId('connect-remote-cancel-btn');
    await cancelBtn.click();
    await expect(modal).not.toBeVisible();
  });

  test('close button dismisses the modal', async ({ freshKnomit, page }) => {
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

    const modal = page.getByTestId('connect-remote-modal');
    await expect(modal).toBeVisible();

    // Close via X button
    await page.getByTestId('connect-remote-close-btn').click();
    await expect(modal).not.toBeVisible();
  });
});
