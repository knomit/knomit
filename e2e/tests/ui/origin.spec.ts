import { test, expect } from '../../fixtures/knomit.js';
import { openConnectRemote } from '../../helpers/nav.js';

// Connecting a remote is a sub-page of the repo's Settings (not a modal).
test.describe('Connect Remote wizard', () => {
  test('open from Settings, enter a URL, and leave via Cancel', async ({ freshKnomit, page }) => {
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    const wizard = await openConnectRemote(page);

    // Enter a URL
    const urlInput = page.getByTestId('wizard-url');
    await urlInput.fill('git@github.com:test/origin-test.git');
    await expect(urlInput).toHaveValue('git@github.com:test/origin-test.git');

    // Test connection is enabled once there is a URL
    await expect(page.getByTestId('wizard-test')).toBeEnabled();

    // Cancel returns to the repo's settings
    await wizard.getByRole('button', { name: 'Cancel' }).click();
    await expect(wizard).not.toBeVisible();
    await expect(page.getByTestId('remote-connect')).toBeVisible();
  });

  test('the breadcrumb leads back to the repo settings', async ({ freshKnomit, page }) => {
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    const wizard = await openConnectRemote(page);
    // Positive control: with no URL the test button is disabled
    await expect(page.getByTestId('wizard-test')).toBeDisabled();

    await page.getByTestId('wizard-crumb-back').click();
    await expect(wizard).not.toBeVisible();
    await expect(page.getByTestId('remote-connect')).toBeVisible();
  });
});
