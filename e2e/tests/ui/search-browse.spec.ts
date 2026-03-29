import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Search in Browse Mode', () => {
  let browse: BrowsePage;
  let factPanel: FactPanel;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    browse = new BrowsePage(page);
    factPanel = new FactPanel(page);
    await page.waitForLoadState('domcontentloaded');
    // Wait for initial browse to load
    await browse.waitForEntry('databases');
  });

  test('searching switches to search results in the left panel', async ({ page }) => {
    await browse.search('PostgreSQL');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
  });

  test('selecting a search result opens the fact in the right panel', async ({ page }) => {
    await browse.search('PostgreSQL MVCC');
    const results = page.getByTestId('dir-entry');
    await results.first().waitFor({ timeout: 10_000 });

    // Click the first search result
    await results.first().click();

    // Right panel should show the selected fact
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
  });

  test('arrow keys change selection and update the right panel', async ({ page }) => {
    await browse.search('PostgreSQL');
    const results = page.getByTestId('dir-entry');
    await results.first().waitFor({ timeout: 10_000 });
    const count = await results.count();
    if (count < 2) return; // need at least 2 results

    // First result should be auto-selected and shown in right panel
    await expect(factPanel.title).toBeVisible();
    const firstTitle = await factPanel.getTitle();

    // Click the left panel area to move focus away from the filter input
    // This enables the tree view keyboard handler to receive arrow key events
    await page.getByTestId('left-panel').click({ position: { x: 10, y: 10 } });
    await page.waitForTimeout(300);
    // Press ArrowDown to select the second result
    await page.keyboard.press('ArrowDown');
    // Wait for the right panel to update
    await page.waitForTimeout(1000);
    const secondTitle = await factPanel.getTitle();
    expect(secondTitle).not.toBe(firstTitle);
  });

  test('clicking an entity tag in the right panel adds a filter chip', async ({ page }) => {
    // Navigate to a fact that has entities
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    await browse.clickEntry(facts[0].name);

    // Wait for fact to load in right panel
    await expect(factPanel.title).toBeVisible();

    // Find an entity tag and click it
    const entityTags = page.getByTestId('tag-item');
    await entityTags.first().waitFor({ timeout: 5_000 });
    const entityValue = await entityTags.first().getAttribute('data-value');
    expect(entityValue).toBeTruthy();
    await entityTags.first().click();

    // In the new UI, clicking a tag dispatches ADD_FILTER which adds a chip
    // Verify a chip with the entity/domain value appears in the filter bar
    const chip = page.locator('span').filter({ hasText: new RegExp(`(?:entity|domain):${entityValue!.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`) });
    await expect(chip.first()).toBeVisible({ timeout: 5_000 });
  });

  test('clicking a domain tag in the right panel adds a filter chip', async ({ page }) => {
    // Navigate to a fact -- we need one with domain tags
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    await browse.clickEntry(facts[0].name);
    await expect(factPanel.title).toBeVisible();

    const allTags = page.getByTestId('tag-item');
    const tagCount = await allTags.count();
    if (tagCount === 0) {
      // No tags available (seed data has no domain field) -- skip gracefully
      return;
    }

    // Click a tag and verify a chip is added
    const tagValue = await allTags.first().getAttribute('data-value');
    await allTags.first().click();
    const chip = page.locator('span').filter({ hasText: new RegExp(`(?:entity|domain):${tagValue!.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`) });
    await expect(chip.first()).toBeVisible({ timeout: 5_000 });
  });

  // Removed: "clicking the fact path enters history" -- the new UI does not have
  // a clickable path element in the right panel. Use the "3" key to enter history view.

  test('clearing search returns to browse with correct directory', async () => {
    // Navigate into a subdirectory first
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');

    // Search
    await browse.search('PostgreSQL');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);

    // Clear search
    await browse.clearSearch();

    // Should return to the same directory (databases), not root
    await browse.waitForEntry('postgresql');
    const entries = await browse.getDirectoryEntries();
    const names = entries.map(e => e.name);
    expect(names).toContain('postgresql');
  });
});
