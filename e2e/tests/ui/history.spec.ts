import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';
import { HistoryPage } from '../../pages/history.page.js';
import { openFirstFact, openHistory } from '../../helpers/nav.js';

// History is a per-fact timeline: open a fact, click its version chip. There
// is no repo-wide commit history view and no 1/2/3 view keys any more.
test.describe('History (per-fact timeline)', () => {
  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    await page.waitForLoadState('domcontentloaded');
  });

  test('the version chip opens the timeline of the open fact', async ({ page }) => {
    await openFirstFact(page);
    // Positive control: live, no timeline yet.
    await expect(page.getByTestId('timeline-nav')).toHaveCount(0);

    await openHistory(page);
    const history = new HistoryPage(page);
    await expect(history.timeline).toBeVisible();
    await expect(page.getByRole('img', { name: 'HISTORY' })).toBeVisible();

    const commits = await history.getCommits();
    expect(commits.length).toBeGreaterThanOrEqual(1);
    for (const c of commits) {
      expect(c.hash).toMatch(/^[0-9a-f]{7}$/);
    }
    // The fact stays on screen beside the timeline.
    await expect(page.getByTestId('fact-title')).toBeVisible();
  });

  test('the return-to-live control leaves history, keeping the fact', async ({ page }) => {
    await openFirstFact(page);
    const title = await new FactPanel(page).getTitle();
    await openHistory(page);

    await page.getByTestId('timeline-return-live').click();

    await expect(page.getByTestId('timeline-nav')).toHaveCount(0);
    await expect(page.getByTestId('left-panel')).toBeVisible();
    await expect(page.getByRole('img', { name: 'LIVE' })).toBeVisible();
    expect(await new FactPanel(page).getTitle()).toBe(title);
  });

  test('"h" returns from history to live', async ({ page }) => {
    await openFirstFact(page);
    await openHistory(page);

    await page.locator('body').click({ position: { x: 5, y: 5 } });
    await page.keyboard.press('h');

    await expect(page.getByTestId('timeline-nav')).toHaveCount(0);
    await expect(page.getByTestId('left-panel')).toBeVisible();
  });

  test('Backspace from history leaves the timeline', async ({ page }) => {
    await openFirstFact(page);
    await openHistory(page);

    await page.locator('body').click({ position: { x: 5, y: 5 } });
    await page.keyboard.press('Backspace');

    // Opening the timeline is not a navigation step (SET_AS_OF does not push
    // the back stack), so Back pops the step BEFORE the fact was opened. Only
    // the exit from history is asserted; the fact is not on screen afterwards.
    await expect(page.getByTestId('timeline-nav')).toHaveCount(0);
    await expect(page.getByTestId('left-panel')).toBeVisible();
    await expect(page.getByRole('img', { name: 'LIVE' })).toBeVisible();
  });
});

test.describe('History (fact with several versions)', () => {
  test('the timeline lists every version and selecting an older one shows its content', async ({ freshKnomit, page }) => {
    const factPath = 'kb/history/demo.md';
    const put = async (body: string) => {
      const res = await freshKnomit.api.put(
        `${freshKnomit.baseURL}/api/v1/repos/knomit/branches/${freshKnomit.branch}/facts/${factPath}`,
        {
          data: {
            content: `---
type: observation
domain: [testing]
confidence: 0.9
sources: 1
entities: [history-entity]
refs: []
---
# History Demo

${body}`,
          },
        },
      );
      expect(res.ok()).toBeTruthy();
    };
    await put('first version text');
    await put('second version text');

    await page.goto(freshKnomit.baseURL);
    await page.waitForLoadState('domcontentloaded');
    const browse = new BrowsePage(page);
    await browse.clickEntry('history');
    await browse.clickEntry('demo.md');
    const fact = new FactPanel(page);
    await expect(fact.body).toContainText('second version text');

    await openHistory(page);
    const history = new HistoryPage(page);
    await expect(page.getByTestId('timeline-row')).toHaveCount(2);
    const [newest, older] = await history.getCommits();
    expect(newest.hash).not.toBe(older.hash);

    // Select the older version: the body is the first text, not the second.
    await page.getByTestId('timeline-row').nth(1).click();
    await expect(fact.body).toContainText('first version text');
    await expect(fact.body).not.toContainText('second version text');
  });
});
