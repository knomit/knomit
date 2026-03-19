import { type Page, type Locator } from '@playwright/test';

export class BrowsePage {
  readonly page: Page;
  readonly searchInput: Locator;
  readonly leftPanel: Locator;

  constructor(page: Page) {
    this.page = page;
    this.searchInput = page.getByTestId('search-input');
    this.leftPanel = page.getByTestId('left-panel');
  }

  async goto() {
    await this.page.goto('/');
    await this.page.waitForLoadState('domcontentloaded');
  }

  async getBreadcrumbs(): Promise<string[]> {
    const segments = this.page.getByTestId('breadcrumb-segment');
    return segments.allTextContents();
  }

  async clickBreadcrumb(name: string) {
    await this.page.getByTestId('breadcrumb-segment').filter({ hasText: name }).click();
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

  async clickEntry(name: string) {
    const entry = this.page.getByTestId('dir-entry').and(this.page.locator(`[data-name="${name}"]`));
    await entry.waitFor({ timeout: 10_000 });
    await entry.click();
  }

  async search(query: string) {
    await this.searchInput.fill(query);
    await this.page.waitForResponse(resp => resp.url().includes('/search'));
  }

  async getSearchResults(): Promise<string[]> {
    const results = this.page.getByTestId('search-result');
    await results.first().waitFor({ timeout: 10_000 }).catch(() => {});
    const count = await results.count();
    const paths: string[] = [];
    for (let i = 0; i < count; i++) {
      paths.push((await results.nth(i).getAttribute('data-path')) || '');
    }
    return paths;
  }

  async clearSearch() {
    await this.searchInput.clear();
  }
}
