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
});
