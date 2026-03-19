import { test, expect } from '../../fixtures/knomit.js';

test.describe('Repo Selector', () => {
  test('with one repo, shows repo name as plain text (no dropdown)', async ({ freshKnomit, page }) => {
    // Seed a fact so at least one repo exists
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/repo-test.md',
        content: `---
type: observation
confidence: 0.9
entities:
  - repo-test
refs: []
---
# Repo Test

Single repo test fact.`,
      },
    });

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    // With only one repo, should show plain text repo name
    const repoName = page.getByTestId('toknomitr-repo-name');
    await expect(repoName).toBeVisible();
    await expect(repoName).toHaveText('knomit');

    // Dropdown should NOT exist
    const repoSelect = page.getByTestId('toknomitr-repo-select');
    await expect(repoSelect).not.toBeVisible();
  });

  // Multi-repo tests are skipped: the server discovers repos from *.db files at
  // startup and the API does not support creating repos on the fly. Writing to a
  // non-existent repo name returns 404 from the repo middleware.

  test.skip('with two repos, shows dropdown selector', async () => {
    // Requires a second repo to be initialised before the server starts.
    // Not currently possible with the freshKnomit fixture.
  });

  test.skip('switching repos via dropdown changes displayed content', async () => {
    // Requires a second repo to be initialised before the server starts.
    // Not currently possible with the freshKnomit fixture.
  });
});
