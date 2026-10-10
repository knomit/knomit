import { test, expect } from '../../fixtures/knomit.js';

test.describe('Repo Selector', () => {
  test('with one repo, shows repo name as plain text (no dropdown)', async ({ freshKnomit, page }) => {
    // Seed a fact so at least one repo exists
    await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/kb/repo-test.md`,
      {
        data: {
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
      },
    );

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    // The top bar names the target with a static label: picking a repository
    // moved to the target rail, so there is no dropdown at any repo count.
    const label = page.getByTestId('target-label');
    await expect(label).toBeVisible();
    await expect(label).toHaveText('knomit');
    await expect(label.locator('button')).toHaveCount(0);
  });

  test('the rail lists the repo and is collapsed until opened', async ({ freshKnomit, page }) => {
    await page.goto(freshKnomit.baseURL);
    await expect(page.getByTestId('target-label')).toHaveText('knomit');
    await expect(page.getByTestId('target-rail')).toHaveCount(0);

    await page.getByTestId('rail-toggle').click();
    await expect(page.getByTestId('target-rail')).toBeVisible();
    await expect(page.getByTestId('rail-repo-knomit')).toBeVisible();
  });

  // Multi-repo tests are skipped: the server discovers repos from *.db files at
  // startup and the API does not support creating repos on the fly. Writing to a
  // non-existent repo name returns 404 from the repo middleware.

  test.skip('with two repos, the rail lists both', async () => {
    // Requires a second repo to be initialised before the server starts.
    // Not currently possible with the freshKnomit fixture.
  });

  test.skip('switching repos via the rail changes displayed content', async () => {
    // Requires a second repo to be initialised before the server starts.
    // Not currently possible with the freshKnomit fixture.
  });
});
