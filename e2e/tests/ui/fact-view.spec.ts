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
    await page.waitForLoadState('domcontentloaded');
  });

  test('displays fact title and body', async () => {
    // Navigate to a known fact with valid type (observation)
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    expect(facts.length).toBeGreaterThan(0);
    // Pick mvcc.md which has type: observation (valid)
    const mvcc = facts.find(f => f.name === 'mvcc.md');
    await browse.clickEntry(mvcc ? mvcc.name : facts[0].name);

    // Verify title and body display
    await expect(factPanel.title).toBeVisible();
    const title = await factPanel.getTitle();
    expect(title.length).toBeGreaterThan(0);
    const body = await factPanel.getBody();
    expect(body.length).toBeGreaterThan(0);
  });

  test('displays fact metadata', async () => {
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    // Pick mvcc.md which has type: observation (valid)
    const mvcc = facts.find(f => f.name === 'mvcc.md');
    await browse.clickEntry(mvcc ? mvcc.name : facts[0].name);

    // Check metadata section is visible and has content
    await expect(factPanel.meta).toBeVisible();
  });

  test('renders markdown content', async () => {
    // Navigate to a fact with type: observation which parses correctly.
    // Seed facts with type: practice/claim show parse errors and display raw frontmatter.
    // Use databases/postgresql/mvcc.md (type: observation)
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    await browse.clickEntry('mvcc.md');

    await expect(factPanel.title).toBeVisible();
    const body = await factPanel.getBody();
    // Should contain actual content, not raw frontmatter
    expect(body).not.toContain('---');
    expect(body.length).toBeGreaterThan(10);
  });

  test('displays fact type badge', async () => {
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    const entries = await browse.getDirectoryEntries();
    const facts = entries.filter(e => !e.isDir);
    const mvcc = facts.find(f => f.name === 'mvcc.md');
    await browse.clickEntry(mvcc ? mvcc.name : facts[0].name);

    // Verify type badge is visible
    await expect(factPanel.typeBadge).toBeVisible({ timeout: 5_000 });
    const badgeText = await factPanel.typeBadge.textContent();
    expect(badgeText).toBeTruthy();
    // Should contain one of the known type labels
    expect(badgeText).toMatch(/observation|concept|process|principle|pattern|reference|synthesis|hypothesis|methodology/);
  });

  test('switching facts updates the panel', async () => {
    // Navigate to a directory with observation-type facts
    await browse.clickEntry('databases');
    await browse.waitForEntry('postgresql');
    await browse.clickEntry('postgresql');
    await browse.waitForFactEntry();
    // Click mvcc.md (type: observation, valid)
    await browse.clickEntry('mvcc.md');
    await expect(factPanel.title).toBeVisible();
    const firstTitle = await factPanel.getTitle();

    // Go back to root and pick a different fact
    await browse.clickBreadcrumb('kb');
    await browse.waitForEntry('networking');
    await browse.clickEntry('networking');
    await browse.waitForEntry('dns');
    await browse.clickEntry('dns');
    await browse.waitForFactEntry();
    const netEntries = await browse.getDirectoryEntries();
    const netFacts = netEntries.filter(e => !e.isDir);
    if (netFacts.length > 0) {
      await browse.clickEntry(netFacts[0].name);
      // Wait for the right panel title to change from the previous fact
      await expect(factPanel.title).not.toHaveText(firstTitle, { timeout: 10_000 });
      const secondTitle = await factPanel.getTitle();
      expect(secondTitle).not.toBe(firstTitle);
    }
  });
});
