import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';

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

  test('domain filter narrows results', async () => {
    await browse.search('domain:security');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
    // All results should be from the security domain
    for (const path of results) {
      expect(path).toContain('security');
    }
  });

  test('entity filter works', async () => {
    await browse.search('entity:postgres');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
  });

  test('combined text and filter search', async () => {
    await browse.search('domain:databases PostgreSQL');
    const results = await browse.getSearchResults();
    expect(results.length).toBeGreaterThan(0);
  });

  test('no results for nonsense query', async ({ page }) => {
    await browse.search('xyznonexistentquery12345');
    // Wait briefly for search to complete
    await page.waitForTimeout(2000);
    const results = await browse.getSearchResults();
    expect(results.length).toBe(0);
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
});
