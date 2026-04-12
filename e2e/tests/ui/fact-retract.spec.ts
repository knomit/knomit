import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

const FACT_CONTENT = `---
type: observation
confidence: 0.9
entities:
  - retract-test
refs: []
---
# Retractable Fact

This fact will be retracted.`;

test.describe('Fact Retract', () => {
  test('retract button appears in tree mode and retracts the fact', async ({ freshKnomit, page }) => {
    // Seed the fact
    const res = await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/kb/to-retract.md`,
      { data: { content: FACT_CONTENT } },
    );
    expect(res.ok()).toBeTruthy();

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');

    const browse = new BrowsePage(page);
    await browse.clickEntry('to-retract.md');

    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toContainText('Retractable Fact');

    // Retract button is visible in tree mode
    const retractBtn = page.getByTestId('retract-btn');
    await expect(retractBtn).toBeVisible();

    // Click retract and wait for DELETE response
    const [response] = await Promise.all([
      page.waitForResponse(resp => resp.url().includes('/facts/') && resp.request().method() === 'DELETE'),
      retractBtn.click(),
    ]);
    expect(response.ok()).toBeTruthy();

    // The fact should no longer appear in the directory listing
    await page.reload();
    await page.waitForLoadState('domcontentloaded');
    const entries = await browse.getDirectoryEntries();
    const found = entries.some(e => e.name === 'to-retract.md');
    expect(found).toBe(false);
  });

  test('retract commits with correct message and operation', async ({ freshKnomit }) => {
    // Seed the fact
    await freshKnomit.api.put(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/kb/retract-check.md`,
      { data: { content: FACT_CONTENT } },
    );

    // Retract via API directly
    const retractRes = await freshKnomit.api.delete(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/kb/retract-check.md`,
    );
    expect(retractRes.ok()).toBeTruthy();
    const body = await retractRes.json();
    expect(body.commit).toBeTruthy();

    // Verify the commit message and operation via commits list (filter by path)
    const histRes = await freshKnomit.api.get(
      `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/commits`,
    );
    const hist = await histRes.json();
    const latest = hist._embedded.commits[0];
    expect(latest.message).toBe('manual-review: retract kb/retract-check.md');
    expect(latest.operation).toBe('retract');
  });
});
