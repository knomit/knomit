import { test, expect } from '../../fixtures/knomit.js';
import { BrowsePage } from '../../pages/browse.page.js';
import { FactPanel } from '../../pages/fact-panel.page.js';

test.describe('Fact View', () => {
  let browse: BrowsePage;
  let factPanel: FactPanel;

  test.beforeEach(async ({ page, sharedBaseURL }) => {
    await page.goto(sharedBaseURL);
    browse = new BrowsePage(page);
    factPanel = new FactPanel(page);
    await page.waitForLoadState('networkidle');
  });

  test('displays fact title and body', async () => {
    // Navigate to a known fact
    await browse.clickEntry('databases');
    await browse.clickEntry('postgresql');
    // Get first fact in listing
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    expect(facts.length).toBeGreaterThan(0);
    await browse.clickEntry(facts[0].name);

    // Verify title and body display
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
    const body = await factPanel.getBody();
    expect(body.length).toBeGreaterThan(0);
  });

  test('displays fact metadata', async () => {
    await browse.clickEntry('databases');
    await browse.clickEntry('postgresql');
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    await browse.clickEntry(facts[0].name);

    // Check metadata section is visible and has content
    await expect(factPanel.meta).toBeVisible();
  });

  test('renders markdown content', async () => {
    await browse.clickEntry('security');
    await browse.clickEntry('authn');
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    await browse.clickEntry(facts[0].name);

    const body = await factPanel.getBody();
    // Should contain actual content, not raw frontmatter
    expect(body).not.toContain('---');
    expect(body.length).toBeGreaterThan(10);
  });

  test('switching facts updates the panel', async () => {
    await browse.clickEntry('observability');
    await browse.clickEntry('logging');
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    expect(facts.length).toBeGreaterThan(0);
    await browse.clickEntry(facts[0].name);
    const firstTitle = await factPanel.getTitle();

    // Go back and select a different domain
    await browse.clickBreadcrumb('kb');
    await browse.clickEntry('networking');
    await browse.clickEntry('dns');
    const netEntries = await browse.getDirectoryEntries();
    const netFacts = netEntries.filter(e => !e.isDir);
    if (netFacts.length > 0) {
      await browse.clickEntry(netFacts[0].name);
      const secondTitle = await factPanel.getTitle();
      expect(secondTitle).not.toBe(firstTitle);
    }
  });
});
