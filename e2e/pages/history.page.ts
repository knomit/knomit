import { type Page, type Locator } from '@playwright/test';

export class HistoryPage {
  readonly page: Page;
  readonly timeline: Locator;

  constructor(page: Page) {
    this.page = page;
    this.timeline = page.getByTestId('history-timeline');
  }

  async getCommits(): Promise<Array<{ hash: string }>> {
    const entries = this.page.getByTestId('history-commit');
    const count = await entries.count();
    const commits: Array<{ hash: string }> = [];
    for (let i = 0; i < count; i++) {
      const hash = (await entries.nth(i).getAttribute('data-hash')) || '';
      commits.push({ hash });
    }
    return commits;
  }

  async clickCommit(hash: string) {
    await this.page.locator(`[data-testid="history-commit"][data-hash="${hash}"]`).click();
  }
}
