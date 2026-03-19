import { test, expect } from '../../fixtures/knomit.js';

test.describe.serial('Multi-Repo', () => {
  test('create facts in two repos → repo selector appears → switch repos → different content', async ({ freshKnomit, page }) => {
    // Create a fact in the default "knomit" repo
    const res1 = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/knomit/fact`, {
      data: {
        path: 'kb/primary-fact.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [primary]
refs: []
---
# Primary Fact

Content in the default knomit repo.`,
      },
    });
    expect(res1.ok()).toBeTruthy();

    // Create a fact in a second repo (auto-created on first write)
    const res2 = await freshKnomit.api.put(`${freshKnomit.baseURL}/api/v1/second/fact`, {
      data: {
        path: 'kb/secondary-fact.md',
        content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [secondary]
refs: []
---
# Secondary Fact

Content in the second repo.`,
      },
    });
    expect(res2.ok()).toBeTruthy();

    // Reload to pick up the new repo list
    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('networkidle');

    // Verify repo selector dropdown appears (multiple repos)
    const repoSelect = page.getByTestId('toknomitr-repo-select');
    await expect(repoSelect).toBeVisible({ timeout: 10_000 });

    // Verify default repo shows primary-fact
    const entries = page.getByTestId('dir-entry');
    await entries.first().waitFor({ timeout: 10_000 });
    const initialNames: string[] = [];
    for (let i = 0; i < await entries.count(); i++) {
      initialNames.push((await entries.nth(i).getAttribute('data-name')) || '');
    }
    expect(initialNames).toContain('primary-fact.md');

    // Switch to the "second" repo
    await repoSelect.selectOption('second');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(1000);

    // Verify different content
    const newEntries = page.getByTestId('dir-entry');
    await newEntries.first().waitFor({ timeout: 10_000 });
    const newNames: string[] = [];
    for (let i = 0; i < await newEntries.count(); i++) {
      newNames.push((await newEntries.nth(i).getAttribute('data-name')) || '');
    }
    expect(newNames).toContain('secondary-fact.md');
    expect(newNames).not.toContain('primary-fact.md');
  });
});
