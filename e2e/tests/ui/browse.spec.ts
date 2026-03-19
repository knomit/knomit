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
    const entries = await browse.getDirectoryEntries();
    const names = entries.map(e => e.name);
    // Should have subdirectories from seed data
    expect(names.length).toBeGreaterThan(0);
  });

  test('navigating updates breadcrumbs', async () => {
    await browse.clickEntry('databases');
    const crumbs = await browse.getBreadcrumbs();
    expect(crumbs.join('/')).toContain('databases');
  });

  test('clicking breadcrumb navigates back', async () => {
    await browse.clickEntry('databases');
    await browse.clickBreadcrumb('kb');
    const entries = await browse.getDirectoryEntries();
    const names = entries.map(e => e.name);
    expect(names).toContain('databases');
    expect(names).toContain('networking');
  });

  test('clicking a fact selects it in the right panel', async ({ page }) => {
    // Navigate to a leaf directory that contains facts (kb/databases/postgresql/)
    await browse.clickEntry('databases');
    await browse.clickEntry('postgresql');
    // Wait for a known fact file to appear (mvcc.md is seeded here)
    await page.getByTestId('dir-entry').and(page.locator('[data-isdir="false"]')).first().waitFor({ timeout: 10_000 });
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    expect(facts.length).toBeGreaterThan(0);
    await browse.clickEntry(facts[0].name);
    // Verify right panel shows the fact
    const title = page.getByTestId('fact-title');
    await expect(title).toBeVisible();
  });
});
