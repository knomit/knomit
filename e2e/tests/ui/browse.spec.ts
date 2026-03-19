import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';

test.describe('Browse', () => {
  let browse: BrowsePage;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    browse = new BrowsePage(page);
    await page.waitForLoadState('domcontentloaded');
  });

  test('shows root directory entries', async () => {
    // Verify seeded directories appear
    const entries = await browse.getDirectoryEntries();
    expect(entries.length).toBeGreaterThan(0);
    const names = entries.map(e => e.name);
    expect(names).toContain('databases');
    expect(names).toContain('networking');
    expect(names).toContain('security');
    expect(names).toContain('observability');
    // All should be directories
    const dirs = entries.filter(e => e.isDir);
    expect(dirs.length).toBe(entries.length);
  });

  test('navigating into a directory shows children', async () => {
    await browse.clickEntry('databases');
    // Wait for a known child to appear after navigation
    await browse.waitForEntry('postgresql');
    const entries = await browse.getDirectoryEntries();
    const names = entries.map(e => e.name);
    // Should have subdirectories from seed data
    expect(names.length).toBeGreaterThan(0);
  });

  test('navigating updates breadcrumbs', async () => {
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    const crumbs = await browse.getBreadcrumbs();
    expect(crumbs.join('/')).toContain('databases');
  });

  test('clicking breadcrumb navigates back', async () => {
    await browse.clickEntry('databases');
    // Wait for databases children to load
    await browse.waitForEntry('postgresql');
    await browse.clickBreadcrumb('kb');
    // Wait for the root entries to reappear
    await browse.waitForEntry('databases');
    const entries = await browse.getDirectoryEntries();
    const names = entries.map(e => e.name);
    expect(names).toContain('databases');
    expect(names).toContain('networking');
  });

  test('clicking a fact selects it in the right panel', async ({ page }) => {
    // Navigate to a leaf directory that contains facts (kb/databases/postgresql/)
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    // Wait for a known fact file to appear (mvcc.md is seeded here)
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    expect(facts.length).toBeGreaterThan(0);
    await browse.clickEntry(facts[0].name);
    // Verify right panel shows the fact
    const title = page.getByTestId('fact-title');
    await expect(title).toBeVisible();
  });
});
