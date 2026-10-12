import { type Page, type Locator } from '@playwright/test';

/** The per-fact version timeline (TimelineNav), not a repo-wide commit view. */
export class HistoryPage {
  readonly page: Page;
  readonly timeline: Locator;

  constructor(page: Page) {
    this.page = page;
    this.timeline = page.getByTestId('timeline-nav');
  }

  /** The 7-character hash shown on each version row, newest first. */
  async getCommits(): Promise<Array<{ hash: string }>> {
    const rows = this.page.getByTestId('timeline-row');
    const count = await rows.count();
    const commits: Array<{ hash: string }> = [];
    for (let i = 0; i < count; i++) {
      const text = (await rows.nth(i).textContent()) || '';
      const m = text.match(/[0-9a-f]{7}/);
      commits.push({ hash: m ? m[0] : '' });
    }
    return commits;
  }
}
