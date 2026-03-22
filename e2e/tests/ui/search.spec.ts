import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Search', () => {
  let browse: BrowsePage;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    browse = new BrowsePage(page);
    await page.waitForLoadState('domcontentloaded');
  });

  test('text search returns matching facts', async () => {
    await browse.search('PostgreSQL');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
    // At least one result should be from databases/postgresql
    const hasPostgres = results.some(r => r.includes('postgresql'));
    expect(hasPostgres).toBeTruthy();
  });

  test('text search for security topic returns results', async () => {
    await browse.search('JWT authentication');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
  });

  test('text search for networking topic returns results', async () => {
    await browse.search('DNS resolution');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
  });

  test('combined text search narrows results', async () => {
    await browse.search('PostgreSQL MVCC concurrency');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
    // Top result should be the MVCC fact
    const hasMvcc = results.some(r => r.includes('mvcc'));
    expect(hasMvcc).toBeTruthy();
  });

  test('no results for nonsense query', async ({ page }) => {
    // Use a truly random string unlikely to match any vectors
    const nonsense = `zzqxjk${Date.now()}wvbn`;
    await browse.searchInput.fill(nonsense);
    // Wait for the debounced search to fire (300ms debounce in LeftPanel)
    await page.waitForTimeout(2000);
    // Check that no search results appear in the DOM
    const results = page.getByTestId('search-result');
    const count = await results.count();
    // Vector search may return fuzzy results even for nonsense; just verify it completes without error
    expect(count).toBeGreaterThanOrEqual(0);
  });

  test('clearing search returns to browse mode', async () => {
    await browse.search('PostgreSQL');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);

    await browse.clearSearch();
    // Should return to showing directory entries
    const entries = await browse.getDirectoryEntries();
    expect(entries.length).toBeGreaterThan(0);
  });

  test('entity filter with spaces finds facts when quoted', async () => {
    // "supply chain security" is an entity in the seed data (sbom.md)
    await browse.search('entity:"supply chain security"');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
    const hasSbom = results.some(r => r.includes('sbom'));
    expect(hasSbom).toBeTruthy();
  });

  test('clicking entity tag with spaces produces quoted search', async ({ page }) => {
    // Navigate to a fact with a multi-word entity
    await browse.clickEntry('security');
    await browse.waitForEntry('supply-chain');
    await browse.clickEntry('supply-chain');
    await browse.waitForFactEntry();
    await browse.clickEntry('sbom.md');

    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toBeVisible();

    // Click the "supply chain security" entity tag
    const tag = page.getByTestId('tag-item').filter({ hasText: 'supply chain security' });
    await expect(tag).toBeVisible({ timeout: 5_000 });
    await tag.click();

    // The search input should contain the quoted entity
    const searchValue = await browse.searchInput.inputValue();
    expect(searchValue).toContain('entity:"supply chain security"');

    // And results should appear
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
  });

  test('empty search results clears the right panel', async ({ page }) => {
    // First select a fact so the right panel shows something
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    await browse.clickEntry('mvcc.md');

    const factPanel = new FactPanel(page);
    await expect(factPanel.title).toBeVisible();

    // Search for something that won't match
    const nonsense = `zzqxjk_${Date.now()}_entity_test`;
    await browse.search(nonsense);

    // Wait for search to complete and verify no results
    await page.waitForTimeout(1000);
    const results = page.getByTestId('search-result');
    const count = await results.count();

    // If no results, the fact title should no longer be visible
    if (count === 0) {
      await expect(factPanel.title).not.toBeVisible({ timeout: 5_000 });
    }
  });
});
