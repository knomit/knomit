import { type Page, type Locator, expect } from '@playwright/test';

export class BrowsePage {
  readonly page: Page;
  readonly filterInput: Locator;
  readonly leftPanel: Locator;

  constructor(page: Page) {
    this.page = page;
    this.filterInput = page.locator('#filter-input');
    this.leftPanel = page.getByTestId('left-panel');
  }

  /** @deprecated Use filterInput instead */
  get searchInput(): Locator {
    return this.filterInput;
  }

  async goto() {
    await this.page.goto('/');
    await this.page.waitForLoadState('domcontentloaded');
  }

  async getDirectoryEntries(): Promise<Array<{ name: string; isDir: boolean }>> {
    // Wait for at least one entry to appear (browse API is async)
    await this.page.getByTestId('dir-entry').first().waitFor({ timeout: 10_000 }).catch(() => {});
    const entries = this.page.getByTestId('dir-entry');
    const count = await entries.count();
    const result: Array<{ name: string; isDir: boolean }> = [];
    for (let i = 0; i < count; i++) {
      const entry = entries.nth(i);
      const name = (await entry.getAttribute('data-name')) || '';
      const isDir = (await entry.getAttribute('data-isdir')) === 'true';
      result.push({ name, isDir });
    }
    return result;
  }

  /**
   * Wait for a specific named entry to appear in the directory listing.
   * Useful after navigation to ensure the DOM has updated.
   */
  async waitForEntry(name: string, opts?: { timeout?: number }) {
    const timeout = opts?.timeout ?? 10_000;
    await this.page.getByTestId('dir-entry').and(this.page.locator(`[data-name="${name}"]`)).waitFor({ timeout });
  }

  /**
   * Wait for any non-directory entry (fact file) to appear.
   */
  async waitForFactEntry(opts?: { timeout?: number }) {
    const timeout = opts?.timeout ?? 10_000;
    await this.page.getByTestId('dir-entry').and(this.page.locator('[data-isdir="false"]')).first().waitFor({ timeout });
  }

  async clickEntry(name: string) {
    const entry = this.page.getByTestId('dir-entry').and(this.page.locator(`[data-name="${name}"]`));
    await entry.waitFor({ timeout: 10_000 });
    await entry.click();
  }

  /**
   * Type a query into the filter input and wait for it to trigger a search.
   * In the new UI, free text in the filter bar dispatches SET_FREE_TEXT after debounce,
   * which triggers the TreeView to call the search API.
   */
  async search(query: string) {
    await this.filterInput.fill(query);
    // The FilterBar dispatches SET_FREE_TEXT after 300ms debounce.
    // Wait for the search API call to complete.
    await this.page.waitForResponse(resp => resp.url().includes('/search') || resp.url().includes('/browse'));
  }

  /**
   * Get search/browse results displayed as dir-entry items.
   * In the new UI, search results are rendered as dir-entry items in TreeView.
   */
  async getSearchResults(): Promise<string[]> {
    const results = this.page.getByTestId('dir-entry');
    await results.first().waitFor({ timeout: 10_000 }).catch(() => {});
    const count = await results.count();
    const paths: string[] = [];
    for (let i = 0; i < count; i++) {
      paths.push((await results.nth(i).getAttribute('data-name')) || '');
    }
    return paths;
  }

  async clearSearch() {
    await this.filterInput.clear();
    // Wait for the debounce to fire and browse to reload
    await this.page.waitForTimeout(500);
  }

  /**
   * Navigate up by pressing the back button or switching path.
   * In the new UI there are no breadcrumbs; use the back button in the TopBar.
   */
  async navigateBack() {
    await this.page.locator('button[title="Back"]').click();
  }
}
