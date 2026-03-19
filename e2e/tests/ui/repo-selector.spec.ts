import { test, expect } from '../../fixtures/knomit.js';

test.describe('Repo Selector', () => {
  test('with one repo, shows repo name as plain text (no dropdown)', async ({ freshKnomit, page }) => {
    // Seed a fact so at least one repo exists
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/repo-test.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [repo-test]
refs: []
---
# Repo Test

Single repo test fact.`,
      },
    });

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');

    // With only one repo, should show plain text repo name
    const repoName = page.getByTestId('toknomitr-repo-name');
    await expect(repoName).toBeVisible();
    await expect(repoName).toHaveText('knomit');

    // Dropdown should NOT exist
    const repoSelect = page.getByTestId('toknomitr-repo-select');
    await expect(repoSelect).not.toBeVisible();
  });

  test('with two repos, shows dropdown selector', async ({ freshKnomit, page }) => {
    // Create a fact in the default "knomit" repo
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/first-repo.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [repo-test]
refs: []
---
# First Repo Fact

Fact in the default repo.`,
      },
    });

    // Create a fact in a second repo (the server auto-creates repos on first write)
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/second/fact`, {
      data: {
        path: 'kb/second-repo.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [repo-test]
refs: []
---
# Second Repo Fact

Fact in the second repo.`,
      },
    });

    // Reload to pick up the new repo list
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');

    // With two repos, should show a dropdown
    const repoSelect = page.getByTestId('toknomitr-repo-select');
    await expect(repoSelect).toBeVisible({ timeout: 10_000 });

    // Should have both repo options
    const options = repoSelect.locator('option');
    const optionCount = await options.count();
    expect(optionCount).toBe(2);
  });

  test('switching repos via dropdown changes displayed content', async ({ freshKnomit, page }) => {
    // Create facts in two different repos
    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/alpha-fact.md',
        content: `---
type: observation
domain: [alpha]
confidence: 0.9
sources: 1
entities: [alpha]
refs: []
---
# Alpha Fact

Content in the knomit repo.`,
      },
    });

    await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/other/fact`, {
      data: {
        path: 'kb/beta-fact.md',
        content: `---
type: observation
domain: [beta]
confidence: 0.9
sources: 1
entities: [beta]
refs: []
---
# Beta Fact

Content in the other repo.`,
      },
    });

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');

    // Verify we start on "knomit" repo and see alpha-fact
    const entries = page.getByTestId('dir-entry');
    await entries.first().waitFor({ timeout: 10_000 });
    const initialNames: string[] = [];
    for (let i = 0; i < await entries.count(); i++) {
      initialNames.push((await entries.nth(i).getAttribute('data-name')) || '');
    }
    expect(initialNames).toContain('alpha-fact.md');

    // Switch to the "other" repo
    const repoSelect = page.getByTestId('toknomitr-repo-select');
    await repoSelect.selectOption('other');
    await page.waitForLoadState('networkidle');

    // Wait for entries to update
    await page.waitForTimeout(1000);
    const newEntries = page.getByTestId('dir-entry');
    await newEntries.first().waitFor({ timeout: 10_000 });
    const newNames: string[] = [];
    for (let i = 0; i < await newEntries.count(); i++) {
      newNames.push((await newEntries.nth(i).getAttribute('data-name')) || '');
    }
    expect(newNames).toContain('beta-fact.md');
    expect(newNames).not.toContain('alpha-fact.md');
  });
});
