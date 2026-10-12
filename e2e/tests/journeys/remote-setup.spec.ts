import { test, expect } from '../../fixtures/knomit.js';
import { openConnectRemote } from '../../helpers/nav.js';

test.describe('Remote Setup', () => {
  test('open connect remote → enter URL → verify UI flow', async ({ freshKnomit, page }) => {
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    const wizard = await openConnectRemote(page);

    // Verify the page title
    await expect(wizard.locator('h3')).toContainText('Connect remote');

    // Enter a remote URL
    await page.getByTestId('wizard-url').fill('git@github.com:test/remote-setup-test.git');

    // Auth method selector should be visible
    await expect(wizard.locator('select').first()).toBeVisible();

    // Test connection should be enabled
    await expect(page.getByTestId('wizard-test')).toBeEnabled();

    // Cancel is offered
    const cancelBtn = wizard.getByRole('button', { name: 'Cancel' });
    await expect(cancelBtn).toBeVisible();

    // Leave without completing the flow
    await cancelBtn.click();
    await expect(wizard).not.toBeVisible();

    // No origin should be configured (we didn't complete the flow)
    const originRes = await freshKnomit.api.get(`${freshKnomit.baseURL}/api/v1/repos/knomit/origin`);
    expect(originRes.status()).toBe(204); // no content = no origin configured
  });
});
