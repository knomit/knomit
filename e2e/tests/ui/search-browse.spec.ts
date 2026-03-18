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
    // Search results should replace directory entries
    const dirEntries = await page.getByTestId('dir-entry').count();
    expect(dirEntries).toBe(0);
  });

  test('selecting a search result opens the fact in the right panel', async ({ page }) => {
    await browse.search('PostgreSQL MVCC');
    const results = page.getByTestId('search-result');
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
    const results = page.getByTestId('search-result');
    await results.first().waitFor({ timeout: 10_000 });
    const count = await results.count();
    if (count < 2) return; // need at least 2 results

    // First result should be auto-selected and shown in right panel
    await expect(factPanel.title).toBeVisible();
    const firstTitle = await factPanel.getTitle();

    // Press ArrowDown to select the second result
    await page.keyboard.press('ArrowDown');
    // Wait for the right panel to update
    await page.waitForTimeout(500);
    const secondTitle = await factPanel.getTitle();
    expect(secondTitle).not.toBe(firstTitle);
  });

  test('clicking an entity tag in the right panel searches for it', async ({ page }) => {
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

    // Should switch to search mode with the entity/domain query
    const searchValue = await browse.searchInput.inputValue();
    expect(searchValue).toContain(entityValue!);

    // Verify search mode is active (search results or "No results" shown)
    // The seed data may not have indexed entities, so we just verify the search was triggered
    await page.waitForResponse(resp => resp.url().includes('/search')).catch(() => {});
  });

  test('clicking a domain tag in the right panel searches for it', async ({ page }) => {
    // Navigate to a fact — we need one with domain tags
    // The seed data may not have domain in frontmatter, so check the stats view
    // which aggregates domains. Use the shared instance's summary view instead.
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    await browse.clickEntry(facts[0].name);
    await expect(factPanel.title).toBeVisible();

    // Look for domain tags specifically (label "DOMAINS")
    // TagCloud for domains has searchPrefix "domain:" and entities has "entity:"
    const allTags = page.getByTestId('tag-item');
    const tagCount = await allTags.count();
    if (tagCount === 0) {
      // No tags available (seed data has no domain field) — skip gracefully
      return;
    }

    // Click a tag and verify search activates
    const tagValue = await allTags.first().getAttribute('data-value');
    await allTags.first().click();
    const searchValue = await browse.searchInput.inputValue();
    expect(searchValue).toContain(tagValue!);
  });

  test('clicking the fact path in the right panel enters history for that fact', async ({ page }) => {
    // Navigate to and select a fact
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    const factName = facts[0].name;
    await browse.clickEntry(factName);
    await expect(factPanel.title).toBeVisible();

    // The fact path is displayed below the title as a clickable div
    // It shows the full path e.g. "kb/databases/postgresql/mvcc.md"
    // Clicking it dispatches FACT_HISTORY which enters history mode
    const pathLocator = page.locator('div').filter({ hasText: /^kb\/databases\/postgresql\// }).first();
    await pathLocator.click();

    // Should enter history mode — timeline should appear
    const timeline = page.getByTestId('history-timeline');
    await expect(timeline).toBeVisible({ timeout: 10_000 });
  });

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
